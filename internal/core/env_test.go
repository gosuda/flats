package core

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

func TestEnvironmentValidationAndReadableIsolation(t *testing.T) {
	s, _ := newTestService(t)
	ctx := t.Context()
	for _, slug := range []string{"app", "other"} {
		if _, err := s.CreateFlat(ctx, slug, "", ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"DB", "FILES", "__PROTO__", "CONSTRUCTOR", "PROTOTYPE", "__proto__", "bad", strings.Repeat("A", 65), "A=B"} {
		if err := s.SetEnv(ctx, "app", name, "x", ViaMCP); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: %v", name, err)
		}
		if err := s.SetSecret(ctx, "app", name, "x", ViaCLI); !errors.Is(err, ErrInvalid) {
			t.Errorf("secret name %q: %v", name, err)
		}
	}
	for _, value := range []string{"nul\x00value", string([]byte{0xff}), strings.Repeat("x", (64<<10)+1)} {
		if err := s.SetEnv(ctx, "app", "MODE", value, ViaAPI); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid value accepted: %v", err)
		}
	}
	if err := s.SetEnv(ctx, "app", "MODE", "", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(ctx, "app", "MODE", "readable", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(ctx, "app", "TOKEN", "s3c", ViaCLI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(ctx, "app", "TOKEN", "overwrite", ViaMCP); !errors.Is(err, ErrInvalid) {
		t.Fatalf("env collision: %v", err)
	}
	if err := s.SetSecret(ctx, "app", "MODE", "overwrite", ViaCLI); !errors.Is(err, ErrInvalid) {
		t.Fatalf("secret collision: %v", err)
	}
	if vars, err := s.ListEnv(ctx, "app"); err != nil || len(vars) != 1 || vars[0].Name != "MODE" || vars[0].Value != "readable" {
		t.Fatalf("ordinary list: %+v %v", vars, err)
	}
	if vars, err := s.ListEnv(ctx, "other"); err != nil || len(vars) != 0 {
		t.Fatalf("isolation: %+v %v", vars, err)
	}
	if _, err := s.ListEnv(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing flat: %v", err)
	}
}

func TestRuntimeEnvironmentSnapshotAndRedaction(t *testing.T) {
	s, _ := newTestService(t)
	ctx := t.Context()
	if _, err := s.CreateFlat(ctx, "app", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(ctx, "app", "MODE", "readable", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(ctx, "app", "TOKEN", "s3c", ViaCLI); err != nil {
		t.Fatal(err)
	}
	var specs []RuntimeSpec
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		specs = append(specs, spec)
		spec.Log("info", "config readable token s3c")
		return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s %s", spec.Env["MODE"], spec.Env["TOKEN"])
		})}, nil
	})
	v := store.Version{Kind: "server", Manifest: []byte(`{"entry":"server.js"}`)}
	old, err := s.build(ctx, "app", v, "unused")
	if err != nil {
		t.Fatal(err)
	}
	defer old.inst.Stop()
	if specs[0].Env["MODE"] != "readable" || specs[0].Env["TOKEN"] != "s3c" {
		t.Fatalf("runtime env: %+v", specs[0].Env)
	}
	if err := s.SetEnv(ctx, "app", "MODE", "new-value", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if specs[0].Env["MODE"] != "readable" {
		t.Fatal("configuration mutated running instance")
	}
	next, err := s.build(ctx, "app", v, "unused")
	if err != nil {
		t.Fatal(err)
	}
	defer next.inst.Stop()
	if specs[1].Env["MODE"] != "new-value" {
		t.Fatal("new instance did not capture update")
	}
	health := healthCheck(old.handler, "/", old.redact)
	if strings.Contains(health.BodyHead, "s3c") {
		t.Fatalf("health leaked: %+v", health)
	}
	events, err := s.st.ListEvents(ctx, "app", "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if strings.Contains(ev.Message, "s3c") {
			t.Fatal("event leaked secret")
		}
		if strings.Contains(ev.Message, "config readable token [redacted]") {
			found = true
		}
	}
	if !found {
		t.Fatal("ordinary value incorrectly redacted")
	}
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) { return nil, fmt.Errorf("startup token s3c") })
	if _, err := s.build(ctx, "app", v, "unused"); err == nil || strings.Contains(err.Error(), "s3c") {
		t.Fatalf("startup error: %v", err)
	}
	static, err := s.build(ctx, "app", store.Version{Kind: "static", Manifest: []byte(`{}`)}, "unused")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	static.handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(rec.Body.String(), "new-value") || strings.Contains(rec.Body.String(), "s3c") {
		t.Fatal("static response leaked config")
	}
}

func TestEnvUpdateActivationOnRedeploy(t *testing.T) {
	s, _ := newTestService(t)
	ctx := t.Context()
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, spec.Env["MODE"]) })}, nil
	})
	if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte(`export default {}`)}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(ctx, "app", "MODE", "before", ViaMCP); err != nil {
		t.Fatal(err)
	}
	lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
	if got := liveBytes(t, s, "app"); got != "before" {
		t.Fatalf("initial env %q", got)
	}
	if err := s.SetEnv(ctx, "app", "MODE", "after", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if got := liveBytes(t, s, "app"); got != "before" {
		t.Fatalf("write changed live env %q", got)
	}
	_, err := s.Deploy(ctx, "app", 1, ViaConsole)
	var pending *PendingApproval
	if !errors.As(err, &pending) {
		t.Fatalf("redeploy approval: %v", err)
	}
	lifecycleApprove(t, s, pending)
	if got := liveBytes(t, s, "app"); got != "after" {
		t.Fatalf("redeploy env %q", got)
	}
	if err := s.DeleteEnv(ctx, "app", "MODE", ViaMCP); err != nil {
		t.Fatal(err)
	}
	if got := liveBytes(t, s, "app"); got != "after" {
		t.Fatalf("delete changed live env %q", got)
	}
	_, err = s.Deploy(ctx, "app", 1, ViaConsole)
	if !errors.As(err, &pending) {
		t.Fatalf("redeploy approval: %v", err)
	}
	lifecycleApprove(t, s, pending)
	if got := liveBytes(t, s, "app"); got != "" {
		t.Fatalf("delete was not applied %q", got)
	}
}
