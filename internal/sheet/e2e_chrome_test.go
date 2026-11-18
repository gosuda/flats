//go:build unix

package sheet

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/wsconn"
)

// The browser test drives a headless Chrome over the DevTools protocol, as
// the board's scripts do: Chrome runs with a throwaway profile in a process
// group of its own, and the test ends that group and every process of that
// profile when it finishes.

const (
	e2eWait    = 20 * time.Second
	e2ePoll    = 20 * time.Millisecond
	cdpTimeout = 30 * time.Second
)

func findChrome() string {
	for _, p := range []string{
		os.Getenv("CHROME"),
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	} {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

type cdpMessage struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
	SessionID string `json:"sessionId"`
}

type chrome struct {
	t       *testing.T
	cmd     *exec.Cmd
	exited  chan struct{}
	path    string
	profile string
	ws      *wsconn.Conn

	mu    sync.Mutex
	next  int64
	calls map[int64]chan cdpMessage
	pages map[string]*page
}

// launchChrome starts Chrome and connects to its DevTools endpoint. The
// cleanup ends the browser's process group and removes its profile.
func launchChrome(t *testing.T, path string) *chrome {
	t.Helper()
	profile, err := os.MkdirTemp("", "flats-sheet-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	b := &chrome{t: t, path: path, profile: profile, exited: make(chan struct{}), calls: map[int64]chan cdpMessage{}, pages: map[string]*page{}}
	b.cmd = exec.Command(path,
		"--user-data-dir="+profile, "--remote-debugging-port=0", "--headless=new", "--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--disable-component-update", "--disable-sync", "--disable-extensions",
		"--disable-default-apps", "--metrics-recording-only", "--password-store=basic", "--use-mock-keychain",
		"--no-proxy-server", "--force-color-profile=srgb", "--hide-scrollbars", "--mute-audio",
		"--disable-renderer-backgrounding", "--disable-background-timer-throttling", "--disable-backgrounding-occluded-windows",
		"--disable-features=Translate,MediaRouter,OptimizationHints", "--window-size=1280,800", "about:blank")
	b.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := b.cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.cmd.Start(); err != nil {
		os.RemoveAll(profile)
		t.Fatalf("start Chrome: %v", err)
	}
	t.Cleanup(b.stop)
	endpoint := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if _, rest, ok := strings.Cut(sc.Text(), "DevTools listening on "); ok {
				select {
				case endpoint <- strings.TrimSpace(rest):
				default:
				}
			}
		}
		_ = b.cmd.Wait()
		close(b.exited)
	}()
	var target string
	select {
	case target = <-endpoint:
	case <-b.exited:
		t.Fatal("Chrome exited before it reported a DevTools endpoint")
	case <-time.After(cdpTimeout):
		t.Fatal("Chrome did not report a DevTools endpoint")
	}
	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("DevTools endpoint %q: %v", target, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cdpTimeout)
	defer cancel()
	b.ws, err = wsconn.Dial(ctx, func(ctx context.Context) (net.Conn, error) { return new(net.Dialer).DialContext(ctx, "tcp", u.Host) }, u.Host, u.Path)
	if err != nil {
		t.Fatalf("connect to DevTools: %v", err)
	}
	go b.read()
	return b
}

func (b *chrome) read() {
	for {
		raw, err := b.ws.ReadMessage(256 << 20)
		if err != nil {
			b.mu.Lock()
			for id, ch := range b.calls {
				close(ch)
				delete(b.calls, id)
			}
			b.mu.Unlock()
			return
		}
		var m cdpMessage
		if json.Unmarshal([]byte(raw), &m) != nil {
			continue
		}
		b.mu.Lock()
		if m.ID != 0 {
			if ch := b.calls[m.ID]; ch != nil {
				delete(b.calls, m.ID)
				ch <- m
			}
			b.mu.Unlock()
			continue
		}
		p := b.pages[m.SessionID]
		b.mu.Unlock()
		if p != nil {
			p.event(m)
		}
	}
}

func (b *chrome) call(session, method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	b.next++
	id := b.next
	ch := make(chan cdpMessage, 1)
	b.calls[id] = ch
	b.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	data, _ := json.Marshal(msg)
	if err := b.ws.Send(string(data)); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: the DevTools connection closed", method)
		}
		if m.Error != nil {
			return nil, fmt.Errorf("%s: %s %s", method, m.Error.Message, m.Error.Data)
		}
		return m.Result, nil
	case <-time.After(cdpTimeout):
		return nil, fmt.Errorf("%s: no answer", method)
	}
}

func (b *chrome) must(session, method string, params any) json.RawMessage {
	b.t.Helper()
	out, err := b.call(session, method, params)
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}

// stop ends the browser: its process group, then any process that still
// has exactly this profile as its --user-data-dir argument.
func (b *chrome) stop() {
	if b.ws != nil {
		_, _ = b.call("", "Browser.close", nil)
		select {
		case <-b.exited:
		case <-time.After(5 * time.Second):
		}
		b.ws.Close(1000, "")
	}
	pid := b.cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	flag := "--user-data-dir=" + b.profile
	if out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			owned := false
			for _, arg := range fields[1:] {
				owned = owned || arg == flag
			}
			if p, err := strconv.Atoi(fields[0]); err == nil && owned && p != os.Getpid() {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	}
	select {
	case <-b.exited:
	case <-time.After(5 * time.Second):
	}
	_ = os.RemoveAll(b.profile)
}

// page is one tab of the browser, in a browser context of its own so that
// pages share no storage.
type page struct {
	b       *chrome
	name    string
	session string
	context string

	mu   sync.Mutex
	errs []string
}

type viewport struct {
	width, height int
	scale         float64
	mobile, dark  bool
}

func (b *chrome) open(name, address string, v viewport) *page {
	b.t.Helper()
	var ctx struct {
		ID string `json:"browserContextId"`
	}
	mustJSON(b.t, b.must("", "Target.createBrowserContext", nil), &ctx)
	var target struct {
		ID string `json:"targetId"`
	}
	mustJSON(b.t, b.must("", "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": ctx.ID}), &target)
	var attached struct {
		Session string `json:"sessionId"`
	}
	mustJSON(b.t, b.must("", "Target.attachToTarget", map[string]any{"targetId": target.ID, "flatten": true}), &attached)
	p := &page{b: b, name: name, session: attached.Session, context: ctx.ID}
	b.mu.Lock()
	b.pages[p.session] = p
	b.mu.Unlock()
	for _, method := range []string{"Runtime.enable", "Log.enable", "Page.enable"} {
		p.cdp(method, nil)
	}
	p.cdp("Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true})
	p.view(v)
	p.cdp("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": pageHelpers})
	p.cdp("Page.navigate", map[string]any{"url": address})
	return p
}

func (p *page) view(v viewport) {
	p.b.t.Helper()
	scale := v.scale
	if scale == 0 {
		scale = 1
	}
	p.cdp("Emulation.setDeviceMetricsOverride", map[string]any{"width": v.width, "height": v.height, "deviceScaleFactor": scale, "mobile": v.mobile})
	if v.mobile {
		p.cdp("Emulation.setTouchEmulationEnabled", map[string]any{"enabled": true})
	}
	scheme := "light"
	if v.dark {
		scheme = "dark"
	}
	p.cdp("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": scheme}}})
}

func mustJSON(t *testing.T, data json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("DevTools result %s: %v", data, err)
	}
}

func (p *page) cdp(method string, params any) json.RawMessage {
	p.b.t.Helper()
	out, err := p.b.call(p.session, method, params)
	if err != nil {
		p.b.t.Fatalf("%s: %v", p.name, err)
	}
	return out
}

// event keeps what the page reports as an error: console errors, uncaught
// exceptions and the browser's own entries, such as a blocked request.
func (p *page) event(m cdpMessage) {
	var text string
	switch m.Method {
	case "Runtime.consoleAPICalled":
		var ev struct {
			Type string `json:"type"`
			Args []struct {
				Value       json.RawMessage `json:"value"`
				Description string          `json:"description"`
			} `json:"args"`
		}
		if json.Unmarshal(m.Params, &ev) != nil || (ev.Type != "error" && ev.Type != "assert") {
			return
		}
		parts := []string{"console." + ev.Type}
		for _, a := range ev.Args {
			if a.Description != "" {
				parts = append(parts, a.Description)
			} else {
				parts = append(parts, string(a.Value))
			}
		}
		text = strings.Join(parts, " ")
	case "Runtime.exceptionThrown":
		var ev struct {
			Details struct {
				Text      string `json:"text"`
				URL       string `json:"url"`
				Line      int    `json:"lineNumber"`
				Exception struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		_ = json.Unmarshal(m.Params, &ev)
		text = fmt.Sprintf("exception: %s %s (%s:%d)", ev.Details.Text, ev.Details.Exception.Description, ev.Details.URL, ev.Details.Line)
	case "Log.entryAdded":
		var ev struct {
			Entry struct {
				Source string `json:"source"`
				Level  string `json:"level"`
				Text   string `json:"text"`
				URL    string `json:"url"`
			} `json:"entry"`
		}
		if json.Unmarshal(m.Params, &ev) != nil || ev.Entry.Level != "error" {
			return
		}
		text = fmt.Sprintf("log %s: %s %s", ev.Entry.Source, ev.Entry.Text, ev.Entry.URL)
	default:
		return
	}
	p.mu.Lock()
	p.errs = append(p.errs, text)
	p.mu.Unlock()
}

// errors returns what the page has reported since the last call.
func (p *page) errors() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.errs
	p.errs = nil
	return out
}

func (p *page) noErrors() {
	p.b.t.Helper()
	if errs := p.errors(); len(errs) > 0 {
		p.b.t.Errorf("%s reported errors:\n%s", p.name, strings.Join(errs, "\n"))
	}
}

// eval runs an expression in the page and decodes its value.
func (p *page) eval(expr string, into any) {
	p.b.t.Helper()
	out := p.cdp("Runtime.evaluate", map[string]any{"expression": expr, "awaitPromise": true, "returnByValue": true, "userGesture": true})
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	mustJSON(p.b.t, out, &res)
	if res.Exception != nil {
		p.b.t.Fatalf("%s: %s threw %s %s", p.name, expr, res.Exception.Text, res.Exception.Exception.Description)
	}
	if into == nil {
		return
	}
	if len(res.Result.Value) == 0 {
		res.Result.Value = json.RawMessage("null")
	}
	if err := json.Unmarshal(res.Result.Value, into); err != nil {
		p.b.t.Fatalf("%s: %s returned %s: %v", p.name, expr, res.Result.Value, err)
	}
}

func (p *page) str(expr string) string {
	p.b.t.Helper()
	var s string
	p.eval("String("+expr+")", &s)
	return s
}

func (p *page) truthy(expr string) bool {
	p.b.t.Helper()
	var ok bool
	p.eval("!!("+expr+")", &ok)
	return ok
}

// wait polls an expression until it is truthy. On a timeout it reports what
// the page shows.
func (p *page) wait(what, expr string) {
	p.b.t.Helper()
	deadline := time.Now().Add(e2eWait)
	for !p.truthy(expr) {
		if time.Now().After(deadline) {
			p.b.t.Fatalf("%s: timed out waiting for %s (%s)\n%s\nerrors: %v", p.name, what, expr, p.str("T.dump()"), p.errors())
		}
		time.Sleep(e2ePoll)
	}
}

// waitText waits until an expression has a value and reports the last one
// it had otherwise.
func (p *page) waitText(what, expr, want string) {
	p.b.t.Helper()
	deadline := time.Now().Add(e2eWait)
	for {
		got := p.str(expr)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			p.b.t.Fatalf("%s: %s is %q, want %q\n%s\nerrors: %v", p.name, what, got, want, p.str("T.dump()"), p.errors())
		}
		time.Sleep(e2ePoll)
	}
}

const (
	modAlt   = 1
	modCtrl  = 2
	modMeta  = 4
	modShift = 8
)

var e2eKeys = map[string]struct {
	code string
	vk   int
	text string
}{
	"Enter": {"Enter", 13, "\r"}, "Tab": {"Tab", 9, ""}, "Escape": {"Escape", 27, ""}, "F2": {"F2", 113, ""}, "F6": {"F6", 117, ""},
	"ArrowLeft": {"ArrowLeft", 37, ""}, "ArrowUp": {"ArrowUp", 38, ""}, "ArrowRight": {"ArrowRight", 39, ""}, "ArrowDown": {"ArrowDown", 40, ""},
	"Delete": {"Delete", 46, ""}, "Backspace": {"Backspace", 8, ""}, "Home": {"Home", 36, ""}, "End": {"End", 35, ""},
	"PageDown": {"PageDown", 34, ""}, "PageUp": {"PageUp", 33, ""},
}

// key presses and releases one key through the browser's input pipeline: a
// named key, or one character with modifiers.
func (p *page) key(name string, mods int) {
	p.b.t.Helper()
	k, named := e2eKeys[name]
	ev := map[string]any{"type": "rawKeyDown", "key": name, "modifiers": mods}
	switch {
	case named:
		ev["code"], ev["windowsVirtualKeyCode"] = k.code, k.vk
		if k.text != "" && mods&(modCtrl|modMeta) == 0 {
			ev["type"], ev["text"] = "keyDown", k.text
		}
	default:
		r := []rune(name)
		if len(r) != 1 {
			p.b.t.Fatalf("no key %q", name)
		}
		upper := strings.ToUpper(name)
		if upper[0] >= 'A' && upper[0] <= 'Z' {
			ev["code"], ev["windowsVirtualKeyCode"] = "Key"+upper, int(upper[0])
		} else if upper[0] >= '0' && upper[0] <= '9' {
			ev["code"], ev["windowsVirtualKeyCode"] = "Digit"+upper, int(upper[0])
		}
		if mods&(modCtrl|modMeta|modAlt) == 0 {
			ev["type"], ev["text"] = "keyDown", name
		}
	}
	p.cdp("Input.dispatchKeyEvent", ev)
	up := map[string]any{"type": "keyUp", "key": name, "modifiers": mods}
	for _, f := range []string{"code", "windowsVirtualKeyCode"} {
		if v, ok := ev[f]; ok {
			up[f] = v
		}
	}
	p.cdp("Input.dispatchKeyEvent", up)
}

// typeText types text one key at a time.
func (p *page) typeText(text string) {
	p.b.t.Helper()
	for _, r := range text {
		p.key(string(r), 0)
	}
}

type point struct{ X, Y float64 }

func (p *page) mouse(kind string, at point, button string, clicks int) {
	p.b.t.Helper()
	ev := map[string]any{"type": kind, "x": at.X, "y": at.Y, "button": button, "clickCount": clicks}
	if kind == "mouseMoved" && button == "left" {
		ev["buttons"] = 1
	}
	p.cdp("Input.dispatchMouseEvent", ev)
}

func (p *page) click(at point) {
	p.b.t.Helper()
	p.mouse("mouseMoved", at, "none", 0)
	p.mouse("mousePressed", at, "left", 1)
	p.mouse("mouseReleased", at, "left", 1)
}

func (p *page) doubleClick(at point) {
	p.b.t.Helper()
	p.click(at)
	p.mouse("mousePressed", at, "left", 2)
	p.mouse("mouseReleased", at, "left", 2)
}

func (p *page) rightClick(at point) {
	p.b.t.Helper()
	p.mouse("mouseMoved", at, "none", 0)
	p.mouse("mousePressed", at, "right", 1)
	p.mouse("mouseReleased", at, "right", 1)
}

// drag presses at from, moves in steps and releases at to.
func (p *page) drag(from, to point) {
	p.b.t.Helper()
	p.mouse("mouseMoved", from, "none", 0)
	p.mouse("mousePressed", from, "left", 1)
	const steps = 6
	for i := 1; i <= steps; i++ {
		f := float64(i) / steps
		p.mouse("mouseMoved", point{from.X + (to.X-from.X)*f, from.Y + (to.Y-from.Y)*f}, "left", 0)
	}
	p.mouse("mouseReleased", to, "left", 1)
}

// at returns the center of the element an expression names.
func (p *page) at(expr string) point {
	p.b.t.Helper()
	var pt *point
	p.eval("T.center("+expr+")", &pt)
	if pt == nil {
		p.b.t.Fatalf("%s: nothing is drawn for %s\n%s", p.name, expr, p.str("T.dump()"))
	}
	return *pt
}

// screenshot saves the viewport as a PNG file.
func (p *page) screenshot(dir, name string) {
	p.b.t.Helper()
	var shot struct {
		Data string `json:"data"`
	}
	mustJSON(p.b.t, p.cdp("Page.captureScreenshot", map[string]any{"format": "png"}), &shot)
	data, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		p.b.t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.b.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		p.b.t.Fatal(err)
	}
}

// pageHelpers is installed in every page before its own scripts run. It
// only reads the page: what a cell shows, where an element lies, what the
// notices say. Everything the test does goes through real input events.
const pageHelpers = `'use strict';
globalThis.T = {
  pos(addr) {
    const m = /^([A-Z]+)([0-9]+)$/.exec(addr);
    let c = 0;
    for (const ch of m[1]) c = c * 26 + ch.charCodeAt(0) - 64;
    return { r: Number(m[2]) - 1, c: c - 1 };
  },
  // cellEl is the element that draws a cell: the active cell has its own.
  cellEl(addr) {
    const { r, c } = T.pos(addr);
    const row = document.querySelector('.gridwrap .gr[aria-rowindex="' + (r + 2) + '"]:not(.ghead)');
    if (!row) return null;
    return [...row.querySelectorAll('.gc[aria-colindex="' + (c + 2) + '"]')].find((el) => !el.classList.contains('ph')) || null;
  },
  shown(addr) { const el = T.cellEl(addr); return el ? el.querySelector('.tx').textContent : '(not drawn)'; },
  classes(addr) { const el = T.cellEl(addr); return el ? el.className : ''; },
  rowHead(n) { const row = document.querySelector('.gridwrap .gr[aria-rowindex="' + (n + 1) + '"]:not(.ghead)'); return row && row.querySelector('.rh'); },
  colHead(letter) { return document.querySelector('.gridwrap .ghead .gh[aria-colindex="' + (T.pos(letter + '1').c + 2) + '"]'); },
  center(el) {
    if (typeof el === 'string') el = T.cellEl(el);
    if (!el) return null;
    const r = el.getBoundingClientRect();
    if (!r.width || !r.height) return null;
    return { X: r.left + r.width / 2, Y: r.top + r.height / 2 };
  },
  byText(sel, text) { return [...document.querySelectorAll(sel)].find((el) => el.textContent.trim() === text && el.getClientRects().length) || null; },
  byLabel(label) { return [...document.querySelectorAll('[aria-label]')].find((el) => el.getAttribute('aria-label').startsWith(label) && el.getClientRects().length) || null; },
  active() { const el = document.querySelector('.namebox'); return el ? el.textContent : ''; },
  bar() { const el = document.querySelector('.fbar .finput'); return el ? el.value : ''; },
  status() { const el = document.querySelector('.status-text'); return el ? el.textContent : ''; },
  notices() { return [...document.querySelectorAll('.toasts .toast')].map((el) => el.textContent).join(' | '); },
  tabs() { return [...document.querySelectorAll('.tabbar .tab')].map((el) => el.textContent).join(','); },
  dump() {
    const s = globalThis.sheet;
    const rows = [];
    for (let r = 1; r <= 9; r++) rows.push(['A', 'B', 'C', 'D', 'E', 'F'].map((c) => T.shown(c + r)).join(' | '));
    return JSON.stringify({
      active: T.active(), bar: T.bar(), status: T.status(), notices: T.notices(), tabs: T.tabs(),
      focus: document.activeElement && (document.activeElement.className || document.activeElement.tagName),
      net: s && { open: s.net.open, access: s.net.access, pending: s.net.pending.length, stopped: s.net.stopped, lastSeq: s.net.lastSeq, failures: s.net.failures },
      editor: s && s.editor.ed && { text: s.editor.text, waiting: s.editor.ed.waiting },
      popup: document.querySelector('.menu, .dialog, .pop:not([hidden])') && document.querySelector('.menu, .dialog, .pop:not([hidden])').textContent.slice(0, 300),
      rows,
    }, null, 1);
  },
};
`
