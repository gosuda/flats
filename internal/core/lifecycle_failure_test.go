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
	"time"

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
	mu                    sync.Mutex
	routes                map[lifecycleRouteKey]http.Handler
	owners                map[lifecycleRouteKey]string
	identities            map[string]int
	nextIdentity          int
	failPublic            error
	failPrivate           error
	failPrivateAfterLocal error
	stopPublic            error
	stopExposure          map[string]error
	stopSlug              map[string]error
	stopSlugCalls         map[string]int
}

func newLifecycleRouteNet() *lifecycleRouteNet {
	return &lifecycleRouteNet{
		routes: make(map[lifecycleRouteKey]http.Handler), owners: make(map[lifecycleRouteKey]string), identities: make(map[string]int),
		stopExposure: make(map[string]error), stopSlug: make(map[string]error), stopSlugCalls: make(map[string]int),
	}
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
		if id == ProviderTailscale && n.failPrivateAfterLocal != nil {
			res.Endpoints = append(res.Endpoints, ExposureEndpoint{Provider: id, State: "unavailable", Detail: n.failPrivateAfterLocal.Error(), Audience: req.Audience, Host: req.Host})
			continue
		}
		key := lifecycleRouteKey{host: req.Host, provider: id}
		n.routes[key] = req.Handler
		n.owners[key] = req.Slug
		if id == ProviderFunnel && n.identities[req.Host] == 0 {
			n.nextIdentity++
			n.identities[req.Host] = n.nextIdentity
		}
		res.Endpoints = append(res.Endpoints, ExposureEndpoint{Provider: id, URL: fmt.Sprintf("https://%s.%s.test", req.Host, id), State: "ready", Configured: true, Permitted: true, Ready: true, Audience: req.Audience, Host: req.Host})
	}
	if req.Visibility == "private" && n.failPrivateAfterLocal != nil && slices.Contains(req.Permitted, ProviderTailscale) {
		return res, n.failPrivateAfterLocal
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
			delete(n.owners, key)
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
	for _, id := range []ProviderID{ProviderLocal, ProviderTailscale} {
		key := lifecycleRouteKey{host: host, provider: id}
		delete(n.routes, key)
		delete(n.owners, key)
	}
	return nil
}

func (n *lifecycleRouteNet) StopSlug(_ context.Context, slugName string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stopSlugCalls[slugName]++
	if err := n.stopSlug[slugName]; err != nil {
		return err
	}
	for key, owner := range n.owners {
		if owner == slugName {
			if err := n.stopExposure[key.host]; err != nil {
				return fmt.Errorf("stop previews: %w", err)
			}
		}
	}
	for key, owner := range n.owners {
		if owner != slugName {
			continue
		}
		delete(n.routes, key)
		delete(n.owners, key)
		if key.provider == ProviderFunnel {
			delete(n.identities, key.host)
		}
	}
	return nil
}

func (n *lifecycleRouteNet) StopProviderRoutes(_ context.Context, host string, id ProviderID) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	key := lifecycleRouteKey{host: host, provider: id}
	delete(n.routes, key)
	delete(n.owners, key)
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

func (n *lifecycleRouteNet) identity(host string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.identities[host]
}

func (n *lifecycleRouteNet) slugStopCount(slugName string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stopSlugCalls[slugName]
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

func makeFunnelPublic(t *testing.T, s *Service, slugName, body string) {
	t.Helper()
	lifecycleSave(t, s, slugName, body)
	lifecycleApprove(t, s, lifecycleRequest(t, s, slugName))
	if err := s.SetProviderPermission(t.Context(), slugName, store.ProviderFunnel, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	request, err := s.SetVisibility(t.Context(), slugName, store.Public, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(t.Context(), request.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
}

func TestFunnelOnlyDeleteRetiresSlugBeforeApprovalCommits(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	makeFunnelPublic(t, s, "funnel-delete", "one")
	firstIdentity := network.identity("funnel-delete")
	if firstIdentity == 0 || !network.serves("funnel-delete", ProviderFunnel) || network.serves("funnel-delete", ProviderTailscale) {
		t.Fatal("test did not create a Funnel-only provider identity")
	}

	network.stopSlug["funnel-delete"] = errors.New("logout is not confirmed")
	pending, err := s.Delete(t.Context(), "funnel-delete", ViaAPI, "retire its provider identity")
	if err != nil || pending.Status != "pending_approval" {
		t.Fatalf("delete must remain approval-gated: %+v %v", pending, err)
	}
	receipt, err := s.Decide(t.Context(), pending.Approval.ID, true)
	if err == nil || receipt.Status != "failed" || network.slugStopCount("funnel-delete") != 1 {
		t.Fatalf("unconfirmed retirement reported success: receipt=%+v err=%v calls=%d", receipt, err, network.slugStopCount("funnel-delete"))
	}
	if _, err := s.GetFlat(t.Context(), "funnel-delete"); err != nil || network.identity("funnel-delete") != firstIdentity || !network.serves("funnel-delete", ProviderFunnel) {
		t.Fatalf("failed retirement lost retry state: flat=%v identity=%d funnel=%t", err, network.identity("funnel-delete"), network.serves("funnel-delete", ProviderFunnel))
	}

	delete(network.stopSlug, "funnel-delete")
	pending, err = s.Delete(t.Context(), "funnel-delete", ViaAPI, "retry confirmed retirement")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = s.Decide(t.Context(), pending.Approval.ID, true)
	if err != nil || receipt.Status != "approved" {
		t.Fatalf("confirmed retirement did not finish delete: %+v %v", receipt, err)
	}
	if _, err := s.GetFlat(t.Context(), "funnel-delete"); !errors.Is(err, store.ErrNotFound) || network.identity("funnel-delete") != 0 || network.serves("funnel-delete", ProviderFunnel) {
		t.Fatalf("delete left the old identity or route: flat=%v identity=%d funnel=%t", err, network.identity("funnel-delete"), network.serves("funnel-delete", ProviderFunnel))
	}

	makeFunnelPublic(t, s, "funnel-delete", "two")
	if nextIdentity := network.identity("funnel-delete"); nextIdentity == 0 || nextIdentity == firstIdentity {
		t.Fatalf("recreated slug inherited deleted identity: old=%d new=%d", firstIdentity, nextIdentity)
	}
}

func TestFunnelOnlyRedirectExpiryRetriesRetirement(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	clock := time.Now().UTC()
	s.now = func() time.Time { return clock }
	makeFunnelPublic(t, s, "funnel-alias", "one")
	if _, err := s.RenameSlug(t.Context(), "funnel-alias", "funnel-moved", ViaAPI); err != nil {
		t.Fatal(err)
	}
	aliasIdentity := network.identity("funnel-alias")
	if aliasIdentity == 0 || s.Redirects()["funnel-alias"] != "funnel-moved" {
		t.Fatal("rename did not retain the Funnel alias identity")
	}

	network.stopSlug["funnel-alias"] = errors.New("alias logout is not confirmed")
	clock = clock.Add(8 * 24 * time.Hour)
	s.Sweep(t.Context())
	if s.Redirects()["funnel-alias"] != "funnel-moved" || network.identity("funnel-alias") != aliasIdentity || !network.serves("funnel-alias", ProviderFunnel) {
		t.Fatalf("failed expiry lost retry state: redirects=%v identity=%d funnel=%t", s.Redirects(), network.identity("funnel-alias"), network.serves("funnel-alias", ProviderFunnel))
	}
	if _, err := s.CreateFlat(t.Context(), "funnel-alias", "", ViaAPI); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unconfirmed alias retirement released the slug: %v", err)
	}

	delete(network.stopSlug, "funnel-alias")
	s.Sweep(t.Context())
	if _, ok := s.Redirects()["funnel-alias"]; ok || network.identity("funnel-alias") != 0 || network.serves("funnel-alias", ProviderFunnel) {
		t.Fatalf("confirmed expiry left the alias identity or route: redirects=%v identity=%d funnel=%t", s.Redirects(), network.identity("funnel-alias"), network.serves("funnel-alias", ProviderFunnel))
	}
	makeFunnelPublic(t, s, "funnel-alias", "two")
	if nextIdentity := network.identity("funnel-alias"); nextIdentity == 0 || nextIdentity == aliasIdentity {
		t.Fatalf("recreated alias inherited retired identity: old=%d new=%d", aliasIdentity, nextIdentity)
	}
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

func TestPartialPrivateExposureRenameTracksLocalAlias(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "private-partial", "one")
	if err := s.SetProviderPermission(t.Context(), "private-partial", store.ProviderTailscale, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	network.failPrivateAfterLocal = fmt.Errorf("host provider: %w", ErrProviderNotPermitted)
	lifecycleApprove(t, s, lifecycleRequest(t, s, "private-partial"))
	if !s.state("private-partial").privateServed || !network.serves("private-partial", ProviderLocal) || network.serves("private-partial", ProviderTailscale) {
		t.Fatal("partial exposure did not retain exact Local registration state")
	}
	if _, err := s.RenameSlug(t.Context(), "private-partial", "private-moved", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if s.Redirects()["private-partial"] != "private-moved" || network.status("private-partial", ProviderLocal) != http.StatusTemporaryRedirect {
		t.Fatal("rename orphaned a Local route after sibling Tailscale failure")
	}
}

func TestRenamePreviewStopFailureIsRetrySafe(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "preview-rename", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "preview-rename"))
	p, err := s.OpenPreview(t.Context(), "preview-rename", 1)
	if err != nil {
		t.Fatal(err)
	}
	network.stopExposure[p.Host] = errors.New("preview route still reachable")
	if _, err := s.RenameSlug(t.Context(), "preview-rename", "preview-moved", ViaAPI); err == nil || !strings.Contains(err.Error(), "close previews before rename") {
		t.Fatalf("rename did not surface preview teardown failure: %v", err)
	}
	if _, err := s.GetFlat(t.Context(), "preview-rename"); err != nil {
		t.Fatalf("failed rename lost source flat: %v", err)
	}
	if _, err := s.GetFlat(t.Context(), "preview-moved"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed rename created target flat: %v", err)
	}
	if _, err := s.st.GetPreview(t.Context(), p.Host); err != nil || !network.serves(p.Host, ProviderLocal) {
		t.Fatalf("failed rename lost retryable preview: row=%v route=%t", err, network.serves(p.Host, ProviderLocal))
	}
	delete(network.stopExposure, p.Host)
	if _, err := s.RenameSlug(t.Context(), "preview-rename", "preview-moved", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.GetPreview(t.Context(), p.Host); !errors.Is(err, store.ErrNotFound) || network.serves(p.Host, ProviderLocal) {
		t.Fatalf("rename retry did not close preview: row=%v route=%t", err, network.serves(p.Host, ProviderLocal))
	}
}

func TestSweepRecordsPreviewExpiryOnlyAfterConfirmedClose(t *testing.T) {
	s, _ := newTestService(t)
	network := newLifecycleRouteNet()
	s.cfg.Lifecycle = network
	lifecycleSave(t, s, "sweep-preview", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "sweep-preview"))
	p, err := s.OpenPreview(t.Context(), "sweep-preview", 1)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	livePreview := s.prevs[p.Host]
	s.mu.Unlock()
	livePreview.last.Store(s.now().Add(-s.previewTTL() - time.Second).UnixMilli())
	network.stopExposure[p.Host] = errors.New("preview retirement is not confirmed")

	s.Sweep(t.Context())
	if _, err := s.st.GetPreview(t.Context(), p.Host); err != nil || !network.serves(p.Host, ProviderLocal) {
		t.Fatalf("failed sweep lost retry state: row=%v route=%t", err, network.serves(p.Host, ProviderLocal))
	}
	if got := previewExpiryEvents(t, s, "sweep-preview", p.Host); got != 0 {
		t.Fatalf("failed close was recorded as expired %d times", got)
	}

	delete(network.stopExposure, p.Host)
	s.Sweep(t.Context())
	if _, err := s.st.GetPreview(t.Context(), p.Host); !errors.Is(err, store.ErrNotFound) || network.serves(p.Host, ProviderLocal) {
		t.Fatalf("sweep retry did not close preview: row=%v route=%t", err, network.serves(p.Host, ProviderLocal))
	}
	if got := previewExpiryEvents(t, s, "sweep-preview", p.Host); got != 1 {
		t.Fatalf("confirmed close recorded %d expiry events", got)
	}
}

func previewExpiryEvents(t *testing.T, s *Service, slugName, host string) int {
	t.Helper()
	events, err := s.Events(t.Context(), slugName, "preview", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := "preview " + host + " expired after "
	count := 0
	for _, event := range events {
		if strings.Contains(event.Message, want) {
			count++
		}
	}
	return count
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
	network.failPublic = fmt.Errorf("public registration failed: %w", ErrProviderNotReady)
	network.stopPublic = fmt.Errorf("public teardown unconfirmed: %w", ErrProviderUnavailable)
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
