package design

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/store"
)

//go:embed web
var webFS embed.FS

// Errors a Backend returns; the handler maps them to HTTP statuses.
var (
	ErrNotFound = errors.New("not found")
	ErrQuota    = errors.New("over quota")
	ErrLimited  = errors.New("too many changes; slow down")
)

// SnapFile is one file of a snapshot.
type SnapFile struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Gen  int64  `json:"gen,omitempty"`
}

// Snapshot is a coherent set of files. Read returns a file's bytes; the
// snapshot holds read leases until Release.
type Snapshot struct {
	Revision int
	Seq      int64
	// Pending reports edits of the head not yet kept as a revision (an
	// open group of a person's edits).
	Pending bool
	// Visibility is the flat's visibility: private or public.
	Visibility string
	Files      []SnapFile
	Read       func(path string) ([]byte, error)
	Release    func()
}

// File returns the snapshot entry of p.
func (s *Snapshot) File(p string) (SnapFile, bool) {
	for _, f := range s.Files {
		if f.Path == p {
			return f, true
		}
	}
	return SnapFile{}, false
}

// RevisionPage is one page of history.
type RevisionPage struct {
	Revisions     []store.Revision `json:"revisions"`
	Next          int              `json:"next,omitempty"`
	Pins          int              `json:"pins"`
	PinQuota      int              `json:"pin_quota"`
	RevisionQuota int              `json:"revision_quota"`
}

// Backend is the content store of one design, implemented by core.
type Backend interface {
	// Snapshot reads the head (rev 0) or revision rev.
	Snapshot(ctx context.Context, rev int) (*Snapshot, error)
	Apply(ctx context.Context, editor, label string, op Op) error
	Revisions(ctx context.Context, before, limit int) (RevisionPage, error)
	Revision(ctx context.Context, n int) (store.Revision, []store.RevisionFile, error)
	Pin(ctx context.Context, n int, pinned bool) error
	Rollback(ctx context.Context, n int, label string) error
}

// Instance serves one design flat's origin.
type Instance struct {
	slug    string
	b       Backend
	ctx     context.Context
	cancel  context.CancelFunc
	stopped atomic.Bool
	wg      sync.WaitGroup

	mu      sync.Mutex
	caps    map[string]capability
	streams map[*stream]struct{}
	active  map[*http.ResponseController]struct{} // long responses Stop interrupts
	limiter *rateLimiter
}

// capability grants reading one scope of files: the head (rev 0) or one
// revision. Artboards learn their own token, so each token is scoped to
// what that frame shows.
type capability struct {
	private bool
	rev     int
	expires time.Time
}

const (
	capTTL       = 12 * time.Hour
	maxCaps      = 256
	maxStreams   = 32
	streamQueue  = 16
	heartbeat    = 25 * time.Second
	stopDrainMax = 5 * time.Second
	writeTimeout = 15 * time.Second
)

// New returns the instance of slug.
func New(slug string, b Backend) *Instance {
	ctx, cancel := context.WithCancel(context.Background())
	return &Instance{slug: slug, b: b, ctx: ctx, cancel: cancel, caps: map[string]capability{}, streams: map[*stream]struct{}{},
		active: map[*http.ResponseController]struct{}{}, limiter: newRateLimiter(20)}
}

// Stop refuses new requests, cancels every in-flight request, interrupts
// blocked writes of event streams and exports, and waits for the handlers
// to return, so no request reads the design's files after Stop.
func (in *Instance) Stop() {
	in.mu.Lock()
	if in.stopped.Load() {
		in.mu.Unlock()
		return
	}
	// Close admission under the same lock ServeHTTP registers under, so no
	// request is admitted after the wait below starts.
	in.stopped.Store(true)
	in.cancel()
	for rc := range in.active {
		_ = rc.SetWriteDeadline(time.Now())
	}
	in.mu.Unlock()
	done := make(chan struct{})
	go func() { in.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(stopDrainMax):
	}
	in.mu.Lock()
	in.caps = map[string]capability{}
	in.mu.Unlock()
}

// Notify tells open canvases that the design changed.
func (in *Instance) Notify(seq int64, revision int, paths []string) {
	in.publish(event{name: "change", data: map[string]any{"seq": seq, "revision": revision, "paths": paths}})
}

func private(r *http.Request) bool { return r.Header.Get("X-Flats-Access") == "private" }

// ServeHTTP routes the design origin.
func (in *Instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	in.mu.Lock()
	if in.stopped.Load() {
		in.mu.Unlock()
		http.Error(w, "this design is not available right now", http.StatusServiceUnavailable)
		return
	}
	in.wg.Add(1)
	in.mu.Unlock()
	defer in.wg.Done()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(in.ctx, cancel)
	defer stop()
	r = r.WithContext(ctx)
	p := r.URL.Path
	switch {
	case p == "/":
		in.serveShell(w, r)
	case strings.HasPrefix(p, "/_design/app/"):
		in.serveApp(w, r, strings.TrimPrefix(p, "/_design/app/"))
	case p == "/_design/api/session":
		in.serveSession(w, r)
	case p == "/_design/api/state":
		in.serveState(w, r)
	case p == "/_design/api/events":
		in.serveEvents(w, r)
	case p == "/_design/api/node":
		in.serveNode(w, r)
	case p == "/_design/api/ops":
		in.serveOp(w, r)
	case p == "/_design/api/revisions" || strings.HasPrefix(p, "/_design/api/revisions/"):
		in.serveRevisions(w, r, strings.TrimPrefix(strings.TrimPrefix(p, "/_design/api/revisions"), "/"))
	case strings.HasPrefix(p, "/_design/c/"):
		in.serveCap(w, r, strings.TrimPrefix(p, "/_design/c/"))
	case p == "/_design/export.zip":
		in.serveExport(w, r)
	case strings.HasPrefix(p, "/_design/"):
		http.NotFound(w, r)
	default:
		in.serveFile(w, r, strings.TrimPrefix(p, "/"), 0, !private(r), false)
	}
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "error"
	switch {
	case errors.Is(err, ErrStale):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, ErrInvalidOp):
		status, code = http.StatusBadRequest, "invalid"
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, ErrQuota):
		status, code = http.StatusRequestEntityTooLarge, "quota"
	case errors.Is(err, ErrLimited):
		status, code = http.StatusTooManyRequests, "rate_limited"
	}
	writeJSON(w, status, map[string]string{"error": err.Error(), "code": code})
}

// sameOrigin implements the write protection's origin rule.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return false
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return origin == scheme+"://"+r.Host || origin == "https://"+r.Host || origin == "http://"+r.Host
}

// checkWrite enforces POST, JSON, the design header, private access and
// same origin.
func checkWrite(w http.ResponseWriter, r *http.Request, needPrivate bool) bool {
	switch {
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case needPrivate && !private(r):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this design is read-only here", "code": "forbidden"})
	case !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || r.Header.Get("X-Flats-Design") != "1" || !sameOrigin(r):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "request refused: not from this design's canvas", "code": "forbidden"})
	default:
		return true
	}
	return false
}

func editorLabel(r *http.Request) string {
	if l := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); l != "" && len(l) <= 200 {
		return l
	}
	return "Person"
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- shell & app ----

const shellCSP = "default-src 'self'; frame-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"

func (in *Instance) serveShell(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", shellCSP)
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-cache")
	fmt.Fprint(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Design</title>
<link rel="icon" href="/_design/app/icon.svg" type="image/svg+xml">
<link rel="stylesheet" href="/_design/app/app.css">
<script type="module" src="/_design/app/app.js"></script>
</head>
<body></body>
</html>
`)
}

func (in *Instance) serveApp(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(webFS, "web/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if strings.HasSuffix(name, ".js") {
		ct = "text/javascript; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	if name == "bridge.js" {
		// Loaded by artboards at an opaque origin.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	}
	_, _ = w.Write(b)
}

// ---- capabilities ----

func (in *Instance) serveSession(w http.ResponseWriter, r *http.Request) {
	if !checkWrite(w, r, false) {
		return
	}
	var body struct {
		Revision int `json:"revision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, fmt.Errorf("%w: %v", ErrInvalidOp, err))
		return
	}
	if body.Revision < 0 || (body.Revision > 0 && !private(r)) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "history is available on a private route only", "code": "forbidden"})
		return
	}
	tok := randomToken()
	now := time.Now()
	in.mu.Lock()
	for k, c := range in.caps {
		if now.After(c.expires) {
			delete(in.caps, k)
		}
	}
	for len(in.caps) >= maxCaps {
		var oldest string
		var at time.Time
		for k, c := range in.caps {
			if oldest == "" || c.expires.Before(at) {
				oldest, at = k, c.expires
			}
		}
		delete(in.caps, oldest)
	}
	in.caps[tok] = capability{private: private(r), rev: body.Revision, expires: now.Add(capTTL)}
	in.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"cap": tok, "expires_in": int(capTTL.Seconds())})
}

// capOf returns a live capability and extends its expiry: a token stays
// valid while its canvas keeps using it.
func (in *Instance) capOf(tok string) (capability, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	c, ok := in.caps[tok]
	now := time.Now()
	if !ok || now.After(c.expires) {
		return capability{}, false
	}
	c.expires = now.Add(capTTL)
	in.caps[tok] = c
	return c, true
}

// serveCap serves /_design/c/<cap>/<path> from the capability's scope.
func (in *Instance) serveCap(w http.ResponseWriter, r *http.Request, rest string) {
	tok, p, _ := strings.Cut(rest, "/")
	c, ok := in.capOf(tok)
	if !ok || (c.rev > 0 && !private(r)) {
		http.NotFound(w, r)
		return
	}
	in.serveFile(w, r, p, c.rev, true, true)
}

// ---- files ----

func (in *Instance) serveFile(w http.ResponseWriter, r *http.Request, p string, rev int, cors, inCanvas bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ValidPath(p) != nil {
		http.NotFound(w, r)
		return
	}
	snap, err := in.b.Snapshot(r.Context(), rev)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer snap.Release()
	f, ok := snap.File(p)
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := snap.Read(p)
	if err != nil {
		writeErr(w, err)
		return
	}
	etag := `"` + f.Hash[:min(len(f.Hash), 32)]
	if IsArtboardPath(p) {
		if inCanvas && rev == 0 {
			etag += "-c" + strconv.FormatInt(f.Gen, 10)
		} else {
			etag += "-s"
		}
	}
	etag += `"`
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if cors {
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	ct := mime.TypeByExtension(path.Ext(p))
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	if IsArtboardPath(p) {
		ct = "text/html; charset=utf-8"
		data = rewriteArtboard(data, inCanvas && rev == 0, f.Gen)
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// rewriteArtboard applies tweak CSS, and for the canvas also node ids and
// the bridge, which carries the file generation the node ids belong to. A
// source that does not scan is served as stored.
func rewriteArtboard(src []byte, canvas bool, gen int64) []byte {
	s, err := Scan(src)
	if err != nil {
		return src
	}
	insert := ""
	if s.HasTweaks {
		if m, err := ParseTweaks(src[s.Tweaks[0]:s.Tweaks[1]]); err == nil {
			if css := TweakCSS(m); css != "" {
				insert += `<style data-flats-host>` + css + `</style>`
			}
		}
	}
	if !canvas {
		if insert == "" {
			return src
		}
		ns := *s
		ns.Elements = nil
		return ns.Rewrite(insert)
	}
	insert += `<script src="/_design/app/bridge.js" data-flats-host data-flats-gen="` + strconv.FormatInt(gen, 10) + `"></script>`
	return s.Rewrite(insert)
}

// ---- state ----

// ArtboardInfo is what the canvas needs to know about one artboard.
type ArtboardInfo struct {
	Tweaks map[string]Tweak `json:"tweaks,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// State is the canvas app's view of the design.
type State struct {
	Slug       string                  `json:"slug"`
	Access     string                  `json:"access"`
	Seq        int64                   `json:"seq"`
	Revision   int                     `json:"revision"`
	Pending    bool                    `json:"pending"`
	Visibility string                  `json:"visibility,omitempty"`
	Canvas     *Canvas                 `json:"canvas"`
	Files      []SnapFile              `json:"files"`
	Artboards  map[string]ArtboardInfo `json:"artboards"`
}

// MarshalJSON keeps the canvas's unknown keys.
func (c *Canvas) MarshalJSON() ([]byte, error) {
	m := map[string]any{"title": c.Title, "boards": c.Boards, "order": c.Order, "notes": c.Notes}
	if c.Launch != nil {
		m["launch"] = c.Launch
	}
	if c.Order == nil {
		m["order"] = []string{}
	}
	for k, v := range c.extra {
		m[k] = v
	}
	return json.Marshal(m)
}

func (in *Instance) serveState(w http.ResponseWriter, r *http.Request) {
	snap, err := in.b.Snapshot(r.Context(), 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer snap.Release()
	st, err := BuildState(snap)
	if err != nil {
		writeErr(w, err)
		return
	}
	st.Slug = in.slug
	st.Access = "public"
	if private(r) {
		st.Access = "private"
	} else {
		for i := range st.Files {
			st.Files[i].Gen = 0
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// BuildState parses the canvas and the artboards' tweaks of a snapshot.
func BuildState(snap *Snapshot) (*State, error) {
	st := &State{Seq: snap.Seq, Revision: snap.Revision, Pending: snap.Pending, Visibility: snap.Visibility, Files: append([]SnapFile(nil), snap.Files...), Artboards: map[string]ArtboardInfo{}}
	raw, err := snap.Read(IndexPath)
	if err != nil {
		st.Canvas = &Canvas{Boards: map[string]Board{}, Notes: map[string]Note{}, extra: map[string]json.RawMessage{}}
	} else if c, err := ParseCanvas(raw); err == nil {
		c.Normalize(func(p string) bool { _, ok := snap.File(p); return ok })
		st.Canvas = c
	} else {
		return nil, err
	}
	for p := range st.Canvas.Boards {
		src, err := snap.Read(p)
		if err != nil {
			continue
		}
		info := ArtboardInfo{}
		if s, err := Scan(src); err != nil {
			info.Error = err.Error()
		} else if s.HasTweaks {
			if m, err := ParseTweaks(src[s.Tweaks[0]:s.Tweaks[1]]); err == nil {
				info.Tweaks = m
			} else {
				info.Error = err.Error()
			}
		}
		st.Artboards[p] = info
	}
	return st, nil
}

// ---- node & ops ----

func (in *Instance) serveNode(w http.ResponseWriter, r *http.Request) {
	if !private(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this design is read-only here", "code": "forbidden"})
		return
	}
	p := r.URL.Query().Get("path")
	node, err := strconv.Atoi(r.URL.Query().Get("node"))
	gen, gerr := strconv.ParseInt(r.URL.Query().Get("gen"), 10, 64)
	if err != nil || gerr != nil || ValidPath(p) != nil || !IsArtboardPath(p) {
		writeErr(w, fmt.Errorf("%w: path, node and gen are required", ErrInvalidOp))
		return
	}
	snap, err := in.b.Snapshot(r.Context(), 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer snap.Release()
	f, ok := snap.File(p)
	if !ok {
		writeErr(w, fmt.Errorf("%w: %s", ErrNotFound, p))
		return
	}
	if f.Gen != gen {
		writeErr(w, fmt.Errorf("%w: %s was replaced since it was shown", ErrStale, p))
		return
	}
	src, err := snap.Read(p)
	if err != nil {
		writeErr(w, err)
		return
	}
	n, err := NodeOf(src, node)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tag": n.Tag, "inner": n.Inner, "inner_hash": n.InnerHash, "text": n.Text, "gen": f.Gen})
}

func (in *Instance) serveOp(w http.ResponseWriter, r *http.Request) {
	if !checkWrite(w, r, true) {
		return
	}
	if !in.limiter.allow() {
		writeErr(w, ErrLimited)
		return
	}
	var body struct {
		Editor string `json:"editor"`
		Op     Op     `json:"op"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	if err := dec.Decode(&body); err != nil {
		writeErr(w, fmt.Errorf("%w: %v", ErrInvalidOp, err))
		return
	}
	editor := strings.TrimSpace(body.Editor)
	if editor == "" || len(editor) > 80 {
		writeErr(w, fmt.Errorf("%w: editor id is required", ErrInvalidOp))
		return
	}
	if err := in.b.Apply(r.Context(), editor, editorLabel(r), body.Op); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// ---- revisions ----

func (in *Instance) serveRevisions(w http.ResponseWriter, r *http.Request, rest string) {
	if !private(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "history is available on a private route only", "code": "forbidden"})
		return
	}
	if rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		before, _ := strconv.Atoi(r.URL.Query().Get("before"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 100 {
			limit = 50
		}
		page, err := in.b.Revisions(r.Context(), before, limit)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
		return
	}
	num, action, _ := strings.Cut(rest, "/")
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "":
		rv, files, err := in.b.Revision(r.Context(), n)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"revision": rv, "files": files})
	case "pin":
		if !checkWrite(w, r, true) {
			return
		}
		var body struct {
			Pinned bool `json:"pinned"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			writeErr(w, fmt.Errorf("%w: %v", ErrInvalidOp, err))
			return
		}
		if err := in.b.Pin(r.Context(), n, body.Pinned); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "pinned": body.Pinned})
	case "rollback":
		if !checkWrite(w, r, true) {
			return
		}
		if err := in.b.Rollback(r.Context(), n, editorLabel(r)); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
	default:
		http.NotFound(w, r)
	}
}

// ---- export ----

func (in *Instance) serveExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap, err := in.b.Snapshot(r.Context(), 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer snap.Release()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, in.slug))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	rc := http.NewResponseController(w)
	defer in.track(rc)()
	_ = WriteExport(r.Context(), &deadlineWriter{w: w, rc: rc, ctx: r.Context()}, snap)
}

// WriteExport writes the export transform of a snapshot as a zip. It stops
// when ctx is cancelled.
func WriteExport(ctx context.Context, out io.Writer, snap *Snapshot) error {
	zw := zip.NewWriter(out)
	for _, f := range snap.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ValidPath(f.Path) != nil {
			continue
		}
		data, err := snap.Read(f.Path)
		if err != nil {
			return err
		}
		if IsArtboardPath(f.Path) {
			data = rewriteArtboard(data, false, 0)
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: f.Path, Method: zip.Deflate, Modified: time.Now().UTC()})
		if err != nil {
			return err
		}
		if _, err := fw.Write(data); err != nil {
			return err
		}
	}
	return zw.Close()
}

// deadlineWriter bounds each write, so a slow client cannot hold a handler
// (and its read leases) past teardown.
type deadlineWriter struct {
	w   io.Writer
	rc  *http.ResponseController
	ctx context.Context
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if d.ctx != nil && d.ctx.Err() != nil {
		return 0, d.ctx.Err()
	}
	_ = d.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	return d.w.Write(p)
}

// track registers a long response so Stop can interrupt its writes.
func (in *Instance) track(rc *http.ResponseController) func() {
	in.mu.Lock()
	in.active[rc] = struct{}{}
	in.mu.Unlock()
	if in.ctx.Err() != nil {
		_ = rc.SetWriteDeadline(time.Now())
	}
	return func() {
		in.mu.Lock()
		delete(in.active, rc)
		in.mu.Unlock()
	}
}

// ---- events ----

type event struct {
	name string
	data any
}

type stream struct {
	ch chan event
}

func (in *Instance) publish(e event) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for s := range in.streams {
		select {
		case s.ch <- e:
		default:
			// Full: drop everything queued (without blocking: the
			// consumer may drain it concurrently) and ask for a resync.
		drain:
			for {
				select {
				case <-s.ch:
				default:
					break drain
				}
			}
			select {
			case s.ch <- event{name: "resync", data: map[string]any{}}:
			default:
			}
		}
	}
}

func (in *Instance) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok || r.Method != http.MethodGet {
		http.Error(w, "streaming unsupported", http.StatusBadRequest)
		return
	}
	in.mu.Lock()
	if len(in.streams) >= maxStreams {
		in.mu.Unlock()
		writeErr(w, ErrLimited)
		return
	}
	s := &stream{ch: make(chan event, streamQueue)}
	in.streams[s] = struct{}{}
	in.mu.Unlock()
	defer func() {
		in.mu.Lock()
		delete(in.streams, s)
		in.mu.Unlock()
	}()
	snap, err := in.b.Snapshot(r.Context(), 0)
	var seq int64
	if err == nil {
		seq = snap.Seq
		snap.Release()
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	defer in.track(rc)()
	write := func(name string, data any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		b, _ := json.Marshal(data)
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		flusher.Flush()
		return err == nil
	}
	if !write("hello", map[string]any{"seq": seq}) {
		return
	}
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-s.ch:
			if !write(e.name, e.data) {
				return
			}
		case <-t.C:
			_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---- rate limiting ----

type rateLimiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newRateLimiter(perSecond float64) *rateLimiter {
	return &rateLimiter{rate: perSecond, tokens: perSecond, last: time.Now()}
}

func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens = min(l.rate, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
