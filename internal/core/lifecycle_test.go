package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

func lifecycleSave(t *testing.T, s *Service, slug, body string) store.Version {
	t.Helper()
	v, err := s.SaveVersion(t.Context(), slug, []bundle.File{{Path: "index.html", Data: []byte(body)}}, SaveMeta{}, ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func lifecycleRequest(t *testing.T, s *Service, slug string) *PendingApproval {
	t.Helper()
	_, err := s.Deploy(t.Context(), slug, 0, ViaConsole)
	var p *PendingApproval
	if !errors.As(err, &p) {
		t.Fatalf("wanted pending: %v", err)
	}
	return p
}
func lifecycleApprove(t *testing.T, s *Service, p *PendingApproval) store.Approval {
	t.Helper()
	a, err := s.Decide(t.Context(), p.Approval.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func liveBytes(t *testing.T, s *Service, slug string) string {
	t.Helper()
	r := httptest.NewRecorder()
	s.siteHandler(slug, false).ServeHTTP(r, httptest.NewRequest("GET", "/", nil))
	if r.Code != 200 {
		t.Fatalf("live status %d", r.Code)
	}
	return r.Body.String()
}
func execution(t *testing.T, a store.Approval) ApprovalExecution {
	t.Helper()
	var d ApprovalExecution
	if err := json.Unmarshal(a.ResultData, &d); err != nil {
		t.Fatalf("result_data %q: %v", a.ResultData, err)
	}
	return d
}

func TestLifecycleDraftFreezeAndPositiveControl(t *testing.T) {
	s, _ := newTestService(t)
	v := lifecycleSave(t, s, "frozen", "one")
	p := lifecycleRequest(t, s, "frozen")
	lifecycleSave(t, s, "frozen", "two")
	a, err := s.Decide(t.Context(), p.Approval.ID, true)
	if !errors.Is(err, ErrStaleApproval) || a.Status != "failed" || execution(t, a).FailureCode != "stale_approval" {
		t.Fatalf("stale: %+v %v", a, err)
	}
	vs, _ := s.ListVersions(t.Context(), "frozen")
	if len(vs) != 0 {
		t.Fatal("stale approval created published history")
	}
	a = lifecycleApprove(t, s, lifecycleRequest(t, s, "frozen"))
	if a.Status != "approved" || liveBytes(t, s, "frozen") != "two" {
		t.Fatal("fresh candidate did not publish")
	}
	d, _ := s.GetDraft(t.Context(), "frozen")
	if d.Revision != v.Revision+1 || d.Dirty {
		t.Fatalf("draft %+v", d)
	}
	// Metadata-only saves remain revisions and cannot consume another vN.
	lifecycleSave(t, s, "frozen", "two")
	a, err = s.Decide(t.Context(), lifecycleRequest(t, s, "frozen").Approval.ID, true)
	if !errors.Is(err, ErrUnchangedContent) {
		t.Fatalf("unchanged: %+v %v", a, err)
	}
	vs, _ = s.ListVersions(t.Context(), "frozen")
	if len(vs) != 1 || vs[0].Number != 1 {
		t.Fatalf("versions %+v", vs)
	}
	_, err = s.SaveDraft(t.Context(), "frozen", []bundle.File{{Path: "index.html", Data: []byte("late")}}, SaveMeta{}, 1, ViaAPI)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("revision conflict %v", err)
	}
	if liveBytes(t, s, "frozen") != "two" {
		t.Fatal("save changed Current")
	}
}

func TestLifecycleAuthorityDeniesLabelsAndMissingValidator(t *testing.T) {
	s, _ := newTestService(t)
	lifecycleSave(t, s, "authority", "one")
	p := lifecycleRequest(t, s, "authority")
	s.cfg.ValidateOperatorDecision = nil
	if _, err := s.Decide(t.Context(), p.Approval.ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing validator %v", err)
	}
	if err := s.SetProviderPermission(t.Context(), "authority", store.ProviderPortal, true, ViaConsole); !errors.Is(err, ErrForbidden) {
		t.Fatalf("via bypass %v", err)
	}
	s.cfg.ValidateOperatorDecision = func(context.Context) error { return errors.New("invalid proof") }
	if _, err := s.Decide(t.Context(), p.Approval.ID, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reject bypass %v", err)
	}
	a, _ := s.GetApproval(t.Context(), p.Approval.ID)
	if a.Status != "pending" {
		t.Fatal("unauthorized decision changed pending")
	}
	s.cfg.ValidateOperatorDecision = func(context.Context) error { return nil }
	lifecycleApprove(t, s, p)
}

func TestLifecycleConcurrentPublishExactlyOnce(t *testing.T) {
	s, _ := newTestService(t)
	lifecycleSave(t, s, "once", "one")
	p := lifecycleRequest(t, s, "once")
	p2 := lifecycleRequest(t, s, "once")
	if p.Approval.ID != p2.Approval.ID {
		t.Fatal("request not deduplicated")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Decide(t.Context(), p.Approval.ID, true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	vs, _ := s.ListVersions(t.Context(), "once")
	ds, _ := s.Deployments(t.Context(), "once")
	if len(vs) != 1 || len(ds) != 1 {
		t.Fatalf("duplicate versions/history %v %v", vs, ds)
	}
	// A crash after durable live commit but before final status must only finish.
	p3 := lifecycleRequest(t, s, "once")
	if err := s.st.ClaimApproval(t.Context(), p3.Approval.ID); err != nil {
		t.Fatal(err)
	}
	// This case uses an activation of the already published version.
	_ = s.st.FinishApproval(t.Context(), p3.Approval.ID, "failed", "fixture cleanup", time.Now())
	_, err := s.Deploy(t.Context(), "once", 1, ViaAPI)
	var activation *PendingApproval
	if !errors.As(err, &activation) {
		t.Fatal(err)
	}
	if err := s.st.ClaimApprovalAuthorized(t.Context(), activation.Approval.ID, "fixture operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.applyApproval(t.Context(), *activation.Approval); err != nil {
		t.Fatal(err)
	}
	s.resumeApplying(t.Context())
	ds, _ = s.Deployments(t.Context(), "once")
	if len(ds) != 2 {
		t.Fatalf("resume reactivated %v", ds)
	}
	a, _ := s.GetApproval(t.Context(), activation.Approval.ID)
	if a.Status != "approved" || execution(t, a).DataImpact != "none" {
		t.Fatalf("recovered %+v", a)
	}
}

type lifecycleRuntime func(RuntimeSpec) (Instance, error)

func (f lifecycleRuntime) Start(_ context.Context, s RuntimeSpec) (Instance, error) { return f(s) }

type lifecycleInstance struct{ http.Handler }

func (lifecycleInstance) Stop() {}

func TestLifecycleSuccessfulHealthWritesOnlyCopy(t *testing.T) {
	s, dir := newTestService(t)
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		if err := os.MkdirAll(spec.DataDir, 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(spec.DataDir, "startup"), []byte("write"), 0600); err != nil {
			return nil, err
		}
		return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				_ = os.WriteFile(filepath.Join(spec.DataDir, "health-sentinel"), []byte("copy only"), 0600)
			}
			w.WriteHeader(200)
		})}, nil
	})
	_, err := s.SaveVersion(t.Context(), "server", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server","health":"/health"}`)}, {Path: "server.js", Data: []byte("export default {}")}}, SaveMeta{}, ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	p := lifecycleRequest(t, s, "server")
	live := filepath.Join(dir, "flats", "server", "data")
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatal("runtime ran before approval")
	}
	a := lifecycleApprove(t, s, p)
	d := execution(t, a)
	if d.DataImpact != "runtime_start" || d.HealthData != "isolated_copy" || d.LiveData != "runtime_may_write" {
		t.Fatalf("false data claim %+v", d)
	}
	if _, err := os.Stat(filepath.Join(live, "health-sentinel")); !os.IsNotExist(err) {
		t.Fatal("healthy sentinel reached live")
	}
	if _, err := os.Stat(filepath.Join(live, "startup")); err != nil {
		t.Fatal("live startup missing")
	}
}

func TestLifecycleFailedHealthDoesNotConsumeVersion(t *testing.T) {
	s, _ := newTestService(t)
	lifecycleSave(t, s, "health", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "health"))
	_, err := s.SaveVersion(t.Context(), "health", []bundle.File{{Path: "index.html", Data: []byte("broken")}, {Path: "flats.json", Data: []byte(`{"health":"/absent"}`)}}, SaveMeta{}, ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Decide(t.Context(), lifecycleRequest(t, s, "health").Approval.ID, true)
	var de *DeployError
	if !errors.As(err, &de) || de.Health.Status != 404 || execution(t, a).FailureCode != "health_check_failed" {
		t.Fatalf("health %+v %v", a, err)
	}
	if liveBytes(t, s, "health") != "one" {
		t.Fatal("failed health changed live")
	}
	lifecycleSave(t, s, "health", "two")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "health"))
	vs, _ := s.ListVersions(t.Context(), "health")
	if len(vs) != 2 || vs[0].Number != 2 {
		t.Fatalf("consumed number %+v", vs)
	}
}

type lifecycleNetwork struct {
	policy, state string
	stopFails     bool
	requests      []ExposureRequest
	public        http.Handler
}

func (n *lifecycleNetwork) ServeExposure(_ context.Context, r ExposureRequest) (ExposureResult, error) {
	n.requests = append(n.requests, r)
	id := ProviderLocal
	if r.Visibility == "public" {
		id = ProviderPortal
		n.public = r.Handler
	}
	return ExposureResult{Endpoints: []ExposureEndpoint{{Provider: id, State: n.state, Configured: true, Permitted: true, Ready: n.state == "ready", Audience: r.Audience, Host: r.Host}}}, nil
}
func (n *lifecycleNetwork) StopPublicRoutes(context.Context, string) (PublicStopResult, error) {
	if n.stopFails {
		return PublicStopResult{Unconfirmed: []ProviderID{ProviderPortal}}, nil
	}
	n.public = nil
	return PublicStopResult{Stopped: []ProviderID{ProviderPortal}}, nil
}
func (n *lifecycleNetwork) ExposurePolicy(context.Context) (string, error) { return n.policy, nil }
func (n *lifecycleNetwork) ExposureStatus(context.Context, string) (ExposureResult, error) {
	return ExposureResult{Endpoints: []ExposureEndpoint{{Provider: ProviderPortal, State: n.state, Configured: true, Permitted: true, Ready: n.state == "ready", Audience: AudienceCurrent}}}, nil
}

func TestLifecycleProviderPolicyReadinessAndStop(t *testing.T) {
	s, _ := newTestService(t)
	n := &lifecycleNetwork{policy: "host granted", state: "ready"}
	s.cfg.Lifecycle = n
	lifecycleSave(t, s, "routes", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "routes"))
	if err := s.SetProviderPermission(t.Context(), "routes", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	r, err := s.SetVisibility(t.Context(), "routes", store.Public, ViaConsole, "")
	if err != nil {
		t.Fatal(err)
	}
	n.policy = "host revoked"
	a, err := s.Decide(t.Context(), r.Approval.ID, true)
	if !errors.Is(err, ErrStaleApproval) || execution(t, a).FailureCode != "stale_approval" {
		t.Fatalf("policy %+v %v", a, err)
	}
	n.policy = "host granted"
	r, err = s.SetVisibility(t.Context(), "routes", store.Public, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	n.state = "starting"
	a, err = s.Decide(t.Context(), r.Approval.ID, true)
	if !errors.Is(err, ErrProviderNotReady) || execution(t, a).FailureCode != "provider_not_ready" {
		t.Fatalf("readiness %+v %v", a, err)
	}
	f, _ := s.GetFlat(t.Context(), "routes")
	if f.Visibility != store.Private || f.Publication != "published" {
		t.Fatalf("false public %+v", f)
	}
	n.state = "ready"
	r, err = s.SetVisibility(t.Context(), "routes", store.Public, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Decide(t.Context(), r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	lifecycleSave(t, s, "routes", "draft")
	pv, err := s.OpenPreview(t.Context(), "routes", 0)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Target != "draft" {
		t.Fatal(pv)
	}
	last := n.requests[len(n.requests)-1]
	if last.Visibility != "private" || last.Audience != AudienceDraft {
		t.Fatalf("public Draft %+v", last)
	}
	for _, id := range last.Permitted {
		if id == ProviderPortal || id == ProviderFunnel {
			t.Fatalf("public provider in Draft %+v", last)
		}
	}
	r, err = s.SetVisibility(t.Context(), "routes", store.Private, ViaConsole, "")
	if err != nil {
		t.Fatal(err)
	}
	n.stopFails = true
	a, err = s.Decide(t.Context(), r.Approval.ID, true)
	if !errors.Is(err, ErrPublicStopUnconfirmed) || execution(t, a).FailureCode != "public_stop_unconfirmed" {
		t.Fatalf("stop %+v %v", a, err)
	}
	f, _ = s.GetFlat(t.Context(), "routes")
	if f.Visibility != store.Public || liveBytes(t, s, "routes") != "one" {
		t.Fatal("incomplete stop changed visibility/current")
	}
	n.stopFails = false
	r, err = s.SetVisibility(t.Context(), "routes", store.Private, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Decide(t.Context(), r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	f, _ = s.GetFlat(t.Context(), "routes")
	if f.Visibility != store.Private || f.LiveVersion != 1 {
		t.Fatal(f)
	}
}

func TestLifecycleRollbackRestoresFilesOnlyAfterApproval(t *testing.T) {
	s, _ := newTestService(t)
	lifecycleSave(t, s, "files", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "files"))
	data := s.dataDirOf("files")
	if err := writeFile(filepath.Join(data, "files", "note"), "before"); err != nil {
		t.Fatal(err)
	}
	lifecycleSave(t, s, "files", "two")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "files"))
	if err := writeFile(filepath.Join(data, "files", "note"), "after"); err != nil {
		t.Fatal(err)
	}
	_, err := s.RollbackWithData(t.Context(), "files", 1, true, ViaConsole)
	var p *PendingApproval
	if !errors.As(err, &p) {
		t.Fatal(err)
	}
	if b, _ := readFile(filepath.Join(data, "files", "note")); b != "after" {
		t.Fatal("pending restore mutated FILES")
	}
	a := lifecycleApprove(t, s, p)
	if b, _ := readFile(filepath.Join(data, "files", "note")); b != "before" {
		t.Fatal("FILES not restored")
	}
	if execution(t, a).DataImpact != "restore_data" {
		t.Fatal(a)
	}
	vs, _ := s.ListVersions(t.Context(), "files")
	if len(vs) != 2 || liveBytes(t, s, "files") != "one" {
		t.Fatal("rollback created version/incorrect current")
	}
	// Legacy snapshots contain only DB and must preserve FILES.
	legacy := filepath.Join(s.snapshotDir("files"), "before-v99-1.sqlite")
	if err := writeFile(legacy, "legacy bytes"); err != nil {
		t.Fatal(err)
	}
	if err := s.installSnapshot("files", legacy); err != nil {
		t.Fatal(err)
	}
	if b, _ := readFile(filepath.Join(data, "files", "note")); b != "before" {
		t.Fatal("legacy DB-only restore destroyed FILES")
	}
}

func TestLifecycleLegacyMigrationKeepsNumberedIdentityAndRefs(t *testing.T) {
	s, _ := newTestService(t)
	lifecycleSave(t, s, "legacy", "published")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "legacy"))
	old := store.Version{Flat: "legacy", Number: 2, Hash: "saved-only", Kind: "static", Manifest: json.RawMessage(`{"kind":"static","entry":"index.html"}`), CreatedAt: time.Now()}
	if err := s.st.InsertVersion(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(s.versionDir("legacy", 2), "index.html"), "saved-only"); err != nil {
		t.Fatal(err)
	}
	if err := s.st.InsertApproval(t.Context(), store.Approval{ID: "legacy-numeric", Flat: "legacy", Action: "activate", Params: json.RawMessage(`{"version":2}`), Status: "pending", Via: "api", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.st.InsertPreview(t.Context(), store.Preview{Host: "old-preview", Flat: "legacy", Version: 2, CreatedAt: time.Now(), LastAccess: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateLegacyDrafts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateLegacyDrafts(t.Context()); err != nil {
		t.Fatal(err)
	}
	vs, _ := s.ListVersions(t.Context(), "legacy")
	if len(vs) != 1 || vs[0].Number != 1 {
		t.Fatal(vs)
	}
	a, _ := s.GetApproval(t.Context(), "legacy-numeric")
	if a.Status != "failed" {
		t.Fatal("legacy alias remained actionable")
	}
	pv, _ := s.st.GetPreview(t.Context(), "old-preview")
	if pv.Target != "draft" || pv.Version != 0 || pv.Revision == 0 {
		t.Fatal(pv)
	}
	revs, _ := s.st.ListDraftRevisions(t.Context(), "legacy")
	var migrated store.DraftRevision
	for _, r := range revs {
		if r.LegacyNumber == 2 {
			migrated = r
		}
	}
	if migrated.Revision == 0 {
		t.Fatal("legacy source not preserved")
	}
	if b, _ := readFile(filepath.Join(s.draftRevDir("legacy", migrated.Revision), "index.html")); b != "saved-only" {
		t.Fatal("legacy bytes missing")
	}
	lifecycleSave(t, s, "legacy", "fresh-two")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "legacy"))
	if liveBytes(t, s, "legacy") != "fresh-two" {
		t.Fatal("number reused wrong content")
	}
	a, _ = s.GetApproval(t.Context(), "legacy-numeric")
	if a.Status != "failed" {
		t.Fatal("number reuse revived old approval")
	}
}
