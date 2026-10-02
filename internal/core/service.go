// Package core implements Flats: flats, versions, deployments, exposure,
// previews and approvals. Every interface (HTTP API, MCP, CLI, console) goes
// through Service.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oesni/flats/internal/bundle"
	"github.com/oesni/flats/internal/site"
	"github.com/oesni/flats/internal/slug"
	"github.com/oesni/flats/internal/store"
)

// Via names the surface a request came from.
type Via string

const (
	ViaMCP     Via = "mcp"
	ViaCLI     Via = "cli"
	ViaAPI     Via = "api"
	ViaConsole Via = "console" // a human operator in the web console
	ViaSystem  Via = "system"
)

// Errors returned to callers.
var (
	ErrNotDeployed = errors.New("flat has no live version yet")
	ErrConflict    = errors.New("conflict")
)

// Runtime starts server flats (nil disables kind "server").
type Runtime interface {
	Start(ctx context.Context, spec RuntimeSpec) (Instance, error)
}

// RuntimeSpec describes one server flat instance.
type RuntimeSpec struct {
	Flat    string
	Version int
	Dir     string // version files (read-only)
	Entry   string
	DataDir string // per-flat data (SQLite, files); a copy for previews
	Env     map[string]string
	Log     func(level, msg string)
}

// Instance is a running server flat.
type Instance interface {
	http.Handler
	Stop()
}

// Config wires a Service.
type Config struct {
	DataDir    string
	Store      *store.Store
	Private    PrivateNet
	Public     PublicNet // nil disables public flats
	Runtime    Runtime   // nil disables server flats
	ConsoleURL func() string
	// Reserved are host names flats may not use (e.g. the console host).
	Reserved []string
	Now      func() time.Time
	Logf     func(format string, args ...any)
}

// Service is the Flats core.
type Service struct {
	cfg   Config
	st    *store.Store
	now   func() time.Time
	logf  func(string, ...any)
	locks sync.Map // slug -> *sync.Mutex

	mu        sync.Mutex
	live      map[string]*liveFlat // slug -> state
	prevs     map[string]*preview  // host -> preview
	redir     map[string]string    // old slug -> new slug (rename redirects being served)
	stop      chan struct{}
	wg        sync.WaitGroup
	secretKey []byte
	settings  sync.Map // setting key -> value cache
}

type liveFlat struct {
	cur           atomic.Pointer[deployed]
	privateServed bool
	publicServed  bool
	limiter       *limiter
	views         atomic.Int64
}

type deployed struct {
	version store.Version
	handler http.Handler
	inst    Instance
}

type preview struct {
	host    string
	flat    string
	version int
	handler http.Handler
	inst    Instance
	dataDir string
	last    atomic.Int64
}

// New creates a Service and restores the serving state from the database.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.ConsoleURL == nil {
		cfg.ConsoleURL = func() string { return "" }
	}
	s := &Service{cfg: cfg, st: cfg.Store, now: cfg.Now, logf: cfg.Logf,
		live: map[string]*liveFlat{}, prevs: map[string]*preview{}, redir: map[string]string{}, stop: make(chan struct{})}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "flats"), 0o700); err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		return nil, err
	}
	s.secretKey = key
	if err := s.restore(ctx); err != nil {
		return nil, err
	}
	s.wg.Add(1)
	go s.sweeper()
	return s, nil
}

// Close stops background work and all served hosts.
func (s *Service) Close() error {
	close(s.stop)
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for slug, lf := range s.live {
		s.flushViews(context.Background(), slug, lf)
		if d := lf.cur.Load(); d != nil && d.inst != nil {
			d.inst.Stop()
		}
	}
	for _, p := range s.prevs {
		if p.inst != nil {
			p.inst.Stop()
		}
	}
	return nil
}

func (s *Service) lock(slug string) func() {
	m, _ := s.locks.LoadOrStore(slug, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Service) flatDir(slug string) string { return filepath.Join(s.cfg.DataDir, "flats", slug) }
func (s *Service) versionDir(slug string, n int) string {
	return filepath.Join(s.flatDir(slug), "versions", strconv.Itoa(n))
}
func (s *Service) dataDirOf(slug string) string { return filepath.Join(s.flatDir(slug), "data") }

// Event records a log event for a flat (errors are logged, not returned).
func (s *Service) Event(ctx context.Context, slug, level, kind, msg string, data any) {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	if _, err := s.st.AddEvent(ctx, store.Event{Flat: slug, Time: s.now(), Level: level, Kind: kind, Message: msg, Data: raw}); err != nil {
		s.logf("event %s: %v", slug, err)
	}
}

// --- restore & serving ---

func (s *Service) restore(ctx context.Context) error {
	flats, err := s.st.ListFlats(ctx)
	if err != nil {
		return err
	}
	for _, f := range flats {
		lf := s.state(f.Slug)
		if f.LiveVersion > 0 {
			v, err := s.st.GetVersion(ctx, f.Slug, f.LiveVersion)
			if err != nil {
				s.logf("restore %s: %v", f.Slug, err)
				continue
			}
			d, err := s.build(ctx, f.Slug, v, s.dataDirOf(f.Slug))
			if err != nil {
				s.Event(ctx, f.Slug, "error", "restore", "could not start live version after restart: "+err.Error(), nil)
				continue
			}
			lf.cur.Store(d)
			if err := s.ensureExposure(ctx, f); err != nil {
				s.Event(ctx, f.Slug, "error", "exposure", err.Error(), nil)
			}
		}
		if f.OldSlug != "" && f.OldSlugTill != nil && f.OldSlugTill.After(s.now()) {
			s.serveRedirect(ctx, f.OldSlug, f.Slug)
		}
	}
	// Previews do not survive a restart: their nodes were ephemeral.
	prevs, err := s.st.ListPreviews(ctx, "")
	if err == nil {
		for _, p := range prevs {
			_ = s.st.DeletePreview(ctx, p.Host)
			os.RemoveAll(filepath.Join(s.flatDir(p.Flat), "previews", p.Host))
		}
	}
	return nil
}

func (s *Service) state(slug string) *liveFlat {
	s.mu.Lock()
	defer s.mu.Unlock()
	lf, ok := s.live[slug]
	if !ok {
		lf = &liveFlat{limiter: newLimiter(s.rateLimit())}
		s.live[slug] = lf
	}
	return lf
}

// build creates the handler for a version.
func (s *Service) build(ctx context.Context, slugName string, v store.Version, dataDir string) (*deployed, error) {
	if v.Pruned {
		return nil, fmt.Errorf("version %d was pruned by the retention policy", v.Number)
	}
	var m bundle.Manifest
	_ = json.Unmarshal(v.Manifest, &m)
	dir := s.versionDir(slugName, v.Number)
	switch v.Kind {
	case "server":
		if s.cfg.Runtime == nil {
			return nil, errors.New("server flats are not enabled on this host")
		}
		env, err := s.secretsFor(ctx, slugName)
		if err != nil {
			return nil, err
		}
		inst, err := s.cfg.Runtime.Start(ctx, RuntimeSpec{Flat: slugName, Version: v.Number, Dir: dir, Entry: m.Entry, DataDir: dataDir, Env: env,
			Log: func(level, msg string) {
				s.Event(context.Background(), slugName, level, "runtime", msg, map[string]int{"version": v.Number})
			}})
		if err != nil {
			return nil, err
		}
		return &deployed{version: v, handler: inst, inst: inst}, nil
	default:
		return &deployed{version: v, handler: &site.Static{Dir: dir, Entry: m.Entry, SPA: m.SPA, NotFound: m.NotFound, ModTime: v.CreatedAt}}, nil
	}
}

// siteHandler serves the live version of slug (used for both networks).
func (s *Service) siteHandler(slugName string, public bool) http.Handler {
	lf := s.state(slugName)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public {
			stripIdentity(r)
		}
		if !lf.limiter.allow() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		d := lf.cur.Load()
		if d == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "<!doctype html><title>%s</title><p>This flat has not been deployed yet.</p>", slugName)
			return
		}
		if isPageView(r) {
			lf.views.Add(1)
		}
		d.handler.ServeHTTP(w, r)
	})
}

// stripIdentity removes Tailscale identity headers a client may have sent.
func stripIdentity(r *http.Request) {
	for k := range r.Header {
		if len(k) >= 15 && http.CanonicalHeaderKey(k)[:15] == "Tailscale-User-" {
			r.Header.Del(k)
		}
	}
}

func isPageView(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.Header.Get("Sec-Fetch-Dest") == "document" {
		return true
	}
	ext := filepath.Ext(r.URL.Path)
	return ext == "" || ext == ".html"
}

func (s *Service) ensureExposure(ctx context.Context, f store.Flat) error {
	lf := s.state(f.Slug)
	if lf.cur.Load() == nil {
		return nil
	}
	if !lf.privateServed {
		if _, err := s.cfg.Private.Serve(ctx, f.Slug, s.siteHandler(f.Slug, false), false); err != nil {
			return fmt.Errorf("private exposure: %w", err)
		}
		lf.privateServed = true
	}
	switch {
	case f.Visibility.Public() && s.cfg.Public == nil:
		return errors.New("public exposure is disabled on this host (start `flats serve` with --portal=true)")
	case f.Visibility.Public() && !lf.publicServed:
		if _, err := s.cfg.Public.Serve(ctx, f.Slug, s.siteHandler(f.Slug, true), f.Visibility == store.PublicUnlisted); err != nil {
			return fmt.Errorf("public exposure: %w", err)
		}
		lf.publicServed = true
	case f.Visibility.Public() && lf.publicServed:
		if err := s.cfg.Public.SetHidden(f.Slug, f.Visibility == store.PublicUnlisted); err != nil {
			return fmt.Errorf("public listing: %w", err)
		}
	case !f.Visibility.Public() && lf.publicServed:
		if err := s.cfg.Public.Stop(f.Slug); err != nil {
			return fmt.Errorf("stop public exposure: %w", err)
		}
		lf.publicServed = false
	}
	return nil
}

// --- flats ---

// FlatView is a flat with derived fields for API consumers.
type FlatView struct {
	store.Flat
	PrivateURL   string         `json:"private_url"`
	PublicURL    string         `json:"public_url,omitempty"`
	PublicNotice string         `json:"public_notice,omitempty"`
	Live         *store.Version `json:"live,omitempty"`
	Versions     int            `json:"versions"`
	DiskBytes    int64          `json:"disk_bytes"`
	Thumbnail    string         `json:"thumbnail,omitempty"`
}

// UnlistedNotice is attached to every public-unlisted response.
const UnlistedNotice = "Unlisted only hides this flat from Portal relay listings. It is NOT access control: anyone with the URL can open it."

// ListedNotice is attached to every public-listed response.
const ListedNotice = "This flat is public: anyone can open it, and it appears in Portal relay listings."

func (s *Service) view(ctx context.Context, f store.Flat) FlatView {
	v := FlatView{Flat: f, PrivateURL: s.cfg.Private.URL(f.Slug)}
	if f.Visibility.Public() && s.cfg.Public != nil {
		v.PublicURL = s.cfg.Public.URL(f.Slug)
		if f.Visibility == store.PublicUnlisted {
			v.PublicNotice = UnlistedNotice
		} else {
			v.PublicNotice = ListedNotice
		}
	}
	if vs, err := s.st.ListVersions(ctx, f.Slug); err == nil {
		v.Versions = len(vs)
		for i := range vs {
			if vs[i].Number == f.LiveVersion {
				lv := vs[i]
				v.Live = &lv
			}
		}
		// Thumbnail from the newest version that ships a screenshot.
		for _, x := range vs {
			if x.Screenshot != "" && !x.Pruned {
				v.Thumbnail = fmt.Sprintf("/api/flats/%s/versions/%d/files/%s", f.Slug, x.Number, x.Screenshot)
				break
			}
		}
	}
	v.DiskBytes = dirSize(s.flatDir(f.Slug))
	return v
}

// ListFlats returns every flat.
func (s *Service) ListFlats(ctx context.Context) ([]FlatView, error) {
	fs, err := s.st.ListFlats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]FlatView, 0, len(fs))
	for _, f := range fs {
		out = append(out, s.view(ctx, f))
	}
	return out, nil
}

// GetFlat returns one flat.
func (s *Service) GetFlat(ctx context.Context, slugName string) (FlatView, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return FlatView{}, err
	}
	return s.view(ctx, f), nil
}

// CreateFlat creates an empty private flat.
func (s *Service) CreateFlat(ctx context.Context, slugName, name string, via Via) (FlatView, error) {
	if via == ViaConsole {
		return FlatView{}, errors.New("flats are created by agents (MCP, CLI or API), not from the console")
	}
	if err := slug.Validate(slugName); err != nil {
		return FlatView{}, err
	}
	if err := s.checkReserved(slugName); err != nil {
		return FlatView{}, err
	}
	if name == "" {
		name = slugName
	}
	now := s.now()
	f := store.Flat{Slug: slugName, Name: name, Visibility: store.Private, CreatedAt: now, UpdatedAt: now}
	if err := s.st.CreateFlat(ctx, f); err != nil {
		return FlatView{}, err
	}
	s.state(slugName)
	s.Event(ctx, slugName, "info", "flat", "flat created via "+string(via), nil)
	return s.view(ctx, f), nil
}

// RenameDisplay changes the display name.
func (s *Service) RenameDisplay(ctx context.Context, slugName, name string) (FlatView, error) {
	unlock := s.lock(slugName)
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return FlatView{}, err
	}
	f.Name, f.UpdatedAt = name, s.now()
	if err := s.st.UpdateFlat(ctx, f); err != nil {
		return FlatView{}, err
	}
	return s.view(ctx, f), nil
}

// --- versions ---

// SaveMeta describes where a version came from.
type SaveMeta struct {
	GitSHA   string
	GitDirty bool
	Message  string
}

// SaveVersion validates files and stores them as a new immutable version.
// The flat is created when it does not exist yet.
func (s *Service) SaveVersion(ctx context.Context, slugName string, files []bundle.File, meta SaveMeta, via Via) (store.Version, error) {
	if err := slug.Validate(slugName); err != nil {
		return store.Version{}, err
	}
	m, err := bundle.ParseManifest(files)
	if err != nil {
		return store.Version{}, err
	}
	if m.Kind == "server" && s.cfg.Runtime == nil {
		return store.Version{}, &bundle.ValidationError{Problems: []bundle.Problem{{Path: bundle.ManifestName, Message: "server flats are not enabled on this host", Fix: `deploy a static build ("kind": "static")`}}}
	}
	if _, err := s.st.GetFlat(ctx, slugName); errors.Is(err, store.ErrNotFound) {
		name := m.Name
		if _, err := s.CreateFlat(ctx, slugName, name, via); err != nil {
			return store.Version{}, err
		}
	} else if err != nil {
		return store.Version{}, err
	}
	unlock := s.lock(slugName)
	defer unlock()
	var size int64
	for _, f := range files {
		size += int64(len(f.Data))
	}
	if quota := s.diskQuota(); quota > 0 {
		if used := dirSize(s.flatDir(slugName)); used+size > quota {
			return store.Version{}, &bundle.ValidationError{Problems: []bundle.Problem{{Message: fmt.Sprintf("flat would use %d bytes, over its %d-byte disk quota", used+size, quota), Fix: "delete old versions/data, or raise the per-flat disk quota in system settings"}}}
		}
	}
	n, err := s.st.NextVersionNumber(ctx, slugName)
	if err != nil {
		return store.Version{}, err
	}
	dir := s.versionDir(slugName, n)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return store.Version{}, err
	}
	res, err := bundle.Write(files, dir)
	if err != nil {
		return store.Version{}, err
	}
	man, _ := json.Marshal(res.Manifest)
	v := store.Version{Flat: slugName, Number: n, Hash: res.Hash, Size: res.Size, Files: res.Files, Kind: res.Manifest.Kind,
		Manifest: man, GitSHA: meta.GitSHA, GitDirty: meta.GitDirty, Message: meta.Message, CreatedAt: s.now(), Screenshot: res.Manifest.Screenshot}
	if err := s.st.InsertVersion(ctx, v); err != nil {
		os.RemoveAll(dir)
		return store.Version{}, err
	}
	if res.Manifest.Name != "" {
		if f, err := s.st.GetFlat(ctx, slugName); err == nil && f.Name == slugName {
			f.Name, f.UpdatedAt = res.Manifest.Name, s.now()
			_ = s.st.UpdateFlat(ctx, f)
		}
	}
	_ = s.st.Touch(ctx, slugName, s.now())
	s.Event(ctx, slugName, "info", "version", fmt.Sprintf("saved version %d (%d files, %d bytes) via %s", n, res.Files, res.Size, via), map[string]any{"version": n, "hash": res.Hash, "git_sha": meta.GitSHA})
	s.pruneVersions(ctx, slugName)
	return v, nil
}

// ListVersions returns versions newest first.
func (s *Service) ListVersions(ctx context.Context, slugName string) ([]store.Version, error) {
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return nil, err
	}
	return s.st.ListVersions(ctx, slugName)
}

// GetVersion returns one version.
func (s *Service) GetVersion(ctx context.Context, slugName string, n int) (store.Version, error) {
	return s.st.GetVersion(ctx, slugName, n)
}

// VersionFile opens a file of a stored version (used for thumbnails).
func (s *Service) VersionFile(slugName string, n int, rel string) (string, error) {
	if err := slug.Validate(slugName); err != nil {
		return "", err
	}
	dir := s.versionDir(slugName, n)
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if rel == "" || !isWithin(dir, p) {
		return "", store.ErrNotFound
	}
	if _, err := os.Stat(p); err != nil {
		return "", store.ErrNotFound
	}
	return p, nil
}

func (s *Service) pruneVersions(ctx context.Context, slugName string) {
	keep := s.keepVersions()
	if keep <= 0 {
		return
	}
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return
	}
	vs, err := s.st.ListVersions(ctx, slugName)
	if err != nil {
		return
	}
	inPreview := map[int]bool{}
	if ps, err := s.st.ListPreviews(ctx, slugName); err == nil {
		for _, p := range ps {
			inPreview[p.Version] = true
		}
	}
	kept := 0
	for _, v := range vs { // newest first
		if v.Pruned {
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		if v.Number == f.LiveVersion || inPreview[v.Number] {
			continue
		}
		if err := os.RemoveAll(s.versionDir(slugName, v.Number)); err == nil {
			_ = s.st.MarkPruned(ctx, slugName, v.Number)
			s.Event(ctx, slugName, "info", "retention", fmt.Sprintf("pruned files of version %d (keeping the newest %d plus the live version)", v.Number, keep), nil)
		}
	}
}

// --- deploy ---

// HealthResult is the outcome of a pre-deploy health check.
type HealthResult struct {
	Path     string `json:"path"`
	Status   int    `json:"status"`
	OK       bool   `json:"ok"`
	Millis   int64  `json:"millis"`
	Error    string `json:"error,omitempty"`
	BodyHead string `json:"body_head,omitempty"`
}

// DeployResult is returned by Deploy and Rollback.
type DeployResult struct {
	Flat     FlatView     `json:"flat"`
	Version  int          `json:"version"`
	Previous int          `json:"previous"`
	Health   HealthResult `json:"health"`
	Millis   int64        `json:"millis"`
}

// DeployError carries the failed health check.
type DeployError struct {
	Version  int
	Previous int // live version when the deploy started (0 = none)
	Health   HealthResult
	Cause    error
}

func (e *DeployError) Error() string {
	tail := "(the previous live version keeps serving)"
	if e.Previous == 0 {
		tail = "(nothing was live before, so the flat is still not deployed)"
	}
	if e.Cause != nil {
		return fmt.Sprintf("deploy of version %d failed: %v %s", e.Version, e.Cause, tail)
	}
	return fmt.Sprintf("deploy of version %d failed its health check: GET %s returned %d %s %s", e.Version, e.Health.Path, e.Health.Status, e.Health.Error, tail)
}

func healthCheck(h http.Handler, p string) HealthResult {
	start := time.Now()
	res := HealthResult{Path: p}
	req := httptest.NewRequest(http.MethodGet, p, nil)
	req.Header.Set("User-Agent", "flats-healthcheck")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				res.Error = fmt.Sprint("handler panicked: ", r)
			}
			close(done)
		}()
		h.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		res.Error = "timed out after 15s"
		res.Millis = time.Since(start).Milliseconds()
		return res
	}
	res.Millis = time.Since(start).Milliseconds()
	if res.Error != "" {
		return res
	}
	res.Status = rec.Code
	body := rec.Body.String()
	if len(body) > 300 {
		body = body[:300]
	}
	res.BodyHead = body
	res.OK = rec.Code >= 200 && rec.Code < 400
	return res
}

// Deploy makes version n live after a successful health check.
func (s *Service) Deploy(ctx context.Context, slugName string, n int, via Via) (DeployResult, error) {
	return s.deploy(ctx, slugName, n, "deploy", via)
}

func (s *Service) deploy(ctx context.Context, slugName string, n int, kind string, via Via) (DeployResult, error) {
	start := time.Now()
	unlock := s.lock(slugName)
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return DeployResult{}, err
	}
	v, err := s.st.GetVersion(ctx, slugName, n)
	if err != nil {
		return DeployResult{}, err
	}
	d, err := s.build(ctx, slugName, v, s.dataDirOf(slugName))
	if err != nil {
		s.Event(ctx, slugName, "error", "deploy", fmt.Sprintf("%s of version %d failed to start: %v", kind, n, err), nil)
		return DeployResult{}, &DeployError{Version: n, Previous: f.LiveVersion, Cause: err}
	}
	var m bundle.Manifest
	_ = json.Unmarshal(v.Manifest, &m)
	h := healthCheck(d.handler, m.Health)
	s.Event(ctx, slugName, map[bool]string{true: "info", false: "error"}[h.OK], "health",
		fmt.Sprintf("health check of version %d: GET %s -> %d in %dms %s", n, h.Path, h.Status, h.Millis, h.Error), h)
	if !h.OK {
		if d.inst != nil {
			d.inst.Stop()
		}
		return DeployResult{}, &DeployError{Version: n, Previous: f.LiveVersion, Health: h}
	}
	prev := f.LiveVersion
	if snap, err := s.snapshotDB(slugName, n); err != nil {
		s.Event(ctx, slugName, "warn", "snapshot", "could not snapshot the database before deploy: "+err.Error(), nil)
	} else if snap != "" {
		s.Event(ctx, slugName, "info", "snapshot", fmt.Sprintf("saved database snapshot %s before deploying version %d", snap, n), nil)
	}
	if err := s.st.SetLive(ctx, slugName, n, prev, kind, s.now()); err != nil {
		if d.inst != nil {
			d.inst.Stop()
		}
		return DeployResult{}, err
	}
	lf := s.state(slugName)
	old := lf.cur.Swap(d)
	if old != nil && old.inst != nil {
		go func() { time.Sleep(5 * time.Second); old.inst.Stop() }() // drain in-flight requests
	}
	f.LiveVersion = n
	if err := s.ensureExposure(ctx, f); err != nil {
		s.Event(ctx, slugName, "error", "exposure", err.Error(), nil)
	}
	s.dropPreviews(ctx, slugName)
	s.Event(ctx, slugName, "info", "deploy", fmt.Sprintf("%s: version %d is live (was %d) via %s", kind, n, prev, via), map[string]int{"version": n, "previous": prev})
	s.pruneVersions(ctx, slugName)
	fv, _ := s.st.GetFlat(ctx, slugName)
	return DeployResult{Flat: s.view(ctx, fv), Version: n, Previous: prev, Health: h, Millis: time.Since(start).Milliseconds()}, nil
}

// Rollback redeploys an earlier version. to=0 picks the version that was
// live before the current one.
func (s *Service) Rollback(ctx context.Context, slugName string, to int, via Via) (DeployResult, error) {
	return s.RollbackWithData(ctx, slugName, to, false, via)
}

// RollbackWithData is Rollback that can also restore the database snapshot
// taken just before the current live version was deployed (server flats).
func (s *Service) RollbackWithData(ctx context.Context, slugName string, to int, restoreData bool, via Via) (DeployResult, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return DeployResult{}, err
	}
	if f.LiveVersion == 0 {
		return DeployResult{}, ErrNotDeployed
	}
	if to == 0 {
		hist, err := s.st.ListDeployments(ctx, slugName, 200)
		if err != nil {
			return DeployResult{}, err
		}
		for _, d := range hist {
			if d.Version == f.LiveVersion && d.Previous > 0 {
				to = d.Previous
				break
			}
		}
		if to == 0 {
			return DeployResult{}, fmt.Errorf("%w: there is no earlier deployed version to roll back to", ErrConflict)
		}
	}
	if to == f.LiveVersion {
		return DeployResult{}, fmt.Errorf("%w: version %d is already live", ErrConflict, to)
	}
	if !restoreData {
		return s.deploy(ctx, slugName, to, "rollback", via)
	}
	if err := s.restoreSnapshot(ctx, slugName, f.LiveVersion); err != nil {
		return DeployResult{}, err
	}
	res, err := s.deploy(ctx, slugName, to, "rollback", via)
	if err != nil {
		// The old instance was stopped for the restore: bring it back.
		if v, gerr := s.st.GetVersion(ctx, slugName, f.LiveVersion); gerr == nil {
			if d, berr := s.build(ctx, slugName, v, s.dataDirOf(slugName)); berr == nil {
				s.state(slugName).cur.Store(d)
			}
		}
	}
	return res, err
}

// Deployments returns the deploy history.
func (s *Service) Deployments(ctx context.Context, slugName string) ([]store.Deployment, error) {
	return s.st.ListDeployments(ctx, slugName, 100)
}

// --- visibility & approvals ---

// ActionResult is returned by actions that may need approval.
type ActionResult struct {
	Status      string          `json:"status"` // done | pending_approval
	Approval    *store.Approval `json:"approval,omitempty"`
	ApprovalURL string          `json:"approval_url,omitempty"`
	Flat        *FlatView       `json:"flat,omitempty"`
	Notice      string          `json:"notice,omitempty"`
	Message     string          `json:"message"`
}

func (s *Service) requestApproval(ctx context.Context, slugName, action string, params any, via Via, reason string) (ActionResult, error) {
	raw, _ := json.Marshal(params)
	a := store.Approval{ID: "apr-" + slug.Random(12), Flat: slugName, Action: action, Params: raw, Status: "pending", Via: string(via), Reason: reason, RequestedAt: s.now()}
	if err := s.st.InsertApproval(ctx, a); err != nil {
		return ActionResult{}, err
	}
	url := s.ApprovalURL(a.ID)
	s.Event(ctx, slugName, "warn", "approval", fmt.Sprintf("%s requested via %s; waiting for the operator: %s", action, via, url), params)
	return ActionResult{Status: "pending_approval", Approval: &a, ApprovalURL: url,
		Message: "This action needs the operator's approval. Share the approval link with them; nothing changes until they approve it in the web console."}, nil
}

// ApprovalURL is the console address where the operator decides approval id.
func (s *Service) ApprovalURL(id string) string {
	return strings.TrimSuffix(s.cfg.ConsoleURL(), "/") + "/approvals/" + id
}

// SetVisibility changes who can open a flat. Requests that widen exposure from
// anything other than the console wait for approval.
func (s *Service) SetVisibility(ctx context.Context, slugName string, vis store.Visibility, via Via, reason string) (ActionResult, error) {
	if !vis.Valid() {
		return ActionResult{}, fmt.Errorf("unknown visibility %q (use private, public-listed or public-unlisted)", vis)
	}
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	if vis.Public() && s.cfg.Public == nil {
		return ActionResult{}, errors.New("public exposure is disabled on this host (start `flats serve` with --portal=true)")
	}
	if f.Visibility == vis {
		fv := s.view(ctx, f)
		return ActionResult{Status: "done", Flat: &fv, Notice: fv.PublicNotice, Message: "visibility unchanged"}, nil
	}
	if via != ViaConsole && vis.Rank() > f.Visibility.Rank() {
		res, err := s.requestApproval(ctx, slugName, "set_visibility", map[string]string{"visibility": string(vis), "from": string(f.Visibility)}, via, reason)
		if vis == store.PublicUnlisted {
			res.Notice = UnlistedNotice
		} else {
			res.Notice = ListedNotice
		}
		return res, err
	}
	return s.applyVisibility(ctx, slugName, vis, via)
}

func (s *Service) applyVisibility(ctx context.Context, slugName string, vis store.Visibility, via Via) (ActionResult, error) {
	unlock := s.lock(slugName)
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	from := f.Visibility
	f.Visibility, f.UpdatedAt = vis, s.now()
	if err := s.ensureExposure(ctx, f); err != nil {
		return ActionResult{}, err
	}
	if err := s.st.UpdateFlat(ctx, f); err != nil {
		return ActionResult{}, err
	}
	s.Event(ctx, slugName, "info", "visibility", fmt.Sprintf("visibility %s -> %s via %s", from, vis, via), nil)
	fv := s.view(ctx, f)
	msg := "visibility changed to " + string(vis)
	if f.LiveVersion == 0 && vis.Public() {
		msg += "; the flat goes public when its first version is deployed"
	}
	return ActionResult{Status: "done", Flat: &fv, Notice: fv.PublicNotice, Message: msg}, nil
}

// Delete permanently deletes a flat. Requests from anything other than the
// console wait for approval.
func (s *Service) Delete(ctx context.Context, slugName string, via Via, reason string) (ActionResult, error) {
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return ActionResult{}, err
	}
	if via != ViaConsole {
		return s.requestApproval(ctx, slugName, "delete", map[string]string{"slug": slugName}, via, reason)
	}
	return s.applyDelete(ctx, slugName, via, "")
}

func (s *Service) applyDelete(ctx context.Context, slugName string, via Via, exceptApproval string) (ActionResult, error) {
	unlock := s.lock(slugName)
	defer unlock()
	s.dropPreviews(ctx, slugName)
	s.mu.Lock()
	lf := s.live[slugName]
	delete(s.live, slugName)
	s.mu.Unlock()
	if lf != nil {
		if lf.publicServed && s.cfg.Public != nil {
			_ = s.cfg.Public.Stop(slugName)
		}
		if lf.privateServed {
			_ = s.cfg.Private.Stop(slugName)
		}
		if d := lf.cur.Load(); d != nil && d.inst != nil {
			d.inst.Stop()
		}
	}
	if err := s.st.DeleteFlat(ctx, slugName); err != nil {
		return ActionResult{}, err
	}
	if pend, err := s.st.ListApprovals(ctx, "pending"); err == nil {
		for _, a := range pend {
			if a.Flat == slugName && a.ID != exceptApproval {
				_ = s.st.DecideApproval(ctx, a.ID, "failed", "the flat was deleted", s.now())
			}
		}
	}
	if err := os.RemoveAll(s.flatDir(slugName)); err != nil {
		return ActionResult{}, err
	}
	s.logf("flat %s deleted via %s", slugName, via)
	return ActionResult{Status: "done", Message: fmt.Sprintf("flat %s was permanently deleted", slugName)}, nil
}

// ListApprovals returns approvals by status ("" = all).
func (s *Service) ListApprovals(ctx context.Context, status string) ([]store.Approval, error) {
	return s.st.ListApprovals(ctx, status)
}

// GetApproval returns one approval.
func (s *Service) GetApproval(ctx context.Context, id string) (store.Approval, error) {
	return s.st.GetApproval(ctx, id)
}

// Decide approves or rejects a pending approval. Only the console calls it.
func (s *Service) Decide(ctx context.Context, id string, approve bool) (store.Approval, error) {
	a, err := s.st.GetApproval(ctx, id)
	if err != nil {
		return a, err
	}
	if a.Status != "pending" {
		return a, fmt.Errorf("%w: approval is already %s", ErrConflict, a.Status)
	}
	if !approve {
		if err := s.st.DecideApproval(ctx, id, "rejected", "rejected by the operator", s.now()); err != nil {
			return a, err
		}
		s.Event(ctx, a.Flat, "info", "approval", a.Action+" rejected by the operator", nil)
		return s.st.GetApproval(ctx, id)
	}
	var res ActionResult
	switch a.Action {
	case "set_visibility":
		var p struct{ Visibility string }
		_ = json.Unmarshal(a.Params, &p)
		res, err = s.applyVisibility(ctx, a.Flat, store.Visibility(p.Visibility), ViaConsole)
	case "delete":
		res, err = s.applyDelete(ctx, a.Flat, ViaConsole, a.ID)
	default:
		err = fmt.Errorf("unknown action %q", a.Action)
	}
	status, result := "approved", res.Message
	if err != nil {
		status, result = "failed", err.Error()
	}
	if derr := s.st.DecideApproval(ctx, id, status, result, s.now()); derr != nil {
		return a, derr
	}
	if a.Action != "delete" || err != nil {
		s.Event(ctx, a.Flat, "info", "approval", fmt.Sprintf("%s %s by the operator: %s", a.Action, status, result), nil)
	}
	out, gerr := s.st.GetApproval(ctx, id)
	if gerr != nil {
		return out, gerr
	}
	return out, err
}

// --- previews ---

// PreviewView describes an open preview.
type PreviewView struct {
	store.Preview
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// OpenPreview serves a saved version at a new random private address.
func (s *Service) OpenPreview(ctx context.Context, slugName string, n int) (PreviewView, error) {
	unlock := s.lock(slugName)
	defer unlock()
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return PreviewView{}, err
	}
	v, err := s.st.GetVersion(ctx, slugName, n)
	if err != nil {
		return PreviewView{}, err
	}
	host := slug.PreviewHost(slugName)
	dataDir := filepath.Join(s.flatDir(slugName), "previews", host)
	if v.Kind == "server" {
		if err := snapshotData(s.dataDirOf(slugName), dataDir); err != nil {
			return PreviewView{}, fmt.Errorf("copy live data for preview: %w", err)
		}
	}
	d, err := s.build(ctx, slugName, v, dataDir)
	if err != nil {
		os.RemoveAll(dataDir)
		return PreviewView{}, err
	}
	p := &preview{host: host, flat: slugName, version: n, handler: d.handler, inst: d.inst, dataDir: dataDir}
	now := s.now()
	p.last.Store(now.UnixMilli())
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.last.Store(s.now().UnixMilli())
		w.Header().Set("X-Robots-Tag", "noindex")
		p.handler.ServeHTTP(w, r)
	})
	url, err := s.cfg.Private.Serve(ctx, host, h, true)
	if err != nil {
		if d.inst != nil {
			d.inst.Stop()
		}
		os.RemoveAll(dataDir)
		return PreviewView{}, err
	}
	rec := store.Preview{Host: host, Flat: slugName, Version: n, CreatedAt: now, LastAccess: now}
	if err := s.st.InsertPreview(ctx, rec); err != nil {
		return PreviewView{}, err
	}
	s.mu.Lock()
	s.prevs[host] = p
	s.mu.Unlock()
	s.Event(ctx, slugName, "info", "preview", fmt.Sprintf("preview of version %d at %s", n, url), nil)
	return PreviewView{Preview: rec, URL: url, ExpiresAt: now.Add(s.previewTTL())}, nil
}

// ListPreviews returns open previews of a flat.
func (s *Service) ListPreviews(ctx context.Context, slugName string) ([]PreviewView, error) {
	ps, err := s.st.ListPreviews(ctx, slugName)
	if err != nil {
		return nil, err
	}
	out := make([]PreviewView, 0, len(ps))
	for _, p := range ps {
		s.mu.Lock()
		if lp := s.prevs[p.Host]; lp != nil {
			p.LastAccess = time.UnixMilli(lp.last.Load()).UTC()
		}
		s.mu.Unlock()
		out = append(out, PreviewView{Preview: p, URL: s.cfg.Private.URL(p.Host), ExpiresAt: p.LastAccess.Add(s.previewTTL())})
	}
	return out, nil
}

// ClosePreview removes one preview.
func (s *Service) ClosePreview(ctx context.Context, host string) error {
	s.mu.Lock()
	p := s.prevs[host]
	delete(s.prevs, host)
	s.mu.Unlock()
	_ = s.cfg.Private.Stop(host)
	if p != nil {
		if p.inst != nil {
			p.inst.Stop()
		}
		os.RemoveAll(p.dataDir)
	}
	return s.st.DeletePreview(ctx, host)
}

func (s *Service) dropPreviews(ctx context.Context, slugName string) {
	ps, err := s.st.ListPreviews(ctx, slugName)
	if err != nil {
		return
	}
	for _, p := range ps {
		_ = s.ClosePreview(ctx, p.Host)
		s.Event(ctx, slugName, "info", "preview", "closed preview "+p.Host, nil)
	}
}

// --- background ---

func (s *Service) sweeper() {
	defer s.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.Sweep(context.Background())
		}
	}
}

// Sweep expires previews and redirects and flushes page-view counters.
func (s *Service) Sweep(ctx context.Context) {
	ttl := s.previewTTL()
	now := s.now()
	s.mu.Lock()
	var expired []*preview
	for _, p := range s.prevs {
		if now.Sub(time.UnixMilli(p.last.Load())) > ttl {
			expired = append(expired, p)
		}
	}
	live := make(map[string]*liveFlat, len(s.live))
	for k, v := range s.live {
		live[k] = v
	}
	redirs := make(map[string]string, len(s.redir))
	for k, v := range s.redir {
		redirs[k] = v
	}
	s.mu.Unlock()
	for _, p := range expired {
		_ = s.st.TouchPreview(ctx, p.host, time.UnixMilli(p.last.Load()))
		_ = s.ClosePreview(ctx, p.host)
		s.Event(ctx, p.flat, "info", "preview", fmt.Sprintf("preview %s expired after %s without visits", p.host, ttl), nil)
	}
	for old, cur := range redirs {
		f, err := s.st.GetFlat(ctx, cur)
		if err != nil || f.OldSlug != old || f.OldSlugTill == nil || !f.OldSlugTill.After(now) {
			s.stopRedirect(old)
		}
	}
	for slugName, lf := range live {
		s.flushViews(ctx, slugName, lf)
	}
}

func (s *Service) flushViews(ctx context.Context, slugName string, lf *liveFlat) {
	if n := lf.views.Swap(0); n > 0 {
		if err := s.st.AddPageViews(ctx, slugName, s.now().Format("2006-01-02"), n); err != nil {
			lf.views.Add(n)
		}
	}
}

// PageViews returns daily request-count page views.
func (s *Service) PageViews(ctx context.Context, slugName string, days int) ([]store.DayCount, error) {
	s.mu.Lock()
	lf := s.live[slugName]
	s.mu.Unlock()
	if lf != nil {
		s.flushViews(ctx, slugName, lf)
	}
	return s.st.PageViews(ctx, slugName, days)
}

// --- logs ---

// Events returns log events of a flat.
func (s *Service) Events(ctx context.Context, slugName, kind string, after int64, limit int) ([]store.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return s.st.ListEvents(ctx, slugName, kind, after, limit)
}

// --- helpers ---

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

func isWithin(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !startsWithDotDot(rel)
}

func startsWithDotDot(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}
