package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

func piNetwork(t *testing.T, s *Service) {
	t.Helper()
	if _, err := s.SetPermittedNetworks([]string{"portal", "tailscale"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(t.Context(), map[string]string{SetDefaultPublic: "portal"}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicInstanceStatusAndRacingSetters(t *testing.T) {
	s, logs := newAutopilotService(t, apNow)
	ctx := t.Context()
	if got := s.PublicInstance(); got != (PublicInstance{}) {
		t.Fatalf("default: %+v", got)
	}
	if _, err := s.SetPublicInstance(ctx, true, "portal", ""); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "no default public network") {
		t.Fatalf("missing network: %v", err)
	}
	piNetwork(t, s)
	if _, err := s.CreateFlat(ctx, "private", "", ViaMCP); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.SetPublicInstance(ctx, true, "portal", "operator"); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	status, err := s.PublicInstanceStatus(ctx)
	if err != nil || !status.Enabled || status.Network != "portal" || !status.Usable || status.Reason != "" || status.PrivateFlats != 1 || !status.UpdatedAt.Equal(apNow) {
		t.Fatalf("status: %+v, %v", status, err)
	}
	if lines := logs.matching("public instance:"); len(lines) != 1 || !strings.Contains(lines[0], "turned on by operator (at 2026-10-24T09:00:00Z)") {
		t.Fatalf("racing setters log: %v", lines)
	}
	if _, err := s.SetPublicInstance(ctx, true, "zrok", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale confirmation: %v", err)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDefaultPublic: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPublicInstance(ctx, false, "", ""); err != nil {
		t.Fatalf("off without network: %v", err)
	}
	if s.PublicInstance().Enabled {
		t.Fatal("off was not committed")
	}
	if f, err := s.st.GetFlat(ctx, "private"); err != nil || f.Visibility != store.Private {
		t.Fatalf("existing flat changed: %+v %v", f, err)
	}
}

type piPausedAutopilot struct {
	*store.Store
	once    sync.Once
	read    chan struct{}
	release chan struct{}
}

func (p *piPausedAutopilot) AutopilotSync(ctx context.Context) (store.AutopilotRow, []store.AutopilotChange, error) {
	row, changes, err := p.Store.AutopilotSync(ctx)
	p.once.Do(func() { close(p.read); <-p.release })
	return row, changes, err
}

func TestPublicInstanceInterleavedAutopilotSync(t *testing.T) {
	s, _ := newAutopilotService(t, apNow)
	piNetwork(t, s)
	p := &piPausedAutopilot{Store: s.st, read: make(chan struct{}), release: make(chan struct{})}
	s.ap.store = p
	synced := make(chan struct{})
	go func() { defer close(synced); s.syncAutopilot(t.Context()) }()
	<-p.read
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(p.release) }) })
	set := make(chan error, 1)
	go func() { _, err := s.SetPublicInstance(t.Context(), true, "portal", ""); set <- err }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !s.PublicInstance().Enabled {
		select {
		case err := <-set:
			t.Fatalf("setter returned before cache change: %v", err)
		case <-deadline.C:
			t.Fatal("committed cache blocked behind Autopilot sync")
		case <-tick.C:
		}
	}
	release.Do(func() { close(p.release) })
	<-synced
	if err := <-set; err != nil {
		t.Fatal(err)
	}
	if !s.PublicInstance().Enabled {
		t.Fatal("stale Autopilot sync regressed committed mode")
	}
}

func TestPublicInstanceSyncFailureDelaysOnlyLog(t *testing.T) {
	s, logs := newAutopilotService(t, apNow)
	piNetwork(t, s)
	f := &faultyAutopilotStore{Store: s.st, failSync: -1}
	s.ap.store = f
	status, err := s.SetPublicInstance(t.Context(), true, "portal", "operator")
	if err != nil || !status.Enabled || !s.PublicInstance().Enabled {
		t.Fatalf("committed value lost on sync failure: %+v %v", status, err)
	}
	if len(logs.matching("public instance:")) != 0 {
		t.Fatal("failed sync logged a line")
	}
	f.set(func(f *faultyAutopilotStore) { f.failSync = 0 })
	if _, err := s.syncAutopilot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(logs.matching("public instance: turned on by operator")) != 1 || !s.PublicInstance().Enabled {
		t.Fatal("recovery lost log or changed mode")
	}
}

func TestPublicInstanceReadFailureKeepsCommittedValue(t *testing.T) {
	s, _ := newAutopilotService(t, apNow)
	piNetwork(t, s)
	if _, err := s.SetPublicInstance(t.Context(), true, "portal", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublicInstanceStatus(t.Context()); err == nil {
		t.Fatal("closed store status succeeded")
	}
	if _, err := s.syncAutopilot(t.Context()); err == nil {
		t.Fatal("closed store sync succeeded")
	}
	if !s.PublicInstance().Enabled {
		t.Fatal("read failure turned public instance off")
	}
	h := s.linkGate(false, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/", nil))
	if r.Code != 403 {
		t.Fatalf("read failure reopened private link: %d", r.Code)
	}
}

func TestPublicInstanceStartupReadFailure(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db, err := sql.Open("sqlite", filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), "DROP TABLE public_instance"); err != nil {
		t.Fatal(err)
	}
	if s, err := New(t.Context(), Config{Store: st, DataDir: dir, Private: &memNet{hosts: map[string]http.Handler{}}}); err == nil {
		s.Close()
		t.Fatal("startup with unreadable mode succeeded")
	}
}

func TestPublicInstanceConnectionStateAndLinkCensus(t *testing.T) {
	for _, tc := range []struct {
		name, visibility, state string
		publicEndpoint          bool
	}{
		{"private", "private", "unavailable", true},
		{"public-error", "public", "error", true},
		{"public-starting", "public", "starting", true},
		{"public-ready", "public", "ready", true},
		{"public-missing", "public", "unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAddressHost(t, "local", []string{"tailscale", "portal"}, "tailscale", "portal")
			h.defaults(t, "tailscale", "portal")
			if tc.visibility == "public" {
				h.makePublic(t)
			}
			private := routeEndpoint(ProviderTailscale, "site", AudienceCurrent, "ready")
			private.URL = "https://distinctive-private-host-marker.test"
			eps := []ExposureEndpoint{private}
			if tc.publicEndpoint {
				eps = append(eps, routeEndpoint(ProviderPortal, "site", AudienceCurrent, tc.state))
			}
			h.net.set("site", eps...)
			before, err := json.Marshal(h.view(t))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(before), "distinctive-private-host-marker") || strings.Contains(string(before), "private_links_off") {
				t.Fatalf("off census: %s", before)
			}
			if _, err := h.s.SetPublicInstance(t.Context(), true, "portal", ""); err != nil {
				t.Fatal(err)
			}
			v := h.view(t)
			if v.ConnectionState != tc.state || !v.PrivateLinksOff || v.PrivateURL != "" || v.PrivateState != "" || v.PrivateDetail != "" {
				t.Fatalf("mode on: %+v", v)
			}
			if slices.ContainsFunc(v.Endpoints, func(ep ExposureEndpoint) bool { return !PublicProvider(ep.Provider) }) {
				t.Fatalf("private endpoint: %+v", v.Endpoints)
			}
			list, err := h.s.ListFlats(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []any{v, list, h.s.previewView(t.Context(), store.Preview{Flat: "site", Host: "preview", LastAccess: apNow})} {
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "distinctive-private-host-marker") {
					t.Fatalf("private marker leaked: %s", raw)
				}
			}
			if _, err := h.s.SetPublicInstance(t.Context(), false, "", ""); err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(h.view(t))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("off view changed:\nbefore %s\nafter %s", before, after)
			}
		})
	}
}

func TestPublicInstanceOpenPreviewRefused(t *testing.T) {
	s, _ := newAutopilotService(t, apNow)
	piNetwork(t, s)
	if _, err := s.SetPublicInstance(t.Context(), true, "portal", ""); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{0, 1} {
		if _, err := s.OpenPreview(t.Context(), "missing", version); !errors.Is(err, ErrConflict) || !errors.Is(err, ErrPublicInstanceOn) || !strings.Contains(err.Error(), "previews are private links and public instance mode is on") {
			t.Fatalf("preview %d refusal: %v", version, err)
		}
	}
}
