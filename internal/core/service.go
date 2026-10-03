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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/site"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
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
	// ErrInvalid marks bad input (HTTP 400).
	ErrInvalid = errors.New("invalid request")
	// ErrForbidden marks actions the caller's surface may not perform (HTTP 403).
	ErrForbidden = errors.New("forbidden")
	// ErrUnavailable marks features disabled on this host (HTTP 409).
	ErrUnavailable = errors.New("unavailable on this host")
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
	Lifecycle  LifecycleNet // optional provider manager; independent of legacy adapters
	Public     PublicNet    // nil disables public flats
	Runtime    Runtime      // nil disables server flats
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

	mu          sync.Mutex
	live        map[string]*liveFlat // slug -> state
	prevs       map[string]*preview  // host -> preview
	redir       map[string]*redirect // old slug -> redirect being served
	quotaWarned map[string]time.Time
	stop        chan struct{}
	wg          sync.WaitGroup
	secretKey   []byte
	settings    sync.Map // setting key -> value cache
	eventCount  sync.Map // slug -> *atomic.Int64, events since the last prune
	logLimits   sync.Map // slug -> *logLimit
}

// redirect is an old slug served as a redirect to the flat now called cur.
type redirect struct {
	cur    string
	public bool // also served on the public network
}

type liveFlat struct {
	cur           atomic.Pointer[deployed]
	privateServed bool
	publicServed  bool
	limiter       *limiter
	views         atomic.Int64
	trafficMu     sync.Mutex
	traffic       map[trafficKey]int64
}

type deployed struct {
	version store.Version
	handler http.Handler
	inst    Instance
	redact  func(string) string // removes secret values from the flat's output
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
		live: map[string]*liveFlat{}, prevs: map[string]*preview{}, redir: map[string]*redirect{}, quotaWarned: map[string]time.Time{}, stop: make(chan struct{})}
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

// pruneEvery is how many events a flat may add between two prunes.
const pruneEvery = 200

// Event records a log event for a flat (errors are logged, not returned).
// The flat's log is trimmed to the events_keep newest events as it grows.
func (s *Service) Event(ctx context.Context, slug, level, kind, msg string, data any) {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	if _, err := s.st.AddEvent(ctx, store.Event{Flat: slug, Time: s.now(), Level: level, Kind: kind, Message: msg, Data: raw}); err != nil {
		s.logf("event %s: %v", slug, err)
		return
	}
	c, _ := s.eventCount.LoadOrStore(slug, new(atomic.Int64))
	if c.(*atomic.Int64).Add(1) >= pruneEvery {
		s.pruneEvents(ctx, slug)
	}
}

func (s *Service) pruneEvents(ctx context.Context, slug string) {
	if c, ok := s.eventCount.Load(slug); ok {
		c.(*atomic.Int64).Store(0)
	}
	if _, err := s.st.PruneEvents(ctx, slug, int(s.intSetting(SetEventsKeep))); err != nil {
		s.logf("prune events of %s: %v", slug, err)
	}
}

// Runtime log lines a server flat may record per second (burst twice that);
// the rest are counted and reported as one event.
const runtimeLogRate = 10

type logLimit struct {
	lim     *limiter
	dropped atomic.Int64
}

// runtimeLog records a log line from a flat's code, rate limited per flat.
func (s *Service) runtimeLog(slug string, version int, level, msg string) {
	v, _ := s.logLimits.LoadOrStore(slug, &logLimit{lim: newLimiter(runtimeLogRate)})
	ll := v.(*logLimit)
	if !ll.lim.allow() {
		ll.dropped.Add(1)
		return
	}
	s.reportDroppedLogs(slug, ll)
	s.Event(context.Background(), slug, level, "runtime", msg, map[string]int{"version": version})
}

func (s *Service) reportDroppedLogs(slug string, ll *logLimit) {
	if n := ll.dropped.Swap(0); n > 0 {
		s.Event(context.Background(), slug, "warn", "runtime", fmt.Sprintf("%d log lines were dropped (more than %d lines per second)", n, runtimeLogRate), nil)
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
	}
	if rs, err := s.st.ActiveRedirects(ctx, s.now()); err == nil {
		for _, r := range rs {
			if _, err := s.st.GetFlat(ctx, r.Old); err == nil {
				continue // a flat uses that slug again; never shadow it
			}
			s.serveRedirect(ctx, r.Old, r.Flat)
		}
	}
	// Approvals that were being applied when the process stopped.
	if as, err := s.st.ListApprovals(ctx, "applying"); err == nil {
		for _, a := range as {
			_ = s.st.FinishApproval(ctx, a.ID, "failed", "interrupted by a restart; request it again", s.now())
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
			return nil, unavailablef("server flats are not enabled on this host")
		}
		env, err := s.secretsFor(ctx, slugName)
		if err != nil {
			return nil, err
		}
		redact := newRedactor(env)
		inst, err := s.cfg.Runtime.Start(ctx, RuntimeSpec{Flat: slugName, Version: v.Number, Dir: dir, Entry: m.Entry, DataDir: dataDir, Env: env,
			Log: func(level, msg string) {
				s.runtimeLog(slugName, v.Number, level, redact(msg))
			}})
		if err != nil {
			// Start errors can carry the worker's stderr.
			return nil, errors.New(redact(err.Error()))
		}
		return &deployed{version: v, handler: inst, inst: inst, redact: redact}, nil
	default:
		return &deployed{version: v, handler: &site.Static{Dir: dir, Entry: m.Entry, SPA: m.SPA, NotFound: m.NotFound, ModTime: v.CreatedAt},
			redact: func(s string) string { return s }}, nil
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
			lf.recordPath(s.now().UTC().Format("2006-01-02"), r.URL.Path)
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
	// Serve is idempotent: it swaps the handler of a running host and
	// restarts one that failed or was stopped, so never trust privateServed
	// to mean the host is up.
	if _, err := s.cfg.Private.Serve(ctx, f.Slug, s.siteHandler(f.Slug, false), false); err != nil {
		return fmt.Errorf("private exposure: %w", err)
	}
	lf.privateServed = true
	switch {
	case f.Visibility.Public() && s.cfg.Public == nil:
		return errPublicDisabled()
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

func errPublicDisabled() error {
	return unavailablef("public exposure is disabled on this host (start `flats serve` with --portal=true)")
}

// --- flats ---

// FlatView is a flat with derived fields for API consumers.
type FlatView struct {
	store.Flat
	PrivateURL string `json:"private_url"`
	// PrivateState is the private host's state once it is served: "ready"
	// when the URL answers, "starting" while the node joins the tailnet or
	// waits for its HTTPS certificate (PrivateDetail says which).
	PrivateState  string         `json:"private_state,omitempty"`
	PrivateDetail string         `json:"private_detail,omitempty"`
	PublicURL     string         `json:"public_url,omitempty"`
	PublicNotice  string         `json:"public_notice,omitempty"`
	Live          *store.Version `json:"live,omitempty"`
	Versions      int            `json:"versions"`
	DiskBytes     int64          `json:"disk_bytes"`
	Thumbnail     string         `json:"thumbnail,omitempty"`
}

// UnlistedNotice is attached to every public-unlisted response.
const UnlistedNotice = "Unlisted only hides this flat from Portal relay listings. It is NOT access control: anyone with the URL can open it."

// ListedNotice is attached to every public-listed response.
const ListedNotice = "This flat is public: anyone can open it, and it appears in Portal relay listings."

func (s *Service) view(ctx context.Context, f store.Flat) FlatView {
	v := FlatView{Flat: f, PrivateURL: s.cfg.Private.URL(f.Slug)}
	v.PrivateState, v.PrivateDetail = s.hostState(f.Slug)
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
		// Fall back to the live version's favicon (the console shows initials
		// when there is neither).
		if v.Thumbnail == "" && v.Live != nil {
			for _, name := range []string{"favicon.svg", "favicon.png", "apple-touch-icon.png", "favicon.ico"} {
				if _, err := os.Stat(filepath.Join(s.versionDir(f.Slug, f.LiveVersion), name)); err == nil {
					v.Thumbnail = fmt.Sprintf("/api/flats/%s/versions/%d/files/%s", f.Slug, f.LiveVersion, name)
					break
				}
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
		return FlatView{}, forbiddenf("flats are created by agents (MCP, CLI or API), not from the console")
	}
	if err := slug.Validate(slugName); err != nil {
		return FlatView{}, invalid(err)
	}
	if err := s.checkReserved(ctx, slugName, ""); err != nil {
		return FlatView{}, err
	}
	if name == "" {
		name = slugName
	}
	now := s.now()
	f := store.Flat{Slug: slugName, Name: name, Visibility: store.Private, CreatedAt: now, UpdatedAt: now}
	if err := s.st.CreateFlat(ctx, f); err != nil {
		if errors.Is(err, store.ErrExists) {
			return FlatView{}, withKind(ErrConflict, err)
		}
		return FlatView{}, err
	}
	s.state(slugName)
	s.Event(ctx, slugName, "info", "flat", "flat created via "+string(via), nil)
	return s.view(ctx, f), nil
}

// RenameDisplay changes the display name.
func (s *Service) RenameDisplay(ctx context.Context, slugName, name string) (FlatView, error) {
	if name = strings.TrimSpace(name); name == "" {
		return FlatView{}, invalidf("the display name must not be empty")
	}
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
		return store.Version{}, invalid(err)
	}
	m, err := bundle.ParseManifest(files)
	if err != nil {
		return store.Version{}, err
	}
	if m.Kind == "server" && s.cfg.Runtime == nil {
		return store.Version{}, &bundle.ValidationError{Problems: []bundle.Problem{{Path: bundle.ManifestName, Message: "server flats are not enabled on this host", Fix: `deploy a static build ("kind": "static")`}}}
	}
	unlock := s.lock(slugName)
	defer unlock()
	if _, err := s.st.GetFlat(ctx, slugName); errors.Is(err, store.ErrNotFound) {
		// A concurrent CreateFlat may win the race; the flat then exists,
		// which is all this save needs.
		if _, err := s.CreateFlat(ctx, slugName, m.Name, via); err != nil && !errors.Is(err, store.ErrExists) {
			return store.Version{}, err
		}
	} else if err != nil {
		return store.Version{}, err
	}
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
		return "", invalid(err)
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
		// The live and previewed versions are kept on top of keep_versions.
		if v.Pruned || v.Number == f.LiveVersion || inPreview[v.Number] {
			continue
		}
		if kept < keep {
			kept++
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
	// Data says what happened to the flat's data when the deploy may have
	// touched it.
	Data string
}

func (e *DeployError) Error() string {
	tail := "(the previous live version keeps serving)"
	if e.Previous == 0 {
		tail = "(nothing was live before, so the flat is still not deployed)"
	}
	if e.Data != "" {
		tail += ". " + e.Data
	}
	if e.Cause != nil {
		return fmt.Sprintf("deploy of version %d failed: %v %s", e.Version, e.Cause, tail)
	}
	return fmt.Sprintf("deploy of version %d failed its health check: GET %s returned %d %s %s", e.Version, e.Health.Path, e.Health.Status, e.Health.Error, tail)
}

func (e *DeployError) Unwrap() error { return e.Cause }

const healthTimeout = 15 * time.Second

// healthCheck GETs p from h. It never panics: a path that is not a valid
// request target fails the check, and so does a handler that panics. Text
// the handler produced is passed through redact.
func healthCheck(h http.Handler, p string, redact func(string) string) HealthResult {
	if p == "" {
		p = "/"
	}
	start := time.Now()
	res := HealthResult{Path: p}
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()
	rec := httptest.NewRecorder()
	errc := make(chan string, 1)
	go func() {
		msg := ""
		defer func() {
			if r := recover(); r != nil {
				msg = fmt.Sprint("handler panicked: ", r)
			}
			errc <- msg
		}()
		req, err := healthRequest(ctx, p)
		if err != nil {
			msg = err.Error()
			return
		}
		h.ServeHTTP(rec, req)
	}()
	select {
	case msg := <-errc:
		res.Error = redact(msg)
	case <-ctx.Done():
		res.Error = fmt.Sprintf("timed out after %s", healthTimeout)
	}
	res.Millis = time.Since(start).Milliseconds()
	if res.Error != "" {
		return res
	}
	res.Status = rec.Code
	// Redact before truncating, so a cut cannot leave part of a secret.
	body := redact(rec.Body.String())
	if len(body) > 300 {
		body = body[:300]
	}
	res.BodyHead = body
	res.OK = rec.Code >= 200 && rec.Code < 400
	return res
}

func healthRequest(ctx context.Context, p string) (*http.Request, error) {
	bad := func(why string) error {
		return fmt.Errorf("invalid health path %q: %s; set \"health\" in flats.json to a path such as \"/\" or \"/healthz\"", p, why)
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return nil, bad("it must start with a single /")
	}
	if strings.ContainsFunc(p, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return nil, bad("it must not contain spaces or control characters")
	}
	if _, err := url.ParseRequestURI(p); err != nil {
		return nil, bad(err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p, nil)
	if err != nil {
		return nil, bad(err.Error())
	}
	// Match what a server-side request looks like (as httptest.NewRequest does).
	req.Host = "example.com"
	req.RequestURI = p
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("User-Agent", "flats-healthcheck")
	return req, nil
}

func manifestOf(v store.Version) bundle.Manifest {
	var m bundle.Manifest
	_ = json.Unmarshal(v.Manifest, &m)
	return m
}

// Deploy makes version n live after a successful health check.
func (s *Service) Deploy(ctx context.Context, slugName string, n int, via Via) (DeployResult, error) {
	unlock := s.lock(slugName)
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return DeployResult{}, err
	}
	return s.deployLocked(ctx, f, n, "deploy", via)
}

// deployLocked starts version n on the live data, health-checks it and makes
// it live. The caller holds s.lock(f.Slug).
func (s *Service) deployLocked(ctx context.Context, f store.Flat, n int, kind string, via Via) (DeployResult, error) {
	start := time.Now()
	slugName := f.Slug
	v, err := s.st.GetVersion(ctx, slugName, n)
	if err != nil {
		return DeployResult{}, err
	}
	// Snapshot before the candidate starts: a server flat runs against the
	// live data while it is checked, so this is the last copy it cannot have
	// changed.
	snap, err := s.snapshotDB(slugName, n, f.LiveVersion)
	if err != nil {
		s.Event(ctx, slugName, "warn", "snapshot", "could not snapshot the database before deploy: "+err.Error(), nil)
	}
	// failed reports a failed deploy. ran says whether the candidate served
	// requests (its health check) against the live data: only then can it
	// have changed data, and the snapshot is kept as failed-v<n>-* (pruned
	// separately so failures cannot evict regular pre-deploy snapshots).
	failed := func(de *DeployError, ran bool) (DeployResult, error) {
		switch {
		case snap == "":
		case v.Kind == "server" && ran:
			kept := s.markFailedSnapshot(slugName, snap, n)
			de.Data = fmt.Sprintf("Version %d ran against the live data during its health check and may have changed it; database snapshot %s holds the data from just before this deploy", n, kept)
		default: // the candidate never touched data
			os.Remove(filepath.Join(s.snapshotDir(slugName), snap))
		}
		return DeployResult{}, de
	}
	d, err := s.build(ctx, slugName, v, s.dataDirOf(slugName))
	if err != nil {
		s.Event(ctx, slugName, "error", "deploy", fmt.Sprintf("%s of version %d failed to start: %v", kind, n, err), nil)
		return failed(&DeployError{Version: n, Previous: f.LiveVersion, Cause: err}, false)
	}
	h := healthCheck(d.handler, manifestOf(v).Health, d.redact)
	s.Event(ctx, slugName, map[bool]string{true: "info", false: "error"}[h.OK], "health",
		fmt.Sprintf("health check of version %d: GET %s -> %d in %dms %s", n, h.Path, h.Status, h.Millis, h.Error), h)
	if !h.OK {
		if d.inst != nil {
			d.inst.Stop()
		}
		return failed(&DeployError{Version: n, Previous: f.LiveVersion, Health: h}, true)
	}
	if snap != "" {
		s.Event(ctx, slugName, "info", "snapshot", fmt.Sprintf("saved database snapshot %s before deploying version %d", snap, n), nil)
	}
	return s.activate(ctx, f, d, h, kind, via, start)
}

// activate makes the started and checked d live. The caller holds
// s.lock(f.Slug).
func (s *Service) activate(ctx context.Context, f store.Flat, d *deployed, h HealthResult, kind string, via Via, start time.Time) (DeployResult, error) {
	slugName, n, prev := f.Slug, d.version.Number, f.LiveVersion
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
// The target is checked on a copy of that snapshot before the live data is
// touched, and the current database is saved first (before-restore-*) and
// put back if anything fails.
func (s *Service) RollbackWithData(ctx context.Context, slugName string, to int, restoreData bool, via Via) (DeployResult, error) {
	unlock := s.lock(slugName)
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return DeployResult{}, err
	}
	if f.LiveVersion == 0 {
		return DeployResult{}, ErrNotDeployed
	}
	if to == 0 {
		if to, err = s.previousLive(ctx, f); err != nil {
			return DeployResult{}, err
		}
	}
	if to == f.LiveVersion {
		return DeployResult{}, fmt.Errorf("%w: version %d is already live", ErrConflict, to)
	}
	if !restoreData {
		return s.deployLocked(ctx, f, to, "rollback", via)
	}
	snap, err := s.liveSnapshot(slugName, f.LiveVersion)
	if err != nil {
		return DeployResult{}, err
	}
	return s.restoreLocked(ctx, f, snap, to, "rollback", via)
}

// previousLive returns the version that was live before the current one.
func (s *Service) previousLive(ctx context.Context, f store.Flat) (int, error) {
	hist, err := s.st.ListDeployments(ctx, f.Slug, 200)
	if err != nil {
		return 0, err
	}
	for _, d := range hist {
		// A redeploy of the live version (to apply secrets) records itself
		// as its previous version; it says nothing about what came before.
		if d.Version == f.LiveVersion && d.Previous > 0 && d.Previous != d.Version {
			return d.Previous, nil
		}
	}
	return 0, fmt.Errorf("%w: there is no earlier deployed version to roll back to", ErrConflict)
}

// RestoreSnapshot puts back database snapshot name (see Snapshots) and
// restarts the live version on it. The current database is saved first as a
// before-restore snapshot and put back if the live version fails its health
// check on the restored data.
func (s *Service) RestoreSnapshot(ctx context.Context, slugName, name string, via Via) (DeployResult, error) {
	if !validSnapshotName(name) {
		return DeployResult{}, invalidf("%q is not a database snapshot name (see the snapshots list)", name)
	}
	unlock := s.lock(slugName)
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return DeployResult{}, err
	}
	if f.LiveVersion == 0 {
		return DeployResult{}, ErrNotDeployed
	}
	return s.restoreLocked(ctx, f, name, f.LiveVersion, "restore", via)
}

// restoreLocked replaces the flat's database with snapshot snap and makes
// version to live on it. The caller holds s.lock(f.Slug).
//
// The target is first started and health-checked on a scratch copy holding
// the snapshot, so a broken or missing target never touches live data. Then
// the live version is stopped, the current database is saved as a
// before-restore snapshot, and the snapshot is installed. If anything fails
// after that, the saved database is put back and the old version restarted.
func (s *Service) restoreLocked(ctx context.Context, f store.Flat, snap string, to int, kind string, via Via) (DeployResult, error) {
	start := time.Now()
	slugName := f.Slug
	v, err := s.st.GetVersion(ctx, slugName, to)
	if err != nil {
		return DeployResult{}, err
	}
	if v.Pruned {
		return DeployResult{}, fmt.Errorf("%w: version %d was pruned by the retention policy", ErrConflict, to)
	}
	snapPath := filepath.Join(s.snapshotDir(slugName), snap)
	if _, err := os.Stat(snapPath); err != nil {
		return DeployResult{}, fmt.Errorf("database snapshot %s: %w", snap, store.ErrNotFound)
	}
	if err := s.trialRun(ctx, f, v, snapPath); err != nil {
		return DeployResult{}, err
	}

	lf := s.state(slugName)
	old := lf.cur.Swap(nil) // the flat answers 503 during the swap
	if old != nil && old.inst != nil {
		old.inst.Stop()
	}
	// When version `to` becomes live, the current data is what existed
	// before it went live, so a later restore_data rollback from `to` undoes
	// this restore. A restore under the live version keeps before-restore.
	backupKind := fmt.Sprintf("before-v%d", to)
	if to == f.LiveVersion {
		backupKind = "before-restore"
	}
	backup, err := s.snapshotAs(slugName, backupKind, "")
	if err != nil {
		rerr := s.restart(ctx, slugName, old)
		return DeployResult{}, &DeployError{Version: to, Previous: f.LiveVersion,
			Cause: fmt.Errorf("could not save the current database before the restore: %w", err),
			Data:  joinNotes("Nothing was changed", rerr)}
	}
	// undo puts the current database back and restarts the old version.
	undo := func() string {
		src := ""
		if backup != "" {
			src = filepath.Join(s.snapshotDir(slugName), backup)
		}
		note := "The database was put back from snapshot " + backup
		if backup == "" {
			note = "The flat had no database before, so the restored one was removed"
		}
		if err := s.installDB(slugName, src); err != nil {
			note = fmt.Sprintf("Putting the database back failed (%v); the data from before the restore is in snapshot %s", err, backup)
		}
		return joinNotes(note, s.restart(ctx, slugName, old))
	}
	fail := func(de *DeployError) (DeployResult, error) {
		de.Version, de.Previous, de.Data = to, f.LiveVersion, undo()
		s.Event(ctx, slugName, "error", "snapshot", fmt.Sprintf("restoring snapshot %s failed: %v", snap, de), nil)
		return DeployResult{}, de
	}
	if err := s.installDB(slugName, snapPath); err != nil {
		return fail(&DeployError{Cause: fmt.Errorf("install snapshot %s: %w", snap, err)})
	}
	d, err := s.build(ctx, slugName, v, s.dataDirOf(slugName))
	if err != nil {
		return fail(&DeployError{Cause: err})
	}
	h := healthCheck(d.handler, manifestOf(v).Health, d.redact)
	if !h.OK {
		if d.inst != nil {
			d.inst.Stop()
		}
		return fail(&DeployError{Health: h})
	}
	msg := fmt.Sprintf("restored database snapshot %s", snap)
	if backup != "" {
		msg += fmt.Sprintf("; the database from before is saved as snapshot %s", backup)
	}
	res, err := s.activate(ctx, f, d, h, kind, via, start)
	if err != nil {
		return fail(&DeployError{Cause: err})
	}
	s.Event(ctx, slugName, "warn", "snapshot", msg, nil)
	return res, nil
}

// trialRun starts version v on a scratch copy of the data holding the
// snapshot at snapPath and health-checks it.
func (s *Service) trialRun(ctx context.Context, f store.Flat, v store.Version, snapPath string) error {
	dir := filepath.Join(s.flatDir(f.Slug), "restore-trial")
	os.RemoveAll(dir)
	defer os.RemoveAll(dir)
	untouched := "The data was not touched"
	if v.Kind == "server" {
		if err := snapshotFiles(s.dataDirOf(f.Slug), dir); err != nil {
			return &DeployError{Version: v.Number, Previous: f.LiveVersion, Cause: fmt.Errorf("copy data for a trial run: %w", err), Data: untouched}
		}
		if err := copyFile(snapPath, filepath.Join(dir, "db.sqlite")); err != nil {
			return &DeployError{Version: v.Number, Previous: f.LiveVersion, Cause: fmt.Errorf("copy snapshot for a trial run: %w", err), Data: untouched}
		}
	}
	d, err := s.build(ctx, f.Slug, v, dir)
	if err != nil {
		return &DeployError{Version: v.Number, Previous: f.LiveVersion, Cause: err, Data: untouched}
	}
	h := healthCheck(d.handler, manifestOf(v).Health, d.redact)
	if d.inst != nil {
		d.inst.Stop()
	}
	s.Event(ctx, f.Slug, map[bool]string{true: "info", false: "error"}[h.OK], "health",
		fmt.Sprintf("health check of version %d on the snapshot to restore: GET %s -> %d in %dms %s", v.Number, h.Path, h.Status, h.Millis, h.Error), h)
	if !h.OK {
		return &DeployError{Version: v.Number, Previous: f.LiveVersion, Health: h, Data: untouched}
	}
	return nil
}

// restart brings back a version stopped for a data swap. It returns why it
// could not.
func (s *Service) restart(ctx context.Context, slugName string, old *deployed) error {
	if old == nil {
		return nil
	}
	d := old
	if old.inst != nil {
		var err error
		if d, err = s.build(ctx, slugName, old.version, s.dataDirOf(slugName)); err != nil {
			s.Event(ctx, slugName, "error", "deploy", fmt.Sprintf("could not restart version %d: %v", old.version.Number, err), nil)
			return fmt.Errorf("version %d could not be restarted (%v); the flat answers 503 until a version is deployed", old.version.Number, err)
		}
	}
	s.state(slugName).cur.Store(d)
	return nil
}

func joinNotes(note string, err error) string {
	if err != nil {
		return note + "; " + err.Error()
	}
	return note
}

// Deployments returns the deploy history.
func (s *Service) Deployments(ctx context.Context, slugName string) ([]store.Deployment, error) {
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return nil, err
	}
	ds, err := s.st.ListDeployments(ctx, slugName, 100)
	if ds == nil && err == nil {
		ds = []store.Deployment{}
	}
	return ds, err
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
		return ActionResult{}, invalidf("unknown visibility %q (use private, public-listed or public-unlisted)", vis)
	}
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	if vis.Public() && s.cfg.Public == nil {
		return ActionResult{}, errPublicDisabled()
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
	if !vis.Public() {
		s.stopPublicRedirects(slugName)
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
	// Redirects to the flat end with it (their rows went with the flat).
	var olds []string
	s.mu.Lock()
	for old, r := range s.redir {
		if r.cur == slugName {
			olds = append(olds, old)
		}
	}
	s.mu.Unlock()
	for _, old := range olds {
		s.stopRedirect(old)
	}
	s.eventCount.Delete(slugName)
	s.logLimits.Delete(slugName)
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
// The approval is claimed before anything is applied, so of two concurrent
// decisions exactly one acts and the other gets ErrConflict.
func (s *Service) Decide(ctx context.Context, id string, approve bool) (store.Approval, error) {
	a, err := s.st.GetApproval(ctx, id)
	if err != nil {
		return a, err
	}
	// lost reports a decision that lost to another one (or why it failed).
	lost := func(cause error) (store.Approval, error) {
		cur, err := s.st.GetApproval(ctx, id)
		if err != nil {
			return a, err
		}
		if cur.Status == "pending" {
			return cur, cause
		}
		return cur, fmt.Errorf("%w: approval is already %s", ErrConflict, cur.Status)
	}
	if a.Status != "pending" {
		return lost(nil)
	}
	if !approve {
		if err := s.st.DecideApproval(ctx, id, "rejected", "rejected by the operator", s.now()); err != nil {
			return lost(err)
		}
		s.Event(ctx, a.Flat, "info", "approval", a.Action+" rejected by the operator", nil)
		return s.st.GetApproval(ctx, id)
	}
	if err := s.st.ClaimApproval(ctx, id); err != nil {
		return lost(err)
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
	if derr := s.st.FinishApproval(ctx, id, status, result, s.now()); derr != nil {
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
	// State and Detail are the preview host's network state, as for
	// FlatView.PrivateState.
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// hostState returns a served private host's state and detail ("" when the
// host is not served).
func (s *Service) hostState(host string) (string, string) {
	for _, h := range s.cfg.Private.Status().Hosts {
		if h.Host == host {
			return h.State, h.Detail
		}
	}
	return "", ""
}

// maxPreviews bounds open previews per flat: each runs its own node and,
// for server flats, holds a copy of the data.
const maxPreviews = 5

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
	if ps, err := s.st.ListPreviews(ctx, slugName); err != nil {
		return PreviewView{}, err
	} else if len(ps) >= maxPreviews {
		return PreviewView{}, fmt.Errorf("%w: %d previews are already open; close one first", ErrConflict, len(ps))
	}
	host := slug.PreviewHost(slugName)
	dataDir := filepath.Join(s.flatDir(slugName), "previews", host)
	if v.Kind == "server" {
		if quota := s.diskQuota(); quota > 0 {
			used, extra := dirSize(s.flatDir(slugName)), dirSize(s.dataDirOf(slugName))
			if used+extra > quota {
				return PreviewView{}, fmt.Errorf("%w: copying the data for a preview would use %d bytes, over the flat's %d-byte disk quota; close previews or remove data or old versions", ErrConflict, used+extra, quota)
			}
		}
		if err := snapshotData(s.dataDirOf(slugName), dataDir); err != nil {
			os.RemoveAll(dataDir)
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
	stop := func() {
		if d.inst != nil {
			d.inst.Stop()
		}
		os.RemoveAll(dataDir)
	}
	url, err := s.cfg.Private.Serve(ctx, host, h, true)
	if err != nil {
		stop()
		return PreviewView{}, err
	}
	rec := store.Preview{Host: host, Flat: slugName, Version: n, CreatedAt: now, LastAccess: now}
	if err := s.st.InsertPreview(ctx, rec); err != nil {
		_ = s.cfg.Private.Stop(host)
		stop()
		return PreviewView{}, err
	}
	s.mu.Lock()
	s.prevs[host] = p
	s.mu.Unlock()
	s.Event(ctx, slugName, "info", "preview", fmt.Sprintf("preview of version %d at %s", n, url), nil)
	pv := PreviewView{Preview: rec, URL: url, ExpiresAt: now.Add(s.previewTTL())}
	pv.State, pv.Detail = s.hostState(host)
	return pv, nil
}

// ListPreviews returns open previews of a flat.
func (s *Service) ListPreviews(ctx context.Context, slugName string) ([]PreviewView, error) {
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return nil, err
	}
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
		pv := PreviewView{Preview: p, URL: s.cfg.Private.URL(p.Host), ExpiresAt: p.LastAccess.Add(s.previewTTL())}
		pv.State, pv.Detail = s.hostState(p.Host)
		out = append(out, pv)
	}
	return out, nil
}

// ClosePreview removes one preview. Hosts that are not open previews (a
// flat, the console, a redirect) are never touched: it returns
// store.ErrNotFound for them.
func (s *Service) ClosePreview(ctx context.Context, host string) error {
	s.mu.Lock()
	p := s.prevs[host]
	delete(s.prevs, host)
	s.mu.Unlock()
	if p == nil {
		// A preview row without a running preview (e.g. a half-failed open)
		// is still a preview host.
		if _, err := s.st.GetPreview(ctx, host); err != nil {
			return err
		}
	}
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

// Sweep expires previews and redirects, flushes page-view counters and
// trims flat logs.
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
	s.mu.Unlock()
	for _, p := range expired {
		_ = s.st.TouchPreview(ctx, p.host, time.UnixMilli(p.last.Load()))
		_ = s.ClosePreview(ctx, p.host)
		s.Event(ctx, p.flat, "info", "preview", fmt.Sprintf("preview %s expired after %s without visits", p.host, ttl), nil)
	}
	s.syncRedirects(ctx)
	for slugName, lf := range live {
		s.flushViews(ctx, slugName, lf)
	}
	s.logLimits.Range(func(k, v any) bool {
		s.reportDroppedLogs(k.(string), v.(*logLimit))
		return true
	})
	if flats, err := s.st.EventFlats(ctx); err == nil {
		for _, f := range flats {
			s.pruneEvents(ctx, f)
		}
	}
	s.checkQuotas(ctx, live, now)
}

// checkQuotas warns (at most daily) about flats whose data grew past the
// disk quota. Uploads over quota are refused outright; database and file
// growth from a running server flat can only be reported.
func (s *Service) checkQuotas(ctx context.Context, live map[string]*liveFlat, now time.Time) {
	quota := s.diskQuota()
	if quota <= 0 {
		return
	}
	for slugName := range live {
		used := dirSize(s.flatDir(slugName))
		if used <= quota {
			continue
		}
		s.mu.Lock()
		last := s.quotaWarned[slugName]
		if now.Sub(last) < 24*time.Hour {
			s.mu.Unlock()
			continue
		}
		s.quotaWarned[slugName] = now
		s.mu.Unlock()
		s.Event(ctx, slugName, "error", "quota", fmt.Sprintf("flat uses %d bytes, over its %d-byte disk quota; new uploads are refused until data or old versions are removed", used, quota), nil)
	}
}

func (s *Service) flushViews(ctx context.Context, slugName string, lf *liveFlat) {
	s.flushPaths(ctx, slugName, lf)
	if n := lf.views.Swap(0); n > 0 {
		if err := s.st.AddPageViews(ctx, slugName, s.now().UTC().Format("2006-01-02"), n); err != nil {
			lf.views.Add(n)
		}
	}
}

// PageViews returns daily request-count page views.
func (s *Service) PageViews(ctx context.Context, slugName string, days int) ([]store.DayCount, error) {
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return nil, err
	}
	s.mu.Lock()
	lf := s.live[slugName]
	s.mu.Unlock()
	if lf != nil {
		s.flushViews(ctx, slugName, lf)
	}
	since := s.now().UTC().AddDate(0, 0, 1-days).Format("2006-01-02")
	pv, err := s.st.PageViewsSince(ctx, slugName, since)
	if pv == nil && err == nil {
		pv = []store.DayCount{}
	}
	return pv, err
}

// --- logs ---

// Events returns log events of a flat.
func (s *Service) Events(ctx context.Context, slugName, kind string, after int64, limit int) ([]store.Event, error) {
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	evs, err := s.st.ListEvents(ctx, slugName, kind, after, limit)
	if evs == nil && err == nil {
		evs = []store.Event{}
	}
	return evs, err
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
