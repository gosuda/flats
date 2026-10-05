package core

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

func TestNetworkPolicyOperatorOnlyAndIsolation(t *testing.T) {
	s, _ := newTestService(t)
	ctx := t.Context()
	for _, slug := range []string{"app", "other"} {
		if _, err := s.CreateFlat(ctx, slug, "", ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	for _, via := range []Via{ViaAPI, ViaMCP, ViaSystem} {
		if err := s.SetNetworkPolicy(ctx, "app", []string{"https://api.example.com"}, via); !errors.Is(err, ErrForbidden) {
			t.Fatalf("via %s: %v", via, err)
		}
	}
	p, err := s.NetworkPolicy(ctx, "app")
	if err != nil || p.Origins == nil || len(p.Origins) != 0 {
		t.Fatalf("default: %+v %v", p, err)
	}
	if err := s.SetNetworkPolicy(ctx, "app", []string{"https://API.example.com:443/", "https://api.example.com"}, ViaCLI); err != nil {
		t.Fatal(err)
	}
	p, err = s.NetworkPolicy(ctx, "app")
	if err != nil || !slices.Equal(p.Origins, []string{"https://api.example.com"}) || p.UpdatedAt.IsZero() {
		t.Fatalf("policy: %+v %v", p, err)
	}
	for _, origin := range []string{"http://127.0.0.1", "https://user:password@example.com", "https://*.example.com", "file:///tmp/file", "https://example.com:8443"} {
		if err := s.SetNetworkPolicy(ctx, "app", []string{origin}, ViaConsole); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid origin accepted: %v", err)
		}
	}
	p, err = s.NetworkPolicy(ctx, "other")
	if err != nil || len(p.Origins) != 0 {
		t.Fatalf("isolation: %+v %v", p, err)
	}
	if err := s.SetNetworkPolicy(ctx, "app", nil, ViaConsole); err != nil {
		t.Fatal(err)
	}
	p, err = s.NetworkPolicy(ctx, "app")
	if err != nil || p.Origins == nil || len(p.Origins) != 0 {
		t.Fatalf("revoke: %+v %v", p, err)
	}
	if _, err := s.NetworkPolicy(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestNetworkPolicyNewPreviewsCaptureDesiredGrant(t *testing.T) {
	s, _ := newTestService(t)
	ctx := t.Context()
	var captured [][]string
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		captured = append(captured, slices.Clone(spec.NetworkOrigins))
		return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })}, nil
	})
	save := func(body string) {
		if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte(body)}}, SaveMeta{}, ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	save("first")
	if err := s.SetNetworkPolicy(ctx, "app", []string{"https://live.example.com"}, ViaCLI); err != nil {
		t.Fatal(err)
	}
	lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
	if err := s.SetNetworkPolicy(ctx, "app", []string{"https://preview.example.com"}, ViaCLI); err != nil {
		t.Fatal(err)
	}
	first, err := s.OpenPreview(ctx, "app", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := captured[len(captured)-1]; !slices.Equal(got, []string{"https://preview.example.com"}) {
		t.Fatalf("version preview grant: %v", got)
	}
	save("draft")
	if err := s.SetNetworkPolicy(ctx, "app", []string{"https://draft.example.com"}, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenPreview(ctx, "app", 0); err != nil {
		t.Fatal(err)
	}
	if got := captured[len(captured)-1]; !slices.Equal(got, []string{"https://draft.example.com"}) {
		t.Fatalf("draft preview grant: %v", got)
	}
	if got := s.state("app").cur.Load().environment.networkOrigins; !slices.Equal(got, []string{"https://live.example.com"}) {
		t.Fatalf("preview changed live grant: %v", got)
	}
	if err := s.ClosePreview(ctx, first.Host); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkPolicyHostRestartCapturesDesiredGrant(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var captured [][]string
	runtime := lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		captured = append(captured, slices.Clone(spec.NetworkOrigins))
		return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Join(spec.NetworkOrigins, ",")) })}, nil
	})
	cfg := Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Runtime: runtime, Logf: t.Logf}
	s, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte("first")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNetworkPolicy(ctx, "app", []string{"https://original.example.com"}, ViaCLI); err != nil {
		t.Fatal(err)
	}
	lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
	if err := s.SetNetworkPolicy(ctx, "app", []string{"https://restart.example.com"}, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if got := liveBytes(t, s, "app"); got != "https://original.example.com" {
		t.Fatalf("desired grant changed running instance: %q", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = nil
	captured = nil
	cfg.Private = &memNet{hosts: map[string]http.Handler{}}
	s, err = New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 || !slices.Equal(captured[0], []string{"https://restart.example.com"}) {
		t.Fatalf("host restoration grants: %v", captured)
	}
	if got := liveBytes(t, s, "app"); got != "https://restart.example.com" {
		t.Fatalf("restored grant: %q", got)
	}
}
