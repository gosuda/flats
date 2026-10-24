package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/provider"
)

// captureLog replaces the host's logf and returns what was logged.
func captureLog(h *Host) func() string {
	var mu sync.Mutex
	var sb strings.Builder
	h.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&sb, format+"\n", args...)
	}
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

// editHostConfig applies mutate to the host's config.json, as a text editor's
// save would leave it.
func editHostConfig(t *testing.T, h *Host, mutate func(*config.Document) error) {
	t.Helper()
	l, err := config.Load(h.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.NewWriter(h.ConfigPath).Save(l.Hash, mutate); err != nil {
		t.Fatal(err)
	}
}

// A file edit followed by a reload applies the live keys at once, advances
// the settings ETag so console saves work against it again, and logs the
// applied keys by name.
func TestReloadAppliesFileEdits(t *testing.T) {
	h, _ := consoleHost(t)
	ctx := context.Background()
	logs := captureLog(h)
	etag0 := h.Svc.SettingsETag()
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("system.rate_limit_rps", "9")
	})
	// Until the reload, the file change blocks a console save.
	if _, err := h.Svc.UpdateSettings(ctx, map[string]string{core.SetUploadMaxBytes: "4096"}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("save over a changed file: %v", err)
	}
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	all, err := h.Svc.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all[core.SetRateLimit] != "9" {
		t.Fatalf("rate limit %q", all[core.SetRateLimit])
	}
	if h.Svc.SettingsETag() == etag0 {
		t.Fatal("the ETag did not advance to the file's hash")
	}
	if !strings.Contains(logs(), "rate_limit_rps") {
		t.Fatalf("the reload did not log the applied key: %q", logs())
	}
	if _, err := h.Svc.UpdateSettings(ctx, map[string]string{core.SetUploadMaxBytes: "4096"}); err != nil {
		t.Fatalf("save after the reload: %v", err)
	}
	// A refused reload is logged and changes nothing. The writer validates,
	// so an invalid value is left by a raw text edit.
	raw, err := os.ReadFile(h.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `"rate_limit_rps": 9`, `"rate_limit_rps": 0`, 1)
	if edited == string(raw) {
		t.Fatal("rate_limit_rps not found in the file")
	}
	if err := os.WriteFile(h.ConfigPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.ReloadConfig(ctx); err == nil {
		t.Fatal("an out-of-range value was reloaded")
	}
	if all, _ := h.Svc.Settings(ctx); all[core.SetRateLimit] != "9" {
		t.Fatal("a refused reload changed the settings")
	}
	if !strings.Contains(logs(), "reload refused") {
		t.Fatalf("the refusal was not logged: %q", logs())
	}
}

// A reload does not change a provider a legacy service flag pins: the grant
// stays, the adopted file keeps the operator's edit, and the log names the
// key and the flag.
func TestReloadProviderPinnedByLegacyFlag(t *testing.T) {
	h, _ := consoleHostWith(t, t.TempDir(), func(o *Options) {
		o.Portal = true
		o.Set = map[string]bool{"portal": true}
	})
	logs := captureLog(h)
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("network.permitted", "")
	})
	if err := h.ReloadConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.Providers.HostAllows(provider.Portal) {
		t.Fatal("a pinned provider lost its grant")
	}
	if got := configPermitted(t, h); len(got) != 0 {
		t.Fatalf("the file was not adopted: %v", got)
	}
	if !strings.Contains(logs(), "--portal") {
		t.Fatalf("the pin was not logged: %q", logs())
	}
}

// A failed grant removal is logged and leaves the provider's state as it was;
// the next reload retries it. Unlike the console, a reload does not refuse to
// remove a provider a flat still allows.
func TestReloadProviderRemoval(t *testing.T) {
	h, client := consoleHost(t)
	ctx := context.Background()
	if code, _ := setHost(t, h, client, "portal", true); code != 200 {
		t.Fatal("turn on portal")
	}
	if _, err := h.Svc.CreateFlat(ctx, "site", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := h.Svc.SetProviderPermission(ctx, "site", "portal", true, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(h)
	h.testGrantFailure = func(id provider.ID, permitted bool) error { return errors.New("grant store down") }
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("network.permitted", "")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.Providers.HostAllows(provider.Portal) {
		t.Fatal("a failed grant removal changed the provider's state")
	}
	if !strings.Contains(logs(), "grant store down") {
		t.Fatalf("the failure was not logged: %q", logs())
	}
	h.testGrantFailure = nil
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if h.Providers.HostAllows(provider.Portal) {
		t.Fatal("the retry did not remove the grant")
	}
	// The flat keeps its permission, as after a restart.
	f, err := h.Svc.GetFlat(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.Providers, "portal") {
		t.Fatalf("the flat's permission was removed: %v", f.Providers)
	}
}

// A reload reconciles the console domain lists like a start: a newly listed
// name is served, and a removed name's node is retired.
func TestReloadConsoleDomains(t *testing.T) {
	h, tail, _ := tailnetHost(t)
	ctx := context.Background()
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("tailscale.console_hosts", "flats,ops")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !tail.isServed("flats") || !tail.isServed("ops") {
		t.Fatalf("after the reload: served %v %v", tail.isServed("flats"), tail.isServed("ops"))
	}
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("tailscale.console_hosts", "flats")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return slices.Contains(tail.stopped(), "ops") })
	if list := hosts(h); !slices.Equal(list, []string{"flats"}) {
		t.Fatalf("list %v", list)
	}
}

// Close waits for an in-flight reload: one blocked partway holds the reload
// gate, and Close does not return before it finishes.
func TestCloseWaitsForReload(t *testing.T) {
	h, _ := consoleHost(t)
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("system.rate_limit_rps", "9")
	})
	// The reload is blocked in its first log line, after the file was adopted
	// and before any reconcile — a point where Close would otherwise run its
	// whole teardown and return.
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.logf = func(format string, args ...any) {
		if strings.Contains(fmt.Sprintf(format, args...), "config.json reloaded") {
			once.Do(func() { close(started); <-release })
		}
	}
	// gate fires when Close reaches the reload gate, which stays parked until
	// the test allows it past.
	gate := make(chan struct{})
	allow := make(chan struct{})
	h.testReloadGate = func() {
		close(gate)
		<-allow
	}
	reloaded := make(chan error, 1)
	go func() { reloaded <- h.ReloadConfig(context.Background()) }()
	select {
	case <-started:
	case err := <-reloaded:
		t.Fatalf("reload returned before its first log line: %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()
	<-gate
	close(allow)
	// With the gate observed, a Close that did not wait for the reload would
	// run its whole teardown and return while the reload is still blocked.
	// The window only ever bounds the broken case: a Close parked on the
	// reload gate cannot return at all until release.
	select {
	case err := <-closed:
		t.Fatalf("Close returned during an in-flight reload: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// A reload that adds a provider to network.permitted records its host grant
// and starts its backend, as the console turning it on does; no flat's
// permission changes.
func TestReloadAddsProvider(t *testing.T) {
	h, _ := consoleHost(t)
	ctx := context.Background()
	if _, err := h.Svc.CreateFlat(ctx, "site", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	before, err := h.Svc.GetFlat(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	logs := captureLog(h)
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("network.permitted", "tailscale")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.Providers.HostAllows(provider.Tailscale) ||
		!slices.Contains(h.Providers.File().Permitted, provider.Tailscale) {
		t.Fatalf("grant not recorded: %+v", h.Providers.File())
	}
	h.pmu.Lock()
	attached := h.tsNet != nil
	h.pmu.Unlock()
	if !attached {
		t.Fatal("the tailscale backend was not started")
	}
	after, err := h.Svc.GetFlat(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before.Providers, after.Providers) {
		t.Fatalf("flat providers %v -> %v", before.Providers, after.Providers)
	}
	if !strings.Contains(logs(), "provider tailscale enabled=true by config reload") {
		t.Fatalf("the grant was not logged: %q", logs())
	}
}

// A reload reconciles the NetBird console domains like the Tailscale ones.
func TestReloadNetbirdConsoleDomains(t *testing.T) {
	h, _, nb := netbirdConsoleHost(t)
	ctx := context.Background()
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("netbird.console_hosts", "ops")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !nb.isServed("ops") {
		t.Fatal("the listed NetBird console domain was not served")
	}
	editHostConfig(t, h, func(d *config.Document) error {
		return d.Set("netbird.console_hosts", "")
	})
	if err := h.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		nb.mu.Lock()
		defer nb.mu.Unlock()
		return slices.Contains(nb.retired, "ops")
	})
	if nb.isServed("ops") || len(nbHosts(h)) != 0 {
		t.Fatalf("after the removal: served %v list %v", nb.isServed("ops"), nbHosts(h))
	}
}

// SIGHUPs that arrive while a reload runs coalesce into one further reload.
func TestReloadSignalsCoalesce(t *testing.T) {
	h, _ := consoleHost(t)
	started := make(chan struct{})
	release := make(chan struct{})
	second := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	count := 0
	h.logf = func(format string, args ...any) {
		if !strings.Contains(fmt.Sprintf(format, args...), "config.json reloaded") {
			return
		}
		once.Do(func() { close(started); <-release })
		mu.Lock()
		count++
		if count == 2 {
			close(second)
		}
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	hup := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		h.reloadOnSIGHUP(ctx, hup)
		close(done)
	}()
	hup <- syscall.SIGHUP
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first reload did not start")
	}
	// Three more requests while the first runs: the buffered channel holds one.
	for range 3 {
		select {
		case hup <- syscall.SIGHUP:
		default:
		}
	}
	close(release)
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("the coalesced reload did not run")
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if count != 2 {
		t.Fatalf("reloads = %d, want 2 (the running one plus one coalesced)", count)
	}
}
