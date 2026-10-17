package provider

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// stateTail is a fake tailnet whose reported host state can change.
type stateTail struct {
	fakeTail
	mu       sync.Mutex
	state    string
	statuses int
}

func (s *stateTail) set(state string) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
}

func (s *stateTail) Status() core.NetStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses++
	return core.NetStatus{Kind: "tailscale", Enabled: true, Hosts: []core.HostInfo{{Host: "notes", URL: "https://notes.example.ts.net", State: s.state}}}
}

func countOf(counts []EndpointCount, id ID, state string) int {
	for _, c := range counts {
		if c.Provider == id && c.State == state {
			return c.N
		}
	}
	return 0
}

// EndpointStates reports the fresh backend state of every observed endpoint,
// like ExposureStatus, without the per-flat permission callback.
func TestEndpointStatesFollowBackendState(t *testing.T) {
	m, ln := managerWith(t, File{Version: 1, Permitted: []ID{Tailscale}})
	defer ln.Close()
	defer m.Close()
	tail := &stateTail{state: "starting"}
	m.ts = tail
	m.permission = func(context.Context, string, ID) (bool, error) {
		t.Error("EndpointStates consulted the permission callback")
		return true, nil
	}
	ctx := context.Background()
	for _, slug := range []string{"notes", "other"} {
		if _, err := m.ServeExposure(ctx, ExposureRequest{Slug: slug, Visibility: "private", Audience: AudienceCurrent,
			Handler: text(slug), Permitted: []ID{Tailscale}}); err != nil {
			t.Fatal(err)
		}
	}
	got := m.EndpointStates()
	// "other" is not in the backend's host list yet: starting, like "notes".
	if countOf(got, Tailscale, "starting") != 2 || countOf(got, Local, "ready") != 2 {
		t.Fatalf("starting routes: %+v", got)
	}
	tail.set("ready")
	got = m.EndpointStates()
	if countOf(got, Tailscale, "ready") != 1 || countOf(got, Tailscale, "starting") != 1 {
		t.Fatalf("after notes became ready: %+v", got)
	}
	if !slices.IsSortedFunc(got, func(a, b EndpointCount) int {
		return cmp.Or(cmp.Compare(a.Provider, b.Provider), cmp.Compare(a.State, b.State))
	}) {
		t.Fatalf("unsorted: %+v", got)
	}
	// A registration that is gone is reported stopped, as ExposureStatus does.
	if _, ok := m.take("other", Tailscale, true); !ok {
		t.Fatal("missing tailscale route")
	}
	got = m.EndpointStates()
	if countOf(got, Tailscale, stateStopped) != 1 || countOf(got, Tailscale, "ready") != 1 {
		t.Fatalf("after a route stopped: %+v", got)
	}
	m.permission = nil
	status, err := m.ExposureStatus(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range status.Endpoints {
		if ep.Provider == Tailscale && ep.State != stateStopped {
			t.Fatalf("ExposureStatus and EndpointStates disagree: %+v", ep)
		}
	}
}
