package core

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

func TestLifecycleRejectionPersistsActor(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	lifecycleSave(t, s, "audit", "one")
	r, err := s.Delete(t.Context(), "audit", ViaAPI, "requester is not the operator")
	if err != nil || r.Approval == nil {
		t.Fatalf("request: %+v %v", r, err)
	}
	before := time.Now().Unix()
	a, err := s.Decide(t.Context(), r.Approval.ID, false)
	if err != nil || a.Status != "rejected" || a.DecidedBy != "console" || a.AuthorizedAt == nil || a.DecidedAt == nil || a.AuthorizedAt.Unix() < before || !a.DecidedAt.Equal(*a.AuthorizedAt) {
		t.Fatalf("rejection audit: %+v %v", a, err)
	}
	again, err := s.Decide(t.Context(), a.ID, false)
	if err != nil || again.DecidedBy != a.DecidedBy || !again.AuthorizedAt.Equal(*a.AuthorizedAt) {
		t.Fatalf("retry overwrote actor: %+v %v", again, err)
	}
	if _, err := s.Decide(t.Context(), a.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("opposite decision: %v", err)
	}
	// Restart both service and store: the returned DTO must reflect durable audit.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.cfg
	cfg.Store = reopened
	st = reopened
	s, err = New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := s.GetApproval(t.Context(), a.ID)
	if err != nil || persisted.Status != "rejected" || persisted.DecidedBy != a.DecidedBy || persisted.AuthorizedAt == nil || !persisted.AuthorizedAt.Equal(*a.AuthorizedAt) || persisted.DecidedAt == nil || !persisted.DecidedAt.Equal(*a.DecidedAt) {
		t.Fatalf("reloaded audit: %+v %v", persisted, err)
	}
	if _, err := reopened.GetFlat(t.Context(), "audit"); err != nil {
		t.Fatal("rejection removed flat", err)
	}
}

func TestLifecycleVisibilityDefersProviderChecksToAuthorizedApply(t *testing.T) {
	for _, via := range []Via{ViaAPI, ViaCLI, ViaMCP, ViaConsole, ViaSystem} {
		for _, permitted := range []bool{false, true} {
			name := string(via) + "/unpermitted"
			if permitted {
				name = string(via) + "/unavailable"
			}
			t.Run(name, func(t *testing.T) {
				s, _ := newTestService(t)
				n := &lifecycleNetwork{policy: "configured", state: "ready"}
				s.cfg.Lifecycle = n
				lifecycleSave(t, s, "visibility", "one")
				unpublished, err := s.SetVisibility(t.Context(), "visibility", store.Public, via, "")
				if !errors.Is(err, ErrConflict) || unpublished.Approval != nil || len(n.requests) != 0 {
					t.Fatalf("unpublished public: %+v %v", unpublished, err)
				}
				same, err := s.SetVisibility(t.Context(), "visibility", store.Private, via, "")
				if err != nil || same.Status != "done" || same.Approval != nil {
					t.Fatalf("same private: %+v %v", same, err)
				}
				lifecycleApprove(t, s, lifecycleRequest(t, s, "visibility"))
				if permitted {
					if err := s.SetProviderPermission(t.Context(), "visibility", store.ProviderPortal, true, ViaConsole); err != nil {
						t.Fatal(err)
					}
				}
				// With no public transport, request creation must still freeze the available policy.
				if permitted {
					s.cfg.Lifecycle = nil
				}
				count := len(n.requests)
				history, _ := s.Deployments(t.Context(), "visibility")
				data := filepath.Join(s.dataDirOf("visibility"), "files", "sentinel")
				if err := writeFile(data, "unchanged"); err != nil {
					t.Fatal(err)
				}
				r, err := s.SetVisibility(t.Context(), "visibility", store.Public, via, "request")
				if err != nil || r.Status != "pending_approval" || r.Approval == nil {
					t.Fatalf("request must be pending: %+v %v", r, err)
				}
				if len(n.requests) != count {
					t.Fatal("network started before approval")
				}
				a, err := s.Decide(t.Context(), r.Approval.ID, true)
				want := ErrProviderNotPermitted
				code := "provider_not_permitted"
				if permitted {
					want = ErrUnavailable
					code = "provider_unavailable"
				}
				if !errors.Is(err, want) || a.Status != "failed" || execution(t, a).FailureCode != code {
					t.Fatalf("authorized apply: %+v %v", a, err)
				}
				f, _ := s.GetFlat(t.Context(), "visibility")
				after, _ := s.Deployments(t.Context(), "visibility")
				b, err := os.ReadFile(data)
				if err != nil || string(b) != "unchanged" || f.Visibility != store.Private || f.LiveVersion != 1 || len(after) != len(history) || liveBytes(t, s, "visibility") != "one" || len(n.requests) != count {
					t.Fatalf("failed apply changed current/data/history: %+v %v", f, err)
				}
				// Fresh approved request succeeds with explicit permission and configured available provider.
				s.cfg.Lifecycle = n
				if err := s.SetProviderPermission(t.Context(), "visibility", store.ProviderPortal, true, ViaConsole); err != nil {
					t.Fatal(err)
				}
				r, err = s.SetVisibility(t.Context(), "visibility", store.Public, via, "fresh")
				if err != nil || r.Approval == nil {
					t.Fatalf("fresh request %+v %v", r, err)
				}
				if len(n.requests) != count {
					t.Fatal("fresh request started network")
				}
				if _, err := s.Decide(t.Context(), r.Approval.ID, true); err != nil {
					t.Fatal(err)
				}
				count = len(n.requests)
				unchanged, err := s.SetVisibility(t.Context(), "visibility", store.Public, via, "")
				if err != nil || unchanged.Status != "done" || unchanged.Approval != nil {
					t.Fatalf("same value: %+v %v", unchanged, err)
				}
				r, err = s.SetVisibility(t.Context(), "visibility", store.Private, via, "")
				if err != nil || r.Approval == nil || len(n.requests) != count || n.public == nil {
					t.Fatalf("private request %+v %v", r, err)
				}
				f, _ = s.GetFlat(t.Context(), "visibility")
				if f.Visibility != store.Public {
					t.Fatal("pending changed Public")
				}
				if _, err := s.Decide(t.Context(), r.Approval.ID, true); err != nil {
					t.Fatal(err)
				}
				f, _ = s.GetFlat(t.Context(), "visibility")
				if f.Visibility != store.Private || f.LiveVersion != 1 || n.public != nil {
					t.Fatalf("private apply %+v", f)
				}
			})
		}
	}
}

func TestPublicApprovalWithoutCurrentRuntimeFailsPrecisely(t *testing.T) {
	s, _ := newTestService(t)
	n := &lifecycleNetwork{policy: "configured", state: "ready"}
	s.cfg.Lifecycle = n
	lifecycleSave(t, s, "stopped", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "stopped"))
	if err := s.SetProviderPermission(t.Context(), "stopped", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	s.state("stopped").cur.Store(nil)
	r, err := s.SetVisibility(t.Context(), "stopped", store.Public, ViaAPI, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Decide(t.Context(), r.Approval.ID, true)
	if !errors.Is(err, ErrNotDeployed) || a.Status != "failed" || execution(t, a).FailureCode != "not_deployed" {
		t.Fatalf("missing current %+v %v", a, err)
	}
	f, _ := s.GetFlat(t.Context(), "stopped")
	if f.Visibility != store.Private || n.public != nil {
		t.Fatal("Public committed without current route")
	}
}

func TestLegacyVisibilityApprovalRequiresFreshFrozenPolicy(t *testing.T) {
	s, _ := newTestService(t)
	lifecycleSave(t, s, "legacy-policy", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "legacy-policy"))
	a := store.Approval{ID: "legacy-visibility", Flat: "legacy-policy", Action: "set_visibility", Params: []byte(`{"visibility":"public","from":"private"}`), Status: "pending", Via: "api", RequestedAt: s.now()}
	if err := s.st.InsertApproval(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	got, err := s.Decide(t.Context(), a.ID, true)
	if !errors.Is(err, ErrStaleApproval) || execution(t, got).FailureCode != "stale_approval" || !strings.Contains(got.Result, "legacy visibility approval lacks frozen access policy") || strings.Contains(got.Result, "live version changed") {
		t.Fatalf("legacy cause %+v %v", got, err)
	}
}

func TestJoinedErrorPrecedence(t *testing.T) {
	for _, err := range []error{errors.Join(ErrProviderNotReady, ErrProviderUnavailable), errors.Join(ErrProviderUnavailable, ErrProviderNotReady)} {
		if failureCode(err) != "provider_unavailable" || ErrorCategory(err) != "provider_unavailable" {
			t.Fatal("joined cause precedence", err)
		}
	}
	if failureCode(errors.Join(ErrUnchangedContent, ErrProviderNotReady)) != "unchanged_content" {
		t.Fatal("unchanged cause precedence")
	}
	for _, err := range []error{
		errors.Join(ErrProviderNotReady, ErrPublicStopUnconfirmed),
		errors.Join(ErrPublicStopUnconfirmed, ErrProviderUnavailable),
	} {
		if failureCode(err) != "public_stop_unconfirmed" || ErrorCategory(err) != "public_stop_unconfirmed" {
			t.Fatal("unconfirmed public route must outrank typed provider cause", err)
		}
	}
}

func TestDraftDirtyFollowsApprovedCurrentVersion(t *testing.T) {
	s, _ := newTestService(t)
	lifecycleSave(t, s, "dirty", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "dirty"))
	lifecycleSave(t, s, "dirty", "two")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "dirty"))
	assertDirty := func(want bool) {
		t.Helper()
		d, err := s.GetDraft(t.Context(), "dirty")
		if err != nil || d.Dirty != want {
			t.Fatalf("draft dirty=%t want=%t err=%v", d.Dirty, want, err)
		}
		view, err := s.GetFlat(t.Context(), "dirty")
		if err != nil || view.Draft == nil || view.Draft.Dirty != want {
			t.Fatalf("view draft %+v %v", view.Draft, err)
		}
	}
	assertDirty(false)
	_, err := s.Rollback(t.Context(), "dirty", 1, ViaAPI)
	var pending *PendingApproval
	if !errors.As(err, &pending) {
		t.Fatal(err)
	}
	assertDirty(false) // requesting a change does not change live or Draft.
	lifecycleApprove(t, s, pending)
	assertDirty(true)
	lifecycleSave(t, s, "dirty", "one")
	assertDirty(false)
	_, err = s.Deploy(t.Context(), "dirty", 2, ViaAPI)
	if !errors.As(err, &pending) {
		t.Fatal(err)
	}
	lifecycleApprove(t, s, pending)
	assertDirty(true)
	_, err = s.Rollback(t.Context(), "dirty", 1, ViaAPI)
	if !errors.As(err, &pending) {
		t.Fatal(err)
	}
	lifecycleApprove(t, s, pending)
	assertDirty(false)
}
