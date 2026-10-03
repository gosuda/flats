package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

type lifecycleRouteKey struct {
	host     string
	provider ProviderID
}

// lifecycleRouteNet keeps exact route registrations so partial exposure and
// teardown failures can be exercised without touching a live provider.
type lifecycleRouteNet struct {
	mu           sync.Mutex
	routes       map[lifecycleRouteKey]http.Handler
	failPublic   error
	failPrivate  error
	stopPublic   error
	stopExposure map[string]error
}

func newLifecycleRouteNet() *lifecycleRouteNet {
	return &lifecycleRouteNet{routes: make(map[lifecycleRouteKey]http.Handler), stopExposure: make(map[string]error)}
}

func (n *lifecycleRouteNet) ServeExposure(_ context.Context, req ExposureRequest) (ExposureResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if req.Visibility == "public" && n.failPublic != nil {
		return ExposureResult{}, n.failPublic
	}
	if req.Visibility == "private" && n.failPrivate != nil {
		return ExposureResult{}, n.failPrivate
	}
	var ids []ProviderID
	if req.Visibility == "public" {
		for _, id := range req.Permitted {
			if id == ProviderPortal || id == ProviderFunnel {
				ids = append(ids, id)
			}
		}
	} else {
		ids = append(ids, ProviderLocal)
		if slices.Contains(req.Permitted, ProviderTailscale) {
			ids = append(ids, ProviderTailscale)
		}
	}
	res := ExposureResult{}
	for _, id := range ids {
		n.routes[lifecycleRouteKey{host: req.Host, provider: id}] = req.Handler
		res.Endpoints = append(res.Endpoints, ExposureEndpoint{Provider: id, URL: fmt.Sprintf("https://%s.%s.test", req.Host, id), State: "ready", Configured: true, Permitted: true, Ready: true, Audience: req.Audience, Host: req.Host})
	}
	return res, nil
}

func (n *lifecycleRouteNet) StopPublicRoutes(_ context.Context, host string) (PublicStopResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopPublic != nil {
		return PublicStopResult{}, n.stopPublic
	}
	var stopped []ProviderID
	for _, id := range []ProviderID{ProviderPortal, ProviderFunnel} {
		key := lifecycleRouteKey{host: host, provider: id}
		if _, ok := n.routes[key]; ok {
			delete(n.routes, key)
			stopped = append(stopped, id)
		}
	}
	return PublicStopResult{Stopped: stopped}, nil
}

func (n *lifecycleRouteNet) StopExposure(_ context.Context, host string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.stopExposure[host]; err != nil {
		return err
	}
	delete(n.routes, lifecycleRouteKey{host: host, provider: ProviderLocal})
	delete(n.routes, lifecycleRouteKey{host: host, provider: ProviderTailscale})
	return nil
}

func (n *lifecycleRouteNet) StopProviderRoutes(_ context.Context, host string, id ProviderID) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.routes, lifecycleRouteKey{host: host, provider: id})
	return nil
}

func (n *lifecycleRouteNet) HasProviderRoute(_ context.Context, host string, id ProviderID) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.routes[lifecycleRouteKey{host: host, provider: id}]
	return ok, nil
}

func (n *lifecycleRouteNet) ExposurePolicy(context.Context) (string, error) {
	return "test provider policy", nil
}

func (n *lifecycleRouteNet) ExposureStatus(_ context.Context, host string) (ExposureResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	res := ExposureResult{}
	for key := range n.routes {
		if key.host == host {
			res.Endpoints = append(res.Endpoints, ExposureEndpoint{Provider: key.provider, URL: fmt.Sprintf("https://%s.%s.test", host, key.provider), State: "ready", Configured: true, Permitted: true, Ready: true, Audience: AudienceCurrent, Host: host})
		}
	}
	return res, nil
}

func (n *lifecycleRouteNet) serves(host string, id ProviderID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.routes[lifecycleRouteKey{host: host, provider: id}]
	return ok
}

func (n *lifecycleRouteNet) status(host string, id ProviderID) int {
	n.mu.Lock()
	h := n.routes[lifecycleRouteKey{host: host, provider: id}]
	n.mu.Unlock()
	if h == nil {
		return http.StatusNotFound
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "https://example.test/path?q=1", nil))
	return r.Code
}

func newLifecycleService(t *testing.T, dir string, st *store.Store, network LifecycleNet, runtime Runtime) *Service {
	t.Helper()
	s, err := New(t.Context(), Config{
		DataDir:                  dir,
		Store:                    st,
		Private:                  &memNet{hosts: map[string]http.Handler{}},
		Lifecycle:                network,
		Runtime:                  runtime,
		ValidateOperatorDecision: func(context.Context) error { return nil },
		Logf:                     t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPartialPublicRestartRenameTracksAliasForRevokeAndDelete(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "flats.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	firstNet := newLifecycleRouteNet()
	first := newLifecycleService(t, dir, st, firstNet, nil)
	lifecycleSave(t, first, "legacy", "one")
	lifecycleApprove(t, first, lifecycleRequest(t, first, "legacy"))
	for _, provider := range []string{store.ProviderTailscale, store.ProviderPortal} {
		if err := first.SetProviderPermission(t.Context(), "legacy", provider, true, ViaConsole); err != nil {
			t.Fatal(err)
		}
	}
	request, err := first.SetVisibility(t.Context(), "legacy", store.Public, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Decide(t.Context(), request.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	restartedNet := newLifecycleRouteNet()
	restartedNet.failPublic = errors.New("public provider unavailable after private registration")
	restarted := newLifecycleService(t, dir, st, restartedNet, nil)
	defer restarted.Close()
	if !restarted.state("legacy").privateServed || !restartedNet.serves("legacy", ProviderTailscale) {
		t.Fatal("restart lost the successful private registration")
	}

	if _, err := restarted.RenameSlug(t.Context(), "legacy", "moved", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if restarted.Redirects()["legacy"] != "moved" || restartedNet.status("legacy", ProviderLocal) != http.StatusTemporaryRedirect {
		t.Fatal("rename did not record and serve the old private address")
	}
	if err := restarted.SetProviderPermission(t.Context(), "moved", store.ProviderTailscale, false, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if restartedNet.serves("legacy", ProviderTailscale) || restartedNet.serves("moved", ProviderTailscale) {
		t.Fatal("Tailscale revocation missed a rename alias")
	}
	if !restartedNet.serves("legacy", ProviderLocal) {
		t.Fatal("Tailscale revocation stopped the Local redirect")
	}
	if _, err := restarted.Delete(t.Context(), "moved", ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	if restartedNet.serves("legacy", ProviderLocal) || restartedNet.serves("moved", ProviderLocal) {
		t.Fatal("delete left a renamed private route serving")
	}
	if _, ok := restarted.Redirects()["legacy"]; ok {
		t.Fatal("delete retained the alias record")
	}
}

type lifecycleTrackedInstance struct {
	http.Handler
	stopped atomic.Bool
}

func (i *lifecycleTrackedInstance) Stop() { i.stopped.Store(true) }

func TestDeleteRetriesFailedPreviewStopBeforeDestroyingFlat(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	var preview *lifecycleTrackedInstance
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		inst := &lifecycleTrackedInstance{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })}
		if strings.Contains(spec.DataDir, string(filepath.Separator)+"previews"+string(filepath.Separator)) {
			preview = inst
		}
		return inst, nil
	})
	_, err := s.SaveVersion(t.Context(), "server-delete", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server"}`)}, {Path: "server.js", Data: []byte("export default {}")}}, SaveMeta{}, ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleApprove(t, s, lifecycleRequest(t, s, "server-delete"))
	p, err := s.OpenPreview(t.Context(), "server-delete", 1)
	if err != nil {
		t.Fatal(err)
	}
	if preview == nil {
		t.Fatal("preview runtime was not started")
	}
	network.stopExposure[p.Host] = errors.New("preview listener still reachable")
	if _, err := s.Delete(t.Context(), "server-delete", ViaConsole, ""); err == nil || !strings.Contains(err.Error(), "stop previews") {
		t.Fatalf("delete did not report preview teardown: %v", err)
	}
	if _, err := s.GetFlat(t.Context(), "server-delete"); err != nil {
		t.Fatalf("failed delete removed the flat: %v", err)
	}
	if _, err := s.st.GetPreview(t.Context(), p.Host); err != nil || preview.stopped.Load() {
		t.Fatalf("failed delete lost retry state or stopped runtime: row=%v stopped=%t", err, preview.stopped.Load())
	}
	delete(network.stopExposure, p.Host)
	if _, err := s.Delete(t.Context(), "server-delete", ViaConsole, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.GetPreview(t.Context(), p.Host); !errors.Is(err, store.ErrNotFound) || !preview.stopped.Load() {
		t.Fatalf("retry did not finish preview teardown: row=%v stopped=%t", err, preview.stopped.Load())
	}
}

func TestVisibilityRollbackSurfacesUnconfirmedPublicStop(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "rollback-stop", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "rollback-stop"))
	if err := s.SetProviderPermission(t.Context(), "rollback-stop", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	request, err := s.SetVisibility(t.Context(), "rollback-stop", store.Public, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	network.failPublic = errors.New("public registration failed")
	network.stopPublic = errors.New("public teardown unconfirmed")
	receipt, err := s.Decide(t.Context(), request.Approval.ID, true)
	if !errors.Is(err, ErrPublicStopUnconfirmed) || !strings.Contains(err.Error(), "public teardown unconfirmed") {
		t.Fatalf("rollback hid public teardown failure: %v", err)
	}
	if receipt.Status != "failed" || execution(t, receipt).FailureCode != "public_stop_unconfirmed" {
		t.Fatalf("wrong failure receipt: %+v (%+v)", receipt, execution(t, receipt))
	}
	f, _ := s.GetFlat(t.Context(), "rollback-stop")
	if f.Visibility != store.Private {
		t.Fatal("failed Public transition changed persisted visibility")
	}
}

func TestCommittedPrivateVisibilityKeepsApprovedReceiptOnExposureError(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "private-receipt", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "private-receipt"))
	if err := s.SetProviderPermission(t.Context(), "private-receipt", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	toPublic, err := s.SetVisibility(t.Context(), "private-receipt", store.Public, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(t.Context(), toPublic.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	toPrivate, err := s.SetVisibility(t.Context(), "private-receipt", store.Private, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	network.failPrivate = errors.New("local listener unavailable")
	receipt, err := s.Decide(t.Context(), toPrivate.Approval.ID, true)
	if err != nil || receipt.Status != "approved" || execution(t, receipt).Status != "approved" {
		t.Fatalf("committed Private transition recorded as failure: %+v %v", receipt, err)
	}
	f, _ := s.GetFlat(t.Context(), "private-receipt")
	if f.Visibility != store.Private || network.serves("private-receipt", ProviderPortal) {
		t.Fatalf("committed transition does not match persisted/network state: %+v", f)
	}
}
