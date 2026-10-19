// Package core implements Flats: flats, versions, deployments, exposure,
// previews and approvals. Every interface (HTTP API, MCP, CLI, console) goes
// through Service.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
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
	// ErrPending marks an action recorded for the operator and not applied.
	ErrPending = errors.New("pending approval")
)

// Runtime starts server flats (nil disables kind "server").
type Runtime interface {
	Start(ctx context.Context, spec RuntimeSpec) (Instance, error)
}

// RuntimeSpec describes one server flat instance.
type RuntimeSpec struct {
	Now            func() time.Time // managed document activity clock; nil uses time.Now
	ManagedDocs    bool             // live built-in document; core owns readiness and idle sleep
	Lazy           bool             // restore a logical document without starting its child
	Flat           string
	Version        int
	Generation     int64                                // host activation order, independent of Version
	NextGeneration func(context.Context) (int64, error) // worker recovery
	Dir            string                               // version files (read-only)
	Entry          string
	DataDir        string // per-flat data (SQLite, files); a copy for previews
	Env            map[string]string
	NetworkOrigins []string // operator-approved exact origins; JS only, default deny
	ImagePath      string   // saved document image; live managed documents only, in a core-owned directory
	Log            func(level, msg string)
	Usage          UsageSink // CPU and memory of the instance's processes; nil: not accounted
}

// Instance is a running server flat.
type Instance interface {
	http.Handler
	Stop()
}

// Config wires a Service.
type Config struct {
	DataDir   string
	Store     *store.Store
	Private   PrivateNet
	Lifecycle LifecycleNet // optional provider manager; independent of legacy adapters
	Public    PublicNet    // nil disables public flats
	DocsApp   DocsApp
	Runtime   Runtime // nil disables server flats
	// Settings holds the system settings (config.json). Nil keeps the
	// frozen defaults in memory.
	Settings   *SettingsSource
	ConsoleURL func() string
	// ConsoleURLFor, when set, is the console address for a request: the
	// console domain the request came through, else what ConsoleURL says.
	// Approval links use it so they open where the agent connected.
	ConsoleURLFor func(context.Context) string
	// PrivateBackend is network.private_backend. When it is "tailscale" the
	// operator chose the tailnet for the console and private routes, so new
	// flats are allowed on Tailscale from the start instead of loopback only.
	PrivateBackend string
	// Reserved are host names flats may not use (e.g. the console host).
	Reserved []string
	// ReservedNames, when set, returns more such names at each check: the
	// console domains and the console nodes still being retired.
	ReservedNames func() []string
	Now           func() time.Time
	Logf          func(format string, args ...any)
}

// Service is the Flats core.
type Service struct {
	cfg   Config
	st    *store.Store
	now   func() time.Time
	logf  func(string, ...any)
	locks sync.Map // slug -> *sync.Mutex

	docsMu   sync.Mutex
	docsRefs map[string]int
	mu       sync.Mutex
	live     map[string]*liveFlat // slug -> state
	prevs    map[string]*preview  // host -> preview
	redir    map[string]*redirect // old slug -> redirect being served
	// consoleAdding holds console domains whose addition is in progress;
	// guarded by mu.
	consoleAdding map[string]bool
	// previewAdding holds preview hosts chosen but not yet registered;
	// guarded by mu.
	previewAdding map[string]bool
	quotaWarned   map[string]time.Time
	stop          chan struct{}
	wg            sync.WaitGroup
	secretKey     []byte
	runtimeMu     sync.Mutex
	runtimeOwners map[string]int64 // data directory -> newest runtime start
	routeEpochs   sync.Map         // route key -> *routeEpoch
	designOnce    sync.Once
	design        *designState
	// docsCatchUps holds when each document last imported for a read.
	docsCatchUps sync.Map
	// docsRollbacks holds the ids of document rollbacks in progress, whose
	// holds are not settled yet.
	docsRollbacks sync.Map
	// boardPending holds the boards whose outbox recorded entries the
	// ticker has not imported yet.
	boardPending sync.Map
	// boardRetry holds when the ticker next tries a board whose import
	// failed, and the delay it waited (boardBackoff).
	boardRetry sync.Map
	boardStats boardStats
	settings   *SettingsSource
	eventCount sync.Map // slug -> *atomic.Int64, events since the last prune
	logLimits  sync.Map // slug -> *logLimit
	// snapshot holds the host-wide figures of the last Sweep for Metrics.
	snapshot atomic.Pointer[hostSnapshot]
	diskMu   sync.Mutex
	disk     map[string]diskSample // flat ID -> its last disk measurement
	// usageMu serializes usage flushes with the stats reads of Usage, so a
	// read sees each usage exactly once: stored or still in memory. The
	// request path never takes it.
	usageMu sync.Mutex
}

// redirect owns an old slug while its route is served or provider teardown is
// still retryable. teardownOnly entries reserve the slug but must never be
// served again. retired entries have confirmed teardown and remain only until
// their durable expired row can be removed.
type redirect struct {
	cur          string
	public       bool // also served on the public network
	teardownOnly bool
	retired      bool
}

type liveFlat struct {
	cur           atomic.Pointer[deployed]
	privateServed bool
	publicServed  bool
	limiter       *limiter
	editLimiter   *limiter // live document edits through the management channel
	views         atomic.Int64
	trafficMu     sync.Mutex
	traffic       map[trafficKey]int64
	// usage counts the live route's requests and its workers' CPU and
	// memory; previewCPU and checkCPU the CPU of its preview and activation
	// check workers. uid is the stored flat's ID, bound the first time this
	// state serves a stored flat; usage is persisted under it only.
	usage      *flatUsage
	previewCPU *workerUsage
	checkCPU   *workerUsage
	uid        atomic.Pointer[string]
}

type deployed struct {
	version     store.Version
	handler     http.Handler
	inst        Instance
	redact      func(string) string // removes secret values from the flat's output
	environment *runtimeEnvironment
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
	if cfg.Settings == nil {
		cfg.Settings = memorySettings()
	}
	s := &Service{cfg: cfg, st: cfg.Store, now: cfg.Now, logf: cfg.Logf, settings: cfg.Settings,
		runtimeOwners: map[string]int64{}, docsRefs: map[string]int{}, live: map[string]*liveFlat{}, prevs: map[string]*preview{}, redir: map[string]*redirect{}, quotaWarned: map[string]time.Time{}, stop: make(chan struct{})}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "flats"), 0o700); err != nil {
		return nil, err
	}
	if err := CheckSecretKey(ctx, cfg.DataDir, cfg.Store); err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		return nil, err
	}
	s.secretKey = key
	if err := s.recoverStorageOperations(ctx); err != nil {
		return nil, err
	}
	if err := s.migrateLegacyDrafts(ctx); err != nil {
		return nil, err
	}
	if err := s.recoverVersionDeletions(ctx); err != nil {
		return nil, err
	}
	if err := s.recoverRestoreJournals(ctx); err != nil {
		return nil, err
	}
	if err := s.restore(ctx); err != nil {
		return nil, err
	}
	s.startSavedDocs(ctx)
	s.cleanupDocs()
	s.startPendingDesigns(ctx)
	s.retainAllRevisions(ctx, false)
	s.CloseDueGroups(ctx)
	s.storeSnapshot(ctx, s.flatTypes(ctx, s.liveFlats()))
	s.wg.Add(2)
	go s.sweeper()
	go s.designLoop()
	return s, nil
}

// Close stops background work and all served hosts.
func (s *Service) Close() error {
	close(s.stop)
	s.routeEpochs.Range(func(key, value any) bool { value.(*routeEpoch).cancel(); return true })
	s.wg.Wait()
	s.mu.Lock()
	live := make(map[string]*liveFlat, len(s.live))
	for slug, lf := range s.live {
		live[slug] = lf
	}
	previews := make([]*preview, 0, len(s.prevs))
	for _, p := range s.prevs {
		previews = append(previews, p)
	}
	s.mu.Unlock()
	for slug, lf := range live {
		s.flushViews(context.Background(), slug, lf)
		if d := lf.cur.Load(); d != nil && d.inst != nil {
			d.inst.Stop()
		}
	}
	for _, p := range previews {
		if p.inst != nil {
			p.inst.Stop()
		}
	}
	// Every worker has stopped and its exit accounting has run (Stop returns
	// after it), so the last CPU reaches flats.db.
	s.flushUsage(context.Background(), slices.Collect(maps.Values(live))...)
	return nil
}

func (s *Service) lock(slug string) func() {
	m, _ := s.locks.LoadOrStore(slug, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// flatDir is the storage directory of a flat. Slugs are validated before
// they reach it; cleaning the slug as a rooted path also keeps any other
// value inside flats/, in the form static analysis recognizes.
func (s *Service) flatDir(slug string) string {
	return filepath.Join(s.cfg.DataDir, "flats", filepath.Clean("/"+slug))
}

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

// runtimeLogLine is the longest runtime log message stored. Callers pass
// messages with secret values already removed, so the cut cannot leave part of
// a secret.
const runtimeLogLine = 8 << 10

// runtimeLog records a log line from a flat's code, rate limited per flat.
func (s *Service) runtimeLog(slug string, version int, level, msg string) {
	v, _ := s.logLimits.LoadOrStore(slug, &logLimit{lim: newLimiter(runtimeLogRate)})
	ll := v.(*logLimit)
	if !ll.lim.allow() {
		ll.dropped.Add(1)
		return
	}
	s.reportDroppedLogs(slug, ll)
	s.Event(context.Background(), slug, level, "runtime", truncateLogLine(msg, runtimeLogLine), map[string]int{"version": version})
}

func truncateLogLine(msg string, max int) string {
	if len(msg) <= max {
		return msg
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "… (truncated)"
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
		lf.bindUID(f.ID)
		if f.LiveVersion > 0 {
			v, err := s.st.GetVersion(ctx, f.Slug, f.LiveVersion)
			if err != nil {
				s.logf("restore %s: %v", f.Slug, err)
				continue
			}
			d, err := s.build(context.WithValue(ctx, lazyDocsKey{}, true), f.Slug, v, s.dataDirOf(f.Slug), lf.liveSink())
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
	if rs, err := s.st.ActiveRedirects(ctx, time.Time{}); err == nil {
		now := s.now()
		for _, r := range rs {
			if _, err := s.st.GetFlat(ctx, r.Old); err == nil {
				continue // a flat uses that slug again; never shadow it
			}
			if r.Until.After(now) {
				s.serveRedirect(ctx, r.Old, r.Flat)
				continue
			}
			// Expired rows remain durable teardown owners until StopSlug
			// confirms. Keep the slug reserved after restart; Sweep performs
			// the retry without reopening an already-expired route.
			s.mu.Lock()
			s.redir[r.Old] = &redirect{cur: r.Flat, teardownOnly: true}
			s.mu.Unlock()
		}
	}
	// An approval left in "applying" is resumed once. A deployment already
	// recorded for it is not activated again.
	s.resumeApplying(ctx)
	s.restorePreviews(ctx)
	return nil
}

func (s *Service) state(slug string) *liveFlat {
	s.mu.Lock()
	defer s.mu.Unlock()
	lf, ok := s.live[slug]
	if !ok {
		lf = &liveFlat{limiter: newLimiter(s.rateLimit()), editLimiter: newLimiter(documentEditRate),
			usage: newFlatUsage(s.now), previewCPU: newWorkerUsage(s.now), checkCPU: newWorkerUsage(s.now)}
		lf.usage.worker.owner = lf
		s.live[slug] = lf
	}
	return lf
}

// build creates the handler for a version. usage receives the CPU and memory
// of its worker processes: the live flat's sink for a live build, a role sink
// for previews and activation checks.
func (s *Service) build(ctx context.Context, slugName string, v store.Version, dataDir string, usage UsageSink) (*deployed, error) {
	env, err := s.captureEnvironment(ctx, slugName, v)
	if err != nil {
		return nil, err
	}
	return s.buildWithEnvironment(ctx, slugName, v, dataDir, env, usage)
}

func (s *Service) buildWithEnvironment(ctx context.Context, slugName string, v store.Version, dataDir string, snapshot *runtimeEnvironment, usage UsageSink) (*deployed, error) {
	if v.Pruned {
		return nil, fmt.Errorf("version %d was pruned by the retention policy", v.Number)
	}
	var m bundle.Manifest
	_ = json.Unmarshal(v.Manifest, &m)
	if contenttype.FromManifest(v.Manifest) == contenttype.Design {
		inst := s.designInstance(slugName)
		return &deployed{version: v, handler: inst, inst: inst, redact: func(s string) string { return s }, environment: snapshot}, nil
	}
	if contenttype.FromManifest(v.Manifest) == contenttype.Board {
		return s.buildBoard(slugName, v, m.Name, dataDir, snapshot)
	}
	dir := s.contentDir(slugName, v)
	switch v.Kind {
	case "server":
		if s.cfg.Runtime == nil {
			return nil, fmt.Errorf("%w: server flats are not enabled on this host", ErrRuntimeUnavailable)
		}
		env := maps.Clone(snapshot.values)
		redact := snapshot.redact
		networkOrigins := slices.Clone(snapshot.networkOrigins)
		var err error
		runtimeDir, entry := dir, m.Entry
		release := func() {}
		var assets []string
		imagePath := ""
		if contenttype.FromManifest(v.Manifest) == contenttype.Docs {
			// Revisions of the live document name this version's other
			// files by its hash: record them before its worker can.
			if dataDir == s.dataDirOf(slugName) {
				if err := s.registerDocsSource(ctx, slugName, v, dir); err != nil {
					return nil, fmt.Errorf("record the document's files for revisions: %w", err)
				}
			}
			runtimeDir, imagePath, assets, release, err = s.docsRuntime(v, dir)
			if err != nil {
				return nil, err
			}
			entry = s.cfg.DocsApp.Entry
			networkOrigins = nil // The embedded docs app has no outbound capability.
		}
		managedDocs := assets != nil && dataDir == s.dataDirOf(slugName)
		if !managedDocs {
			imagePath = "" // previews and scratch trials never save an image
		}
		if managedDocs {
			// The flat whose live sink this build reports to; a rename holds
			// it out of s.live while it rebuilds, so it is not looked up.
			lf := liveFlatOf(usage)
			if lf == nil {
				lf = s.state(slugName)
			}
			if old := docsManaged(lf.cur.Load()); old != nil {
				old.InvalidateClean()
			}
		}
		generation, nextGeneration, undoGeneration, err := s.runtimeGeneration(ctx, dataDir)
		if err != nil {
			release()
			return nil, err
		}
		lazy, _ := ctx.Value(lazyDocsKey{}).(bool)
		inst, err := s.cfg.Runtime.Start(ctx, RuntimeSpec{
			ManagedDocs: managedDocs, Lazy: managedDocs && lazy, Now: s.now,
			Flat: slugName, Version: v.Number,
			Generation: generation, NextGeneration: nextGeneration,
			Dir: runtimeDir, Entry: entry, DataDir: dataDir, Env: env, NetworkOrigins: networkOrigins, ImagePath: imagePath,
			Usage: usage,
			Log: func(level, msg string) {
				s.runtimeLog(slugName, v.Number, level, redact(msg))
			}})
		if err != nil {
			undoGeneration()
			release()
			// Start errors can carry the worker's stderr.
			return nil, errors.New(redact(err.Error()))
		}
		base := &runtimeInstance{Instance: inst, release: func() { release(); undoGeneration() }}
		var managed Instance = base
		if control, ok := inst.(ManagedDocsInstance); ok && managedDocs {
			managed = &managedDocsRuntime{runtimeInstance: base, control: control}
		}
		d := &deployed{version: v, inst: managed, redact: redact, environment: snapshot}
		var handler http.Handler = managed
		if managedDocs && docsManaged(d) != nil {
			handler = s.docsDemand(slugName, d)
		}
		if assets != nil {
			handler = docsAssets(handler, dir, assets)
			if managedDocs {
				handler = s.docsRevisionRoutes(slugName, handler)
			}
		}
		d.handler = handler
		return d, nil
	default:
		return &deployed{version: v, handler: &site.Static{Dir: dir, Entry: m.Entry, SPA: m.SPA, NotFound: m.NotFound, ModTime: v.CreatedAt},
			redact: func(s string) string { return s }}, nil
	}
}

// siteHandler serves the live version of slug (used for both networks).
func (s *Service) siteHandler(slugName string, public bool) http.Handler {
	lf := s.state(slugName)
	var epoch *routeEpoch
	if public {
		epoch = s.routeEpoch("public:" + slugName)
	}
	route := routePrivate
	if public {
		route = routePublic
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Counted before any check, so Flats' own answers (the public
		// epoch's and "not deployed" 503s, the rate limit's 429) count too.
		serveCounted(lf.usage, route, w, r, func(w http.ResponseWriter, r *http.Request) { s.serveSite(lf, slugName, public, epoch, w, r) })
	})
}

// serveSite answers one request of slug's live route.
func (s *Service) serveSite(lf *liveFlat, slugName string, public bool, epoch *routeEpoch, w http.ResponseWriter, r *http.Request) {
	if public {
		var done func()
		r, done = epoch.request(r)
		defer done()
		if r.Context().Err() != nil {
			http.Error(w, "public exposure is not active", http.StatusServiceUnavailable)
			return
		}
		f, err := s.st.GetFlat(r.Context(), slugName)
		if err != nil || !f.Visibility.Public() {
			http.Error(w, "public exposure is not active", http.StatusServiceUnavailable)
			return
		}
		stripIdentity(r)
	}
	if !lf.limiter.allow() {
		if lf.usage != nil {
			lf.usage.countRateLimited(lf.usage.today())
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	d := lf.cur.Load()
	if d == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "<!doctype html><title>%s</title><p>This flat has not been deployed yet.</p>", html.EscapeString(slugName))
		return
	}
	if isPageView(r) {
		lf.views.Add(1)
		lf.recordPath(s.now().UTC().Format("2006-01-02"), r.URL.Path)
	}
	d.handler.ServeHTTP(w, trustedAccess(r, public))
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
	if ln, ok := s.lifecycleNet(); ok {
		return s.ensureLifecycle(ctx, f, ln)
	}
	lf := s.state(f.Slug)
	if lf.cur.Load() == nil {
		if f.Visibility.Public() {
			return fmt.Errorf("%w: no current runtime is serving", ErrNotDeployed)
		}
		return nil
	}
	// Serve is idempotent: it swaps the handler of a running host and
	// restarts one that failed or was stopped, so never trust privateServed
	// to mean the host is up.
	if _, err := s.cfg.Private.Serve(ctx, f.Slug, s.siteHandler(f.Slug, false), false); err != nil {
		return fmt.Errorf("private exposure: %w", err)
	}
	lf.privateServed = true
	portal, err := s.st.ProviderPermitted(ctx, f.Slug, store.ProviderPortal)
	if err != nil {
		return err
	}
	wantPublic := f.Visibility.Canonical().Public() && portal && s.cfg.Public != nil
	hidden := false
	if wantPublic {
		if hidden, err = s.portalHidden(ctx, f.Slug); err != nil {
			return err
		}
	}
	switch {
	case f.Visibility.Public() && portal && s.cfg.Public == nil:
		return errPublicDisabled()
	case wantPublic && !lf.publicServed:
		if _, err := s.cfg.Public.Serve(ctx, f.Slug, s.siteHandler(f.Slug, true), hidden); err != nil {
			return fmt.Errorf("public exposure: %w", err)
		}
		lf.publicServed = true
	case wantPublic && lf.publicServed:
		if err := s.cfg.Public.SetHidden(f.Slug, hidden); err != nil {
			return fmt.Errorf("public listing: %w", err)
		}
	case !wantPublic && lf.publicServed:
		if err := s.cfg.Public.Stop(f.Slug); err != nil {
			return fmt.Errorf("stop public exposure: %w", err)
		}
		s.revokeRoute("public:" + f.Slug)
		lf.publicServed = false
	}
	if wantPublic {
		opened := false
		publicURL := s.cfg.Public.URL(f.Slug)
		for _, host := range s.cfg.Public.Status().Hosts {
			if (host.Host == f.Slug || (publicURL != "" && host.URL == publicURL)) && (host.State == "ready" || host.State == "starting" || host.State == "key-expiring") {
				opened = true
			}
		}
		if !opened {
			return fmt.Errorf("%w: no legacy public route opened", ErrProviderNotReady)
		}
	}
	return nil
}

func errPublicDisabled() error {
	return fmt.Errorf("%w: public exposure is disabled on this host (start `flats serve` with --portal=true)", ErrProviderUnavailable)
}

// --- flats ---

// FlatView is a flat with derived fields for API consumers.
type FlatView struct {
	Type string `json:"type"`
	store.Flat
	PrivateURL string `json:"private_url"`
	// PrivateState is the private host's state once it is served: "ready"
	// when the URL answers, "starting" while the node joins the tailnet or
	// waits for its HTTPS certificate (PrivateDetail says which).
	PrivateState          string         `json:"private_state,omitempty"`
	PrivateDetail         string         `json:"private_detail,omitempty"`
	PublicURL             string         `json:"public_url,omitempty"`
	PublicNotice          string         `json:"public_notice,omitempty"`
	Live                  *store.Version `json:"live,omitempty"`
	Versions              int            `json:"versions"`
	EffectiveVersionQuota int            `json:"effective_version_quota"`
	VersionCount          int            `json:"version_count"`
	NextVersion           int            `json:"next_version"`
	LatestVersion         int            `json:"latest_version,omitempty"`
	// LatestDeployed reports whether LatestVersion was ever made live. False
	// means the newest saved version still waits to be published.
	LatestDeployed bool   `json:"latest_deployed,omitempty"`
	DiskBytes      int64  `json:"disk_bytes"`
	Thumbnail      string `json:"thumbnail,omitempty"`
	// Running reports a logical serving instance on this host. Documents
	// retain it while asleep or lazily restored before their first wake;
	// physical wake errors are reported on requests and in runtime events.
	Running bool `json:"running"`
	// Publication is unpublished until the first successful activation, then
	// published. It does not follow connection state.
	Publication     string             `json:"publication"`
	Draft           *store.Draft       `json:"draft,omitempty"`
	Providers       []string           `json:"providers,omitempty"`
	ConnectionState string             `json:"connection_state,omitempty"`
	Endpoints       []ExposureEndpoint `json:"endpoints"`
	// PortalListing is the flat's own relay listing choice: default (follow
	// the host's portal_hide setting), hidden or listed. PortalHidden is the
	// resolved choice, the listing Flats asks the relays for. It is not the
	// relays' observed state: they apply a change at their next lease
	// renewal, and a failed apply leaves the served route as it was. Neither
	// is access control.
	PortalListing string `json:"portal_listing"`
	PortalHidden  bool   `json:"portal_hidden"`
}

// UnlistedNotice is attached to every public-unlisted response.
const UnlistedNotice = "Unlisted only hides this flat from Portal relay listings. It is NOT access control: anyone with the URL can open it."

// ListedNotice is attached to every public-listed response.
const ListedNotice = "This flat is public: anyone can open it, and it appears in Portal relay listings."

// PublicAccessNotice is attached to a flat whose visibility is public.
const PublicAccessNotice = "This flat is public: anyone on the internet can open it. A domain or URL is not what makes it public."

// PendingPublicAccessNotice describes the effect of a visibility request that
// has not yet received the operator's explicit approval.
const PendingPublicAccessNotice = "If approved, this flat becomes Public: anyone on the internet can open it. A domain or URL is not what makes it public."

func (s *Service) view(ctx context.Context, f store.Flat) FlatView {
	v := FlatView{Flat: f, PrivateURL: s.cfg.Private.URL(f.Slug)}
	v.EffectiveVersionQuota = s.versionQuota(f)
	v.NextVersion, _ = s.st.NextVersionNumber(ctx, f.Slug)
	s.mu.Lock()
	lf := s.live[f.Slug]
	s.mu.Unlock()
	v.Running = lf != nil && lf.cur.Load() != nil
	v.PrivateState, v.PrivateDetail = s.hostState(f.Slug)
	if f.Visibility.Public() {
		v.PublicNotice = PublicAccessNotice
	}
	v.PortalListing = store.ListingDefault
	if mode, err := s.st.PortalListing(ctx, f.Slug); err == nil {
		v.PortalListing = mode
	}
	v.PortalHidden = s.listingHidden(v.PortalListing)
	if f.Visibility.Canonical().Public() && s.cfg.Public != nil {
		if ok, _ := s.st.ProviderPermitted(ctx, f.Slug, store.ProviderPortal); ok {
			v.PublicURL = s.cfg.Public.URL(f.Slug)
			v.ConnectionState = "unavailable"
			for _, host := range s.cfg.Public.Status().Hosts {
				if host.Host == f.Slug || (v.PublicURL != "" && host.URL == v.PublicURL) {
					v.ConnectionState = host.State
					break
				}
			}
			v.PublicNotice = PublicAccessNotice
		}
	}
	if f.LiveVersion > 0 {
		v.Publication = "published"
	} else if ok, err := s.st.HasDeployment(ctx, f.Slug); err == nil && ok {
		// A saved but never deployed version does not publish the flat.
		v.Publication = "published"
	} else {
		v.Publication = "unpublished"
	}
	if d, err := s.st.GetDraft(ctx, f.Slug); err == nil {
		v.Draft = &d
	}
	if ps, err := s.st.PermittedProviders(ctx, f.Slug); err == nil {
		v.Providers = ps
	}
	if observer, ok := s.cfg.Lifecycle.(LifecycleObserver); ok {
		// The configured legacy Private backend may be unpermitted. URLs and
		// readiness come from the current routes the Manager actually registered.
		v.PrivateURL, v.PrivateState, v.PrivateDetail = "", "", ""
		if f.Visibility.Public() {
			v.ConnectionState = "unavailable"
		}
		if res, err := observer.ExposureStatus(ctx, f.Slug); err == nil {
			privateFrom := ProviderID("")
			v.Endpoints = res.Endpoints
			for i := range v.Endpoints {
				ep := &v.Endpoints[i]
				ep.Permitted = ep.Permitted && slices.Contains(v.Providers, string(ep.Provider))
				if ep.Audience != AudienceCurrent || ep.Host != f.Slug {
					continue
				}
				if (ep.Provider == ProviderLocal || PrivateMeshProvider(ep.Provider)) && ep.Permitted && ep.URL != "" {
					if v.PrivateURL == "" || privateRank(ep.Provider) >= privateRank(privateFrom) {
						v.PrivateURL, v.PrivateState, v.PrivateDetail = ep.URL, ep.State, ep.Detail
						privateFrom = ep.Provider
					}
				}
				if PublicProvider(ep.Provider) && f.Visibility.Public() && ep.Permitted {
					if v.ConnectionState == "unavailable" || ep.Ready {
						v.PublicURL, v.ConnectionState = ep.URL, ep.State
					}
				}
			}
		}
	}

	if v.Endpoints == nil {
		v.Endpoints = []ExposureEndpoint{}
	}
	if v.ConnectionState == "" {
		v.ConnectionState = v.PrivateState
	}
	if v.ConnectionState == "" && f.LiveVersion > 0 {
		v.ConnectionState = "unavailable"
	}
	var latestManifest json.RawMessage
	if vs, err := s.st.ListVersions(ctx, f.Slug); err == nil {
		v.Versions = len(vs)
		for _, version := range vs {
			if !version.Pruned {
				v.VersionCount++
			}
		}
		if len(vs) > 0 {
			v.LatestVersion = vs[0].Number
			latestManifest = vs[0].Manifest
			v.LatestDeployed = vs[0].Number == f.LiveVersion
			if !v.LatestDeployed {
				if deployed, err := s.st.DeployedVersions(ctx, f.Slug); err == nil {
					v.LatestDeployed = deployed[vs[0].Number]
				}
			}
		}
		for i := range vs {
			if vs[i].Number == f.LiveVersion {
				lv := vs[i]
				lv.Deployed = true // the live version is deployed by definition
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
	// The running version decides the type, as in isDocs; the latest saved
	// version or Draft while nothing runs.
	v.Type = contenttype.Flat
	if v.Live != nil {
		v.Type = contenttype.FromManifest(v.Live.Manifest)
	} else if len(latestManifest) > 0 {
		v.Type = contenttype.FromManifest(latestManifest)
	} else if v.Draft != nil {
		v.Type = contenttype.FromManifest(v.Draft.Manifest)
	}
	if v.Type == contenttype.Design || v.Type == contenttype.Board {
		// A design or board runs from an internal shell version published
		// at creation; it has no versions, Draft or publishing to report.
		v.LiveVersion, v.Live, v.Versions, v.VersionCount = 0, nil, 0, 0
		v.NextVersion, v.LatestVersion, v.LatestDeployed = 0, 0, false
		v.Draft, v.Publication, v.EffectiveVersionQuota = nil, "", 0
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
	unlock := s.lock(slugName)
	defer unlock()
	return s.createFlatLocked(ctx, slugName, name, via)
}

// createFlatLocked creates a flat while the caller holds slugName's operation
// lock. The shared lock keeps an expired redirect's retry reservation atomic
// with both its provider teardown and a competing slug reuse.
func (s *Service) createFlatLocked(ctx context.Context, slugName, name string, via Via) (FlatView, error) {
	if via == ViaConsole {
		return FlatView{}, forbiddenf("flats are created by agents (MCP, CLI or API), not from the console")
	}
	if err := s.checkReserved(ctx, slugName, ""); err != nil {
		return FlatView{}, err
	}
	// An existing flat owns its directory: leave that case to the "already
	// exists" conflict below.
	if _, err := s.st.GetFlat(ctx, slugName); errors.Is(err, store.ErrNotFound) {
		if err := s.requireUnusedStorage(slugName); err != nil {
			return FlatView{}, err
		}
	} else if err != nil {
		return FlatView{}, err
	}
	if name == "" {
		name = slugName
	}
	now := s.now()
	f := store.Flat{Slug: slugName, Name: name, Visibility: store.Private, CreatedAt: now, UpdatedAt: now}
	// A new flat inherits the host's default providers: the private default
	// (network.default_private, else network.private_backend) and the public
	// default (network.default_public). It stays Private either way: a public
	// grant is only a permission and opens nothing until an approved
	// visibility change. The flat, the grants and the events recording them
	// are written together.
	var providers []string
	events := []store.Event{{Time: now, Level: "info", Kind: "flat", Message: "flat created via " + string(via)}}
	for _, d := range s.newFlatProviders() {
		providers = append(providers, d.provider)
		events = append(events, store.Event{Time: now, Level: "info", Kind: "provider",
			Message: fmt.Sprintf("provider %s permitted=true by default (%s)", d.provider, d.why)})
	}
	if err := s.st.CreateFlatRecorded(ctx, f, providers, events); err != nil {
		if errors.Is(err, store.ErrExists) {
			return FlatView{}, withKind(ErrConflict, err)
		}
		return FlatView{}, err
	}
	s.state(slugName)
	return s.view(ctx, f), nil
}

type defaultGrant struct{ provider, why string }

// newFlatProviders returns the providers a new flat is allowed on from the
// start. A configured default that the host no longer permits is skipped.
func (s *Service) newFlatProviders() []defaultGrant {
	n := s.settings.network()
	permitted := s.settings.permitted()
	var out []defaultGrant
	switch {
	case n.DefaultPrivate == store.ProviderTailscale && slices.Contains(permitted, store.ProviderTailscale):
		out = append(out, defaultGrant{store.ProviderTailscale, "network.default_private is tailscale"})
	case n.DefaultPrivate == store.ProviderNetbird && slices.Contains(permitted, store.ProviderNetbird):
		out = append(out, defaultGrant{store.ProviderNetbird, "network.default_private is netbird"})
	case n.DefaultPrivate == "" && s.cfg.PrivateBackend == "tailscale":
		out = append(out, defaultGrant{store.ProviderTailscale, "network.private_backend is tailscale"})
	}
	if p := n.DefaultPublic; p != "" && slices.Contains(permitted, p) {
		out = append(out, defaultGrant{p, "network.default_public is " + p})
	}
	return out
}

// DefaultProviders reports the providers new flats inherit, for the console.
func (s *Service) DefaultProviders() (private, public string) {
	n := s.settings.network()
	private = n.DefaultPrivate
	if private == "" {
		private = s.cfg.PrivateBackend
	}
	if private == "" {
		private = "local"
	}
	return private, n.DefaultPublic
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

// SaveMeta describes where a draft save came from.
type SaveMeta struct {
	GitSHA           string
	GitDirty         bool
	Message          string
	ExpectedRevision int
	CheckRevision    bool
	// quiet skips the "saved draft" event, for the internal design shell.
	quiet bool
	// historical writes a bundle rebuilt from a document revision, which
	// may exceed the upload-only Markdown total (bundle.WriteHistorical).
	historical bool
}

// SaveVersion validates files and stores them as the flat's draft.
// It never allocates a published version and never changes what is live.
// The flat is created when it does not exist yet.
func (s *Service) SaveVersion(ctx context.Context, slugName string, files []bundle.File, meta SaveMeta, via Via) (store.Version, error) {
	if err := slug.Validate(slugName); err != nil {
		return store.Version{}, invalid(err)
	}
	unlock, err := s.lockStorage(slugName)
	if err != nil {
		return store.Version{}, err
	}
	defer unlock()
	return s.saveVersionLocked(ctx, slugName, files, meta, via)
}

// saveVersionLocked validates and saves a complete bundle while the caller
// holds the flat's storage lock. Partial document saves assemble their source
// bundle under that same lock, so simultaneous saves cannot lose sibling files.
func (s *Service) saveVersionLocked(ctx context.Context, slugName string, files []bundle.File, meta SaveMeta, via Via) (store.Version, error) {
	files, err := bundle.FromFiles(files, bundle.Limits{MaxBytes: s.UploadLimit()})
	if err != nil {
		return store.Version{}, err
	}
	m, err := bundle.ParseManifest(files)
	if err != nil {
		return store.Version{}, err
	}
	if m.Kind == "server" && s.cfg.Runtime == nil {
		return store.Version{}, &bundle.ValidationError{Problems: []bundle.Problem{{Path: bundle.ManifestName, Message: "server flats are not enabled on this host", Fix: `deploy a static build ("kind": "static")`}}}
	}
	switch m.Type {
	case contenttype.Design:
		return store.Version{}, ErrDesignUnversioned
	case contenttype.Board:
		return store.Version{}, ErrBoardUnversioned
	}
	docs := m.Type == contenttype.Docs
	if f, err := s.st.GetFlat(ctx, slugName); errors.Is(err, store.ErrNotFound) {
		if _, err := s.createFlatLocked(ctx, slugName, m.Name, via); err != nil && !errors.Is(err, store.ErrExists) {
			return store.Version{}, err
		}
	} else if err != nil {
		return store.Version{}, err
	} else if err := s.checkTypeChange(ctx, f, m.Type); err != nil {
		return store.Version{}, err
	}
	var size int64
	for _, f := range files {
		size += int64(len(f.Data))
	}
	if quota := s.diskQuota(); quota > 0 {
		if used := dirSize(s.flatDir(slugName)); used+size > quota {
			fix := "delete old versions/data, or raise the per-flat disk quota in system settings"
			if docs {
				fix = documentDiskQuotaFix
			}
			return store.Version{}, &bundle.ValidationError{Problems: []bundle.Problem{{Message: fmt.Sprintf("flat would use %d bytes, over its %d-byte disk quota", used+size, quota), Fix: fix}}}
		}
	}
	v, err := s.saveDraftLocked(ctx, slugName, files, meta, via)
	if err != nil || !docs {
		return v, err
	}
	// A document has no publish step: its save runs at once, Private
	// unless the operator already approved Public. Its revision is recorded
	// by the new version's activation, labelled through the intent.
	if err := s.docsIntentLocked(ctx, slugName, v.Hash, agentLabel(via), meta.Message); err != nil {
		return v, err
	}
	err = s.applyDocsLocked(ctx, slugName, v.Revision, via)
	s.docsAfterWriteLocked(ctx, slugName)
	if err != nil {
		return v, err
	}
	return v, nil
}

// ListVersions returns versions newest first. A document has none.
func (s *Service) ListVersions(ctx context.Context, slugName string) ([]store.Version, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return nil, err
	}
	if err := s.refuseUnversioned(ctx, f); err != nil {
		return nil, err
	}
	vs, err := s.st.ListVersions(ctx, slugName)
	if err != nil {
		return nil, err
	}
	deployed, err := s.st.DeployedVersions(ctx, slugName)
	if err != nil {
		return nil, err
	}
	for i := range vs {
		vs[i].Deployed = deployed[vs[i].Number] || vs[i].Number == f.LiveVersion
	}
	return vs, nil
}

// GetVersion returns one version.
func (s *Service) GetVersion(ctx context.Context, slugName string, n int) (store.Version, error) {
	v, err := s.st.GetVersion(ctx, slugName, n)
	if err != nil {
		return v, err
	}
	if f, err := s.st.GetFlat(ctx, slugName); err == nil && f.LiveVersion == n {
		v.Deployed = true
	} else if deployed, err := s.st.DeployedVersions(ctx, slugName); err == nil {
		v.Deployed = deployed[n]
	}
	return v, nil
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
	prune, err := s.prunableVersions(ctx, slugName, true)
	if err != nil {
		return
	}
	for _, n := range prune {
		if err := os.RemoveAll(s.versionDir(slugName, n)); err == nil {
			_ = s.st.MarkPruned(ctx, slugName, n)
			s.Event(ctx, slugName, "info", "retention", fmt.Sprintf("pruned internal backing bundle %d (keeping the live and previous bundles)", n), nil)
		}
	}
}

// prunableVersions returns obsolete internal backing bundles for Revision-model
// flats. It retains the live bundle, the newest non-live bundle, and any pinned
// bundles. User-facing Version-model snapshots are never automatically pruned.
func (s *Service) prunableVersions(ctx context.Context, slugName string, keepPreviewed bool) ([]int, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return nil, err
	}
	// Documents and designs keep internal bundles for their running and
	// draining runtimes; their user-facing history uses Revisions.
	t, err := s.flatType(ctx, f)
	if err != nil {
		return nil, err
	}
	if !contenttype.RevisionModel(t) {
		return nil, nil
	}
	vs, err := s.st.ListVersions(ctx, slugName)
	if err != nil {
		return nil, err
	}
	inPreview := map[int]bool{}
	if keepPreviewed {
		if ps, err := s.st.ListPreviews(ctx, slugName); err == nil {
			for _, p := range ps {
				inPreview[p.Version] = true
			}
		}
	}
	// Preserve any historical approval pins conservatively even though new
	// Revision-model operations do not create these approvals.
	for _, st := range []string{"pending", "applying"} {
		if aps, err := s.st.ListApprovals(ctx, st); err == nil {
			for _, a := range aps {
				if a.Flat != slugName || (a.Action != "activate" && a.Action != "rollback" && a.Action != "restore_data") {
					continue
				}
				var p struct {
					Version int `json:"version"`
				}
				if json.Unmarshal(a.Params, &p) == nil && p.Version > 0 {
					inPreview[p.Version] = true
				}
			}
		}
	}
	var prune []int
	kept := 0
	for _, v := range vs { // newest first
		// Pinned bundles do not consume the one previous-bundle allowance.
		if v.Pruned || v.Number == f.LiveVersion || inPreview[v.Number] {
			continue
		}
		if kept < 1 {
			kept++
			continue
		}
		prune = append(prune, v.Number)
	}
	return prune, nil
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
	Flat       FlatView     `json:"flat"`
	Version    int          `json:"version"`
	Previous   int          `json:"previous"`
	Health     HealthResult `json:"health"`
	Millis     int64        `json:"millis"`
	DataImpact string       `json:"data_impact"`
	HealthData string       `json:"health_data"`
	LiveData   string       `json:"live_data"`
}

// DeployError carries the failed health check.
type DeployError struct {
	Committed bool // live commit succeeded; never delete/undo this version on error
	Version   int
	Previous  int // live version when the deploy started (0 = none)
	Health    HealthResult
	Cause     error
	// Data says what happened to the flat's data when the deploy may have
	// touched it.
	Data       string
	DataImpact string
	HealthData string
	LiveData   string
	// Document marks a saved document that could not start, which has no
	// version to name.
	Document bool
}

func (e *DeployError) Error() string {
	if e.Committed {
		return fmt.Sprintf("content saved and committed; serving initialization failed: %v. Do not repeat the save or create another flat; a later request retries serving. Inspect the flat logs for details", e.Cause)
	}
	if e.Document {
		tail := "(the document keeps running with its previous text)"
		if e.Previous == 0 {
			tail = "(the document is not running yet)"
		}
		if e.Cause != nil {
			return fmt.Sprintf("the saved document could not start: %v %s", e.Cause, tail)
		}
		return fmt.Sprintf("the saved document failed its health check: GET %s returned %d %s %s", e.Health.Path, e.Health.Status, e.Health.Error, tail)
	}
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
		req = trustedAccess(req, false)
		req.Header.Set("X-Flats-Health", "1")
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

// Deploy requests activation of a saved version (n > 0). Saving a draft as a
// new numbered version is a separate step via Publish. Nothing is activated
// until the operator approves the returned request.
func (s *Service) Deploy(ctx context.Context, slugName string, n int, via Via) (DeployResult, error) {
	return s.requestDeploy(ctx, slugName, n, via)
}

// deployLocked health-checks version n on a copy of the flat data, then
// starts it on the live data and makes it live. The check does not run
// against live data. The caller holds s.lock(f.Slug).
func (s *Service) deployLocked(ctx context.Context, f store.Flat, n int, kind string, via Via, approvalID string) (DeployResult, error) {
	start := time.Now()
	slugName := f.Slug
	v, err := s.st.GetVersion(ctx, slugName, n)
	if err != nil {
		return DeployResult{}, err
	}
	lifecyclePhase(ctx, "before_health", slugName, approvalID)
	env, err := s.captureEnvironment(ctx, slugName, v)
	if err != nil {
		return DeployResult{}, &DeployError{Version: v.Number, Previous: f.LiveVersion, Cause: err, Data: dataUntouched, DataImpact: "none", HealthData: "not_run", LiveData: "untouched"}
	}
	h, err := s.checkIsolated(ctx, f, v, env)
	if err != nil {
		return DeployResult{}, err
	}
	lifecyclePhase(ctx, "after_health", slugName, approvalID)
	if snap, serr := s.snapshotDB(slugName, n, f.LiveVersion); serr != nil {
		return DeployResult{}, &DeployError{Version: n, Previous: f.LiveVersion, Cause: fmt.Errorf("%w: %w", ErrSnapshotFailed, serr), Data: dataIsolated, DataImpact: "none", HealthData: "isolated_copy", LiveData: "untouched"}
	} else if snap != "" {
		s.Event(ctx, slugName, "info", "snapshot", fmt.Sprintf("saved database snapshot %s before deploying version %d", snap, n), nil)
	}
	lf := s.state(slugName)
	lf.bindUID(f.ID)
	d, err := s.buildWithEnvironment(ctx, slugName, v, s.dataDirOf(slugName), env, lf.liveSink())
	if err != nil {
		s.Event(ctx, slugName, "error", "deploy", fmt.Sprintf("%s of version %d failed to start: %v", kind, n, err), nil)
		return DeployResult{}, &DeployError{Version: n, Previous: f.LiveVersion, Cause: err, Data: "Runtime startup used live data and may have changed it.", DataImpact: "runtime_start", HealthData: "isolated_copy", LiveData: "runtime_may_write"}
	}
	return s.activate(ctx, f, d, h, kind, via, approvalID, start)
}

// activate makes the started and checked d live. The caller holds
// s.lock(f.Slug).
func (s *Service) activate(ctx context.Context, f store.Flat, d *deployed, h HealthResult, kind string, via Via, approvalID string, start time.Time) (DeployResult, error) {
	slugName, n, prev := f.Slug, d.version.Number, f.LiveVersion
	var commitErr error
	if kind == "publish" || kind == "save" {
		commitErr = s.st.CommitPublished(ctx, d.version, prev, approvalID, s.now())
	} else {
		commitErr = s.st.SetLive(ctx, slugName, n, prev, kind, approvalID, s.now())
	}
	if err := commitErr; err != nil {
		if d.inst != nil {
			d.inst.Stop()
		}
		if d.version.Kind == "server" {
			return DeployResult{}, &DeployError{Version: n, Previous: prev, Cause: err, Data: "Live runtime startup may have changed data.", DataImpact: "runtime_start", HealthData: "isolated_copy", LiveData: "runtime_may_write"}
		}
		return DeployResult{}, err
	}
	// From here the committed live version must survive caller cancellation.
	ctx = context.WithoutCancel(ctx)
	lf := s.state(slugName)
	lf.bindUID(f.ID)
	if d.version.Kind != "server" {
		s.retireRuntime(s.dataDirOf(slugName))
	}
	old := lf.cur.Swap(d)
	if control := docsManaged(old); control != nil {
		control.Retire()
	}
	if old != nil && old.inst != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C: // drain in-flight requests
			case <-s.stop: // shutdown must stop every instance before releasing data ownership
			}
			old.inst.Stop()
		}()
	}
	var preparationErr error
	if docsManaged(d) != nil {
		_, release, err := s.acquireDocsLocked(ctx, slugName, d)
		release()
		preparationErr = err
	}
	f.LiveVersion = n
	if err := s.ensureExposure(ctx, f); err != nil {
		s.Event(ctx, slugName, "error", "exposure", err.Error(), nil)
	}
	_ = s.dropPreviews(ctx, slugName)
	if t := contenttype.FromManifest(d.version.Manifest); kind == "save" && (t == contenttype.Design || t == contenttype.Board) {
		// The design or board shell goes live once, at creation; that is
		// logged as a design or board event.
	} else if kind == "save" {
		message := fmt.Sprintf("document saved and running via %s", via)
		if preparationErr != nil {
			message = fmt.Sprintf("document saved via %s; serving initialization will retry", via)
		}
		s.Event(ctx, slugName, "info", "deploy", message, map[string]int{"version": n, "previous": prev})
	} else {
		s.Event(ctx, slugName, "info", "deploy", fmt.Sprintf("%s: version %d is live (was %d) via %s", kind, n, prev, via), map[string]int{"version": n, "previous": prev})
	}
	s.pruneVersions(ctx, slugName)
	fv, _ := s.st.GetFlat(ctx, slugName)
	impact, live := "none", "untouched"
	if d.version.Kind == "server" {
		impact, live = "runtime_start", "runtime_may_write"
	}
	result := DeployResult{Flat: s.view(ctx, fv), Version: n, Previous: prev, Health: h, Millis: time.Since(start).Milliseconds(), DataImpact: impact, HealthData: "isolated_copy", LiveData: live}
	if preparationErr != nil {
		return result, &DeployError{Committed: true, Version: n, Previous: prev, Cause: preparationErr, Data: "Saved content remains committed; a later request retries serving.", DataImpact: impact, HealthData: "isolated_copy", LiveData: live}
	}
	return result, nil
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
	return s.requestRollback(ctx, slugName, to, restoreData, via)
}

// rollbackLocked applies an already approved rollback. The caller holds s.lock.
func (s *Service) rollbackLocked(ctx context.Context, f store.Flat, to int, restoreData bool, via Via, approvalID string) (DeployResult, error) {
	if f.LiveVersion == 0 {
		return DeployResult{}, ErrNotDeployed
	}
	if to == 0 {
		var err error
		if to, err = s.previousLive(ctx, f); err != nil {
			return DeployResult{}, err
		}
	}
	if to == f.LiveVersion {
		return DeployResult{}, fmt.Errorf("%w: version %d is already live", ErrConflict, to)
	}
	if !restoreData {
		return s.deployLocked(ctx, f, to, "rollback", via, approvalID)
	}
	snap, err := s.liveSnapshot(f.Slug, f.LiveVersion)
	if err != nil {
		return DeployResult{}, err
	}
	return s.restoreLocked(ctx, f, snap, to, "rollback", via, approvalID)
}

// previousLive returns the version that was live before the current one.
func (s *Service) previousLive(ctx context.Context, f store.Flat) (int, error) {
	n, err := s.st.PreviousAvailableDeployment(ctx, f.Slug, f.LiveVersion)
	if errors.Is(err, store.ErrNotFound) {
		return 0, fmt.Errorf("%w: there is no available earlier deployed version to roll back to", ErrConflict)
	}
	return n, err
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
	v, err := s.st.GetVersion(ctx, slugName, f.LiveVersion)
	if err != nil {
		return DeployResult{}, err
	}
	key, err := s.providerKey(ctx, slugName)
	if err != nil {
		return DeployResult{}, err
	}
	hash, err := snapshotHash(filepath.Join(s.snapshotDir(slugName), name))
	if err != nil {
		return DeployResult{}, err
	}
	res, err := s.requestFrozen(ctx, slugName, "restore_data", rollbackParams{Version: f.LiveVersion, ExpectedLive: f.LiveVersion, RestoreData: true, Visibility: string(f.Visibility.Canonical()), Providers: key, Hash: v.Hash, Snapshot: name, SnapshotHash: hash}, via, "")
	if err != nil {
		return DeployResult{}, err
	}
	return pending(res)
}

// restoreLocked replaces the flat's database with snapshot snap and makes
// version to live on it. The caller holds s.lock(f.Slug).
//
// The target is first started and health-checked on a scratch copy holding
// the snapshot, so a broken or missing target never touches live data. Then
// the live version is stopped, the current database is saved as a
// before-restore snapshot, and the snapshot is installed. If anything fails
// after that, the saved database is put back and the old version restarted.
func (s *Service) restoreLocked(ctx context.Context, f store.Flat, snap string, to int, kind string, via Via, approvalID string) (DeployResult, error) {
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
	env, err := s.captureEnvironment(ctx, slugName, v)
	if err != nil {
		return DeployResult{}, &DeployError{Version: v.Number, Previous: f.LiveVersion, Cause: err, Data: dataUntouched, DataImpact: "none", HealthData: "not_run", LiveData: "untouched"}
	}
	if err := s.trialRun(ctx, f, v, snapPath, env); err != nil {
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
			Data:  joinNotes("Nothing was changed", rerr), DataImpact: "unknown", HealthData: "isolated_copy", LiveData: "unknown"}
	}
	journal := filepath.Join(s.flatDir(slugName), "restore-journal.json")
	raw, _ := json.Marshal(restoreJournal{Approval: approvalID, Backup: backup})
	if err := atomicJournal(journal, raw); err != nil {
		_ = s.restart(ctx, slugName, old)
		return DeployResult{}, err
	}
	defer os.Remove(journal)
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
		if err := s.installSnapshot(slugName, src); err != nil {
			note = fmt.Sprintf("Putting the database back failed (%v); the data from before the restore is in snapshot %s", err, backup)
		}
		return joinNotes(note, s.restart(ctx, slugName, old))
	}
	fail := func(de *DeployError) (DeployResult, error) {
		de.Version, de.Previous, de.Data = to, f.LiveVersion, undo()
		de.DataImpact, de.HealthData, de.LiveData = "unknown", "isolated_copy", "unknown"
		s.Event(ctx, slugName, "error", "snapshot", fmt.Sprintf("restoring snapshot %s failed: %v", snap, de), nil)
		return DeployResult{}, de
	}
	if err := s.installSnapshot(slugName, snapPath); err != nil {
		return fail(&DeployError{Cause: fmt.Errorf("install snapshot %s: %w", snap, err)})
	}
	lf.bindUID(f.ID)
	d, err := s.buildWithEnvironment(ctx, slugName, v, s.dataDirOf(slugName), env, lf.liveSink())
	if err != nil {
		return fail(&DeployError{Cause: err})
	}
	h := HealthResult{OK: true, Path: manifestOf(v).Health}
	msg := fmt.Sprintf("restored database snapshot %s", snap)
	if backup != "" {
		msg += fmt.Sprintf("; the database from before is saved as snapshot %s", backup)
	}
	res, err := s.activate(ctx, f, d, h, kind, via, approvalID, start)
	if err != nil {
		var committed *DeployError
		if errors.As(err, &committed) && committed.Committed {
			return res, err
		}
		return fail(&DeployError{Cause: err})
	}
	res.DataImpact, res.LiveData = "restore_data", "restored"
	s.Event(ctx, slugName, "warn", "snapshot", msg, nil)
	return res, nil
}

// trialRun starts version v on a scratch copy of the data holding the
// snapshot at snapPath and health-checks it.
func (s *Service) trialRun(ctx context.Context, f store.Flat, v store.Version, snapPath string, env *runtimeEnvironment) error {
	dir := filepath.Join(s.flatDir(f.Slug), "restore-trial")
	os.RemoveAll(dir)
	defer os.RemoveAll(dir)
	untouched := "The data was not touched"
	if v.Kind == "server" {
		source := s.dataDirOf(f.Slug)
		if _, err := os.Stat(filepath.Join(snapPath+".data", ".flats-snapshot-complete")); err == nil {
			source = snapPath + ".data"
		}
		if err := snapshotFiles(source, dir); err != nil {
			return &DeployError{Cause: err, Data: untouched}
		}
		dbSource := snapPath
		if source == snapPath+".data" {
			dbSource = filepath.Join(source, dbName)
		}
		if _, err := os.Stat(dbSource); err == nil {
			if err := copyFile(dbSource, filepath.Join(dir, dbName)); err != nil {
				return &DeployError{Cause: err, Data: untouched}
			}
		} else if !os.IsNotExist(err) {
			return &DeployError{Cause: err, Data: untouched}
		}
	}
	d, err := s.buildWithEnvironment(ctx, f.Slug, v, dir, env, s.state(f.Slug).checkSink())
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
	lf := s.state(slugName)
	if old.inst != nil {
		var err error
		if d, err = s.buildWithEnvironment(context.WithValue(ctx, lazyDocsKey{}, true), slugName, old.version, s.dataDirOf(slugName), old.environment, lf.liveSink()); err != nil {
			s.Event(ctx, slugName, "error", "deploy", fmt.Sprintf("could not restart version %d: %v", old.version.Number, err), nil)
			return fmt.Errorf("version %d could not be restarted (%v); the flat answers 503 until a version is deployed", old.version.Number, err)
		}
	}
	lf.cur.Store(d)
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
	Status         string          `json:"status"` // done | pending_approval | cleanup_pending
	CleanupPending bool            `json:"cleanup_pending,omitempty"`
	Approval       *store.Approval `json:"approval,omitempty"`
	ApprovalURL    string          `json:"approval_url,omitempty"`
	Flat           *FlatView       `json:"flat,omitempty"`
	Version        int             `json:"version,omitempty"`
	Notice         string          `json:"notice,omitempty"`
	Message        string          `json:"message"`
	DataImpact     string          `json:"data_impact,omitempty"`
	HealthData     string          `json:"health_data,omitempty"`
	LiveData       string          `json:"live_data,omitempty"`
}

func (s *Service) requestApproval(ctx context.Context, slugName, action string, params any, via Via, reason string) (ActionResult, error) {
	return s.requestFrozen(ctx, slugName, action, params, via, reason)
}

// ApprovalURL is the console address where the operator decides approval
// id, on the console domain the request came through when there is one.
func (s *Service) ApprovalURL(ctx context.Context, id string) string {
	base := ""
	if s.cfg.ConsoleURLFor != nil {
		base = s.cfg.ConsoleURLFor(ctx)
	} else {
		base = s.cfg.ConsoleURL()
	}
	return strings.TrimSuffix(base, "/") + "/approvals/" + id
}

// SetVisibility requests a private or public change. Both directions, from
// every surface including the console, wait for approval. Legacy listed and
// unlisted values are accepted and stored as public. Nothing is applied here.
func (s *Service) SetVisibility(ctx context.Context, slugName string, vis store.Visibility, via Via, reason string) (ActionResult, error) {
	return s.requestVisibility(ctx, slugName, vis, via, reason)
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
	if err := slug.Validate(slugName); err != nil {
		return ActionResult{}, invalid(err)
	}
	unlock := s.lock(slugName)
	defer unlock()
	return s.deleteLocked(ctx, slugName, via, exceptApproval)
}

// deleteLocked deletes a flat; the caller holds its lock.
func (s *Service) deleteLocked(ctx context.Context, slugName string, via Via, exceptApproval string) (ActionResult, error) {
	flat, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return ActionResult{}, err
	}
	type alias struct {
		old, cur string
		restore  bool
	}
	var aliases []alias
	s.mu.Lock()
	for old, r := range s.redir {
		if r.cur == slugName {
			aliases = append(aliases, alias{old: old, cur: r.cur, restore: !r.teardownOnly && !r.retired})
		}
	}
	s.mu.Unlock()
	slices.SortFunc(aliases, func(a, b alias) int { return strings.Compare(a.old, b.old) })

	// Previews and aliases can fail independently of the current route. Prepare
	// their network teardown while keeping their rows, runtimes and data intact,
	// so any later failure can restore every route of the retained flat.
	previews, err := s.prepareDeletePreviews(ctx, slugName)
	if err != nil {
		return ActionResult{}, fmt.Errorf("stop previews: %w", err)
	}
	stoppedAliases := make([]alias, 0, len(aliases))
	restoreAliases := func() {
		restoreCtx := context.WithoutCancel(ctx)
		for _, a := range stoppedAliases {
			if a.restore {
				s.serveRedirect(restoreCtx, a.old, a.cur)
			}
		}
	}
	for _, a := range aliases {
		if err := s.stopRedirect(a.old); err != nil {
			restoreAliases()
			restoreErr := s.restoreDeletePreviews(context.WithoutCancel(ctx), previews)
			return ActionResult{}, errors.Join(err, restoreErr)
		}
		stoppedAliases = append(stoppedAliases, a)
	}
	if err := s.stopSlugRoutes(ctx, slugName); err != nil {
		restoreAliases()
		restoreErr := s.restoreDeletePreviews(context.WithoutCancel(ctx), previews)
		return ActionResult{}, errors.Join(err, restoreErr)
	}
	op := storageOperation{Kind: "delete", From: slugName}
	if err := s.beginStorageOperation(op); err != nil {
		restoreCtx := context.WithoutCancel(ctx)
		restoreErr := s.ensureExposure(restoreCtx, flat)
		restoreAliases()
		restoreErr = errors.Join(restoreErr, s.restoreDeletePreviews(restoreCtx, previews))
		return ActionResult{}, errors.Join(err, restoreErr)
	}
	if err := s.st.DeleteFlat(ctx, slugName); err != nil {
		journalErr := s.finishStorageOperation(op)
		restoreCtx := context.WithoutCancel(ctx)
		restoreErr := s.ensureExposure(restoreCtx, flat)
		restoreAliases()
		restoreErr = errors.Join(restoreErr, s.restoreDeletePreviews(restoreCtx, previews))
		return ActionResult{}, errors.Join(err, journalErr, restoreErr)
	}
	s.mu.Lock()
	lf := s.live[slugName]
	delete(s.live, slugName)
	for _, a := range aliases {
		delete(s.redir, a.old)
	}
	for _, p := range previews {
		delete(s.prevs, p.record.Host)
	}
	s.mu.Unlock()
	for _, p := range previews {
		if p.live != nil {
			if p.live.inst != nil {
				p.live.inst.Stop()
			}
			_ = os.RemoveAll(p.live.dataDir)
		}
	}
	if lf != nil {
		if d := lf.cur.Load(); d != nil && d.inst != nil {
			stopInstance(d.inst, "deleted")
		}
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
	if err := s.discardDeletedStorage(context.WithoutCancel(ctx), op); err != nil {
		return ActionResult{}, fmt.Errorf("flat %q metadata deleted but storage cleanup is incomplete: %w", slugName, err)
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

type actorKey struct{}

// WithActor records who decides an approval in the console (for example a
// tailnet login); Decide stores it as decided_by. Without one it is "console".
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// Decide approves or rejects a pending approval. Only the console calls it.
// The approval is claimed before anything is applied, so of two concurrent
// decisions exactly one acts and the other gets ErrConflict.
func (s *Service) Decide(ctx context.Context, id string, approve bool) (store.Approval, error) {
	unlock := s.lock("approval:" + id)
	defer unlock()
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
	actor := "console"
	if who, _ := ctx.Value(actorKey{}).(string); who != "" {
		actor = who
	}
	if !approve {
		if a.Status == "rejected" {
			return a, nil
		}
		if a.Status != "pending" {
			return lost(nil)
		}
		if err := s.st.RejectApprovalAuthorized(ctx, id, actor, "rejected by the operator", s.now()); err != nil {
			return lost(err)
		}
		s.Event(ctx, a.Flat, "info", "approval", a.Action+" rejected by the operator", nil)
		return s.st.GetApproval(ctx, id)
	}
	switch a.Status {
	case "approved":
		return a, nil
	case "applying":
		return lost(nil)
	case "pending":
		if err := s.st.ClaimApprovalAuthorized(ctx, id, actor, s.now()); err != nil {
			return lost(err)
		}
		a, err = s.st.GetApproval(ctx, id)
		if err != nil {
			return a, err
		}
		return s.finishApplying(ctx, a)
	default:
		return lost(nil)
	}
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

// previewView selects a currently registered Private route. After a per-flat
// Tailscale revocation, a surviving Local preview must not advertise a dead URL.
func (s *Service) previewView(ctx context.Context, p store.Preview) PreviewView {
	pv := PreviewView{Preview: p, URL: s.cfg.Private.URL(p.Host), ExpiresAt: p.LastAccess.Add(s.previewTTL())}
	pv.State, pv.Detail = s.hostState(p.Host)
	if observer, ok := s.cfg.Lifecycle.(LifecycleObserver); ok {
		pv.URL, pv.State, pv.Detail = "", "unavailable", "route not reported"
		providers, err := s.st.PermittedProviders(ctx, p.Flat)
		if err != nil {
			return pv
		}
		res, err := observer.ExposureStatus(ctx, p.Flat)
		if err != nil {
			return pv
		}
		for _, ep := range res.Endpoints {
			if ep.Host != p.Host || ep.Audience != AudienceDraft || !ep.Configured || !ep.Permitted || ep.URL == "" || !slices.Contains(providers, string(ep.Provider)) {
				continue
			}
			if ep.Provider != ProviderLocal && ep.Provider != ProviderTailscale {
				continue
			}
			if pv.URL == "" || ep.Provider == ProviderTailscale {
				pv.URL, pv.State, pv.Detail = ep.URL, ep.State, ep.Detail
			}
		}
	}
	return pv
}

// maxPreviews bounds open previews per flat: each runs its own node and,
// for server flats, holds a copy of the data.
const maxPreviews = 5

// OpenPreview serves a saved version at a new random private address.
func (s *Service) OpenPreview(ctx context.Context, slugName string, n int) (PreviewView, error) {
	unlock, err := s.lockStorage(slugName)
	if err != nil {
		return PreviewView{}, err
	}
	defer unlock()
	if f, err := s.st.GetFlat(ctx, slugName); err != nil {
		return PreviewView{}, err
	} else if err := s.refuseUnversioned(ctx, f); err != nil {
		return PreviewView{}, err
	}
	if n == 0 {
		return s.openDraftPreviewLocked(ctx, slugName)
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
	host, release := s.previewHost(slugName)
	defer release()
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
	d, err := s.build(ctx, slugName, v, dataDir, s.state(slugName).previewSink())
	if err != nil {
		os.RemoveAll(dataDir)
		return PreviewView{}, err
	}
	p := &preview{host: host, flat: slugName, version: n, handler: d.handler, inst: d.inst, dataDir: dataDir}
	now := s.now()
	p.last.Store(now.UnixMilli())
	h := s.previewHandler(p)
	stop := func() {
		if d.inst != nil {
			d.inst.Stop()
		}
		os.RemoveAll(dataDir)
	}
	url, err := s.servePreview(ctx, slugName, host, h)
	if err != nil {
		stop()
		return PreviewView{}, err
	}
	rec := store.Preview{Host: host, Flat: slugName, Version: n, CreatedAt: now, LastAccess: now, Target: "version"}
	if err := s.st.InsertPreview(ctx, rec); err != nil {
		_ = s.stopPreviewExposure(ctx, host)
		stop()
		return PreviewView{}, err
	}
	s.mu.Lock()
	s.prevs[host] = p
	s.mu.Unlock()
	s.Event(ctx, slugName, "info", "preview", fmt.Sprintf("preview of version %d at %s", n, url), nil)
	return s.previewView(ctx, rec), nil
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
		out = append(out, s.previewView(ctx, p))
	}
	return out, nil
}

// ClosePreview removes one preview. Hosts that are not open previews (a
// flat, the console, a redirect) are never touched: it returns
// store.ErrNotFound for them.
func (s *Service) ClosePreview(ctx context.Context, host string) error {
	unlock := s.lock("preview:" + host)
	defer unlock()
	s.mu.Lock()
	p := s.prevs[host]
	s.mu.Unlock()
	if p == nil {
		// A preview row without a running preview (e.g. a half-failed open)
		// is still a preview host.
		if _, err := s.st.GetPreview(ctx, host); err != nil {
			return err
		}
	}
	if err := s.stopPreviewExposure(ctx, host); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.prevs, host)
	s.mu.Unlock()
	if p != nil {
		if p.inst != nil {
			p.inst.Stop()
		}
		os.RemoveAll(p.dataDir)
	}
	return s.st.DeletePreview(ctx, host)
}

func (s *Service) dropPreviews(ctx context.Context, slugName string) error {
	ps, err := s.st.ListPreviews(ctx, slugName)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range ps {
		if err := s.ClosePreview(ctx, p.Host); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Host, err))
			continue
		}
		s.Event(ctx, slugName, "info", "preview", "closed preview "+p.Host, nil)
	}
	return errors.Join(errs...)
}

type deletePreview struct {
	record store.Preview
	live   *preview
}

// prepareDeletePreviews confirms that every preview route can stop without
// deleting its row, runtime or data. A later alias/current failure can then
// restore the stopped routes and leave the flat fully usable for retry.
func (s *Service) prepareDeletePreviews(ctx context.Context, slugName string) ([]deletePreview, error) {
	rows, err := s.st.ListPreviews(ctx, slugName)
	if err != nil {
		return nil, err
	}
	prepared := make([]deletePreview, 0, len(rows))
	for _, row := range rows {
		s.mu.Lock()
		live := s.prevs[row.Host]
		s.mu.Unlock()
		if err := s.stopPreviewExposure(ctx, row.Host); err != nil {
			restoreErr := s.restoreDeletePreviews(context.WithoutCancel(ctx), prepared)
			return nil, errors.Join(fmt.Errorf("%s: %w", row.Host, err), restoreErr)
		}
		prepared = append(prepared, deletePreview{record: row, live: live})
	}
	return prepared, nil
}

func (s *Service) restoreDeletePreviews(ctx context.Context, previews []deletePreview) error {
	var errs []error
	for _, prepared := range previews {
		p := prepared.live
		if p == nil {
			continue
		}
		handler := s.previewHandler(p)
		if _, err := s.servePreview(ctx, p.flat, p.host, handler); err != nil {
			errs = append(errs, fmt.Errorf("restore preview %s: %w", p.host, err))
		}
	}
	return errors.Join(errs...)
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
	expired := s.expiredPreviews(ttl, now)
	s.mu.Lock()
	live := make(map[string]*liveFlat, len(s.live))
	for k, v := range s.live {
		live[k] = v
	}
	s.mu.Unlock()
	for _, p := range expired {
		_ = s.st.TouchPreview(ctx, p.host, time.UnixMilli(p.last.Load()))
		if err := s.ClosePreview(ctx, p.host); err != nil {
			continue
		}
		s.Event(ctx, p.flat, "info", "preview", fmt.Sprintf("preview %s expired after %s without visits", p.host, ttl), nil)
	}
	s.syncRedirects(ctx)
	// One read of every flat's ID, visibility and type; it also binds flat
	// IDs, so usage counted before a flat's first activation is flushed.
	types := s.flatTypes(ctx, live)
	for slugName, lf := range live {
		s.flushViews(ctx, slugName, lf)
	}
	// Usage of every flat in one transaction: one write per sweep, not one
	// per running flat.
	s.flushUsage(ctx, slices.Collect(maps.Values(live))...)
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
	s.walkDisks(types)
	s.storeSnapshot(ctx, types)
}

// expiredPreviews returns the open previews without a visit for longer
// than ttl at now: those Sweep closes.
func (s *Service) expiredPreviews(ttl time.Duration, now time.Time) []*preview {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []*preview
	for _, p := range s.prevs {
		if now.Sub(time.UnixMilli(p.last.Load())) > ttl {
			expired = append(expired, p)
		}
	}
	return expired
}

// checkQuotas warns (at most daily) about flats whose data grew past the
// disk quota. Uploads over quota are refused outright; database and file
// growth from a running server flat can only be reported.
func (s *Service) checkQuotas(ctx context.Context, live map[string]*liveFlat, now time.Time) {
	quota := s.diskQuota()
	if quota <= 0 {
		return
	}
	for slugName, lf := range live {
		// A partial total still counts against the quota, as before, but is
		// not recorded as the flat's disk figure: the last one stays, with
		// its time.
		used, err := dirSizeErr(s.flatDir(slugName))
		if err == nil {
			s.recordDisk(lf.uidOf(), used)
		} else {
			s.diskFailed(lf.uidOf())
		}
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

// dirSize is the size of the regular files under dir. Parts it cannot read
// are left out of the total.
func dirSize(dir string) int64 { return dirSizeExcept(dir) }

// dirSizeExcept is dirSize without the files at the paths in skip.
func dirSizeExcept(dir string, skip ...string) int64 {
	n, _ := walkSize(dir, skip)
	return n
}

// dirSizeErr is dirSize that also reports the first part it could not read,
// for a measurement that must not publish a partial total as a fresh figure.
// A missing dir, and files removed during the walk, are not errors: they
// hold nothing.
func dirSizeErr(dir string) (int64, error) { return walkSize(dir, nil) }

func walkSize(dir string, skip []string) (int64, error) {
	var n int64
	var first error
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if first == nil && !errors.Is(err, fs.ErrNotExist) {
				first = err
			}
			return nil
		}
		if info.Mode().IsRegular() && !slices.Contains(skip, p) {
			n += info.Size()
		}
		return nil
	})
	return n, first
}

func isWithin(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !startsWithDotDot(rel)
}

func startsWithDotDot(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}

// privateRank orders the Private URL a flat shows when several private routes
// are open: Tailscale, then NetBird, then loopback.
func privateRank(id ProviderID) int {
	switch id {
	case ProviderTailscale:
		return 3
	case ProviderNetbird:
		return 2
	case ProviderLocal:
		return 1
	}
	return 0
}
