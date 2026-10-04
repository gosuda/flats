package core

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Old releases accepted these reserved names and arbitrary secret bytes. An
// upgrade must decrypt them unchanged, while rejecting equivalent new writes.
func TestLegacySecretsRemainReadableByRuntime(t *testing.T) {
	s, _ := newTestService(t)
	ctx := t.Context()
	if _, err := s.CreateFlat(ctx, "legacy", "", ViaCLI); err != nil {
		t.Fatal(err)
	}
	g, err := s.aead()
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string]string{"PROTOTYPE": "prototype-value", "CONSTRUCTOR": "constructor-value", "__PROTO__": "proto-value", "DB": "legacy-db", "FILES": "legacy-files", "BYTES": "legacy\x00\xffvalue"}
	for name, value := range legacy {
		nonce := make([]byte, g.NonceSize())
		ciphertext := g.Seal(nil, nonce, []byte(value), []byte(name))
		if err := s.st.PutSecret(ctx, "legacy", store.SealedSecret{Name: name, Nonce: nonce, Ciphertext: ciphertext, UpdatedAt: s.now()}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSecret(ctx, "legacy", name, value, ViaCLI); !errors.Is(err, ErrInvalid) {
			t.Fatalf("new write %s: %v", name, err)
		}
	}
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		if !maps.Equal(spec.Env, legacy) {
			t.Fatalf("legacy configuration changed: got %q want %q", spec.Env, legacy)
		}
		return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })}, nil
	})
	d, err := s.build(ctx, "legacy", store.Version{Kind: "server", Manifest: []byte(`{"entry":"server.js"}`)}, "unused")
	if err != nil {
		t.Fatal(err)
	}
	d.inst.Stop()
	vars, err := s.ListEnv(ctx, "legacy")
	if err != nil || len(vars) != 0 {
		t.Fatalf("legacy secrets exposed as ordinary vars: %+v %v", vars, err)
	}
}

func TestDeploymentUsesCheckedEnvironmentDespiteConcurrentWrites(t *testing.T) {
	s, _ := newTestService(t)
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) { return nil, errors.New("unused runtime") })
	ctx := t.Context()
	if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte(`export default {}`)}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(ctx, "app", "MODE", "original", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(ctx, "app", "TOKEN", "original-secret", ViaCLI); err != nil {
		t.Fatal(err)
	}
	starts := 0
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		starts++
		start := starts
		mode, token := spec.Env["MODE"], spec.Env["TOKEN"]
		return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if start == 1 || start == 3 {
				// A settings write must complete during health checking without deadlocking
				// on the deployment lock. It applies only to the following activation.
				done := make(chan error, 1)
				go func() {
					if err := s.SetEnv(ctx, "app", "MODE", fmt.Sprintf("updated-%d", start), ViaAPI); err != nil {
						done <- err
						return
					}
					done <- s.SetSecret(ctx, "app", "TOKEN", fmt.Sprintf("updated-secret-%d", start), ViaCLI)
				}()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("settings write blocked during deployment health check")
				}
				// Runtime implementations must not mutate the frozen live-start snapshot.
				spec.Env["MODE"] = "mutated-runtime-map"
				spec.Env["TOKEN"] = "mutated-runtime-secret"
			}
			fmt.Fprintf(w, "%s|%s", mode, token)
		})}, nil
	})
	lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
	if got := liveBytes(t, s, "app"); got != "original|original-secret" {
		t.Fatalf("publish applied unchecked settings: %q", got)
	}
	for _, want := range []string{"updated-1|updated-secret-1", "updated-3|updated-secret-3"} {
		_, err := s.Deploy(ctx, "app", 1, ViaConsole)
		var pending *PendingApproval
		if !errors.As(err, &pending) {
			t.Fatal(err)
		}
		lifecycleApprove(t, s, pending)
		if got := liveBytes(t, s, "app"); got != want {
			t.Fatalf("redeploy environment: got %q want %q", got, want)
		}
	}
	events, err := s.st.ListEvents(ctx, "app", "health", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if strings.Contains(ev.Message, "original-secret") || strings.Contains(ev.Message, "updated-secret-") || strings.Contains(string(ev.Data), "original-secret") || strings.Contains(string(ev.Data), "updated-secret-") {
			t.Fatalf("checked snapshot secret leaked: %+v", ev)
		}
	}
}

func TestRestoreUsesCheckedEnvironmentAndFailureRetainsPrevious(t *testing.T) {
	for _, failLive := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail-live-%v", failLive), func(t *testing.T) {
			s, _ := newTestService(t)
			ctx := t.Context()
			s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
				return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, "%s|%s", spec.Env["MODE"], spec.Env["TOKEN"])
				})}, nil
			})
			save := func(body string) {
				if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte(body)}}, SaveMeta{}, ViaAPI); err != nil {
					t.Fatal(err)
				}
			}
			configure := func(value string) {
				if err := s.SetEnv(ctx, "app", "MODE", value, ViaAPI); err != nil {
					t.Fatal(err)
				}
				if err := s.SetSecret(ctx, "app", "TOKEN", value+"-secret", ViaCLI); err != nil {
					t.Fatal(err)
				}
			}
			save("version-one")
			configure("first")
			lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
			if err := writeFile(filepath.Join(s.dataDirOf("app"), "files", "note"), "before"); err != nil {
				t.Fatal(err)
			}
			save("version-two")
			configure("previous-live")
			lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
			if err := writeFile(filepath.Join(s.dataDirOf("app"), "files", "note"), "after"); err != nil {
				t.Fatal(err)
			}
			configure("checked")
			starts := 0
			s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
				starts++
				start := starts
				mode, token := spec.Env["MODE"], spec.Env["TOKEN"]
				if start == 2 && failLive {
					return nil, errors.New("live restore startup rejected")
				}
				return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if start == 1 {
						done := make(chan error, 1)
						go func() {
							if err := s.SetEnv(ctx, "app", "MODE", "pending", ViaAPI); err != nil {
								done <- err
								return
							}
							done <- s.SetSecret(ctx, "app", "TOKEN", "pending-secret", ViaCLI)
						}()
						select {
						case err := <-done:
							if err != nil {
								t.Fatal(err)
							}
						case <-time.After(3 * time.Second):
							t.Fatal("settings write blocked during restore trial")
						}
						spec.Env["MODE"] = "runtime-mutation"
					}
					fmt.Fprintf(w, "%s|%s", mode, token)
				})}, nil
			})
			_, err := s.RollbackWithData(ctx, "app", 1, true, ViaConsole)
			var pending *PendingApproval
			if !errors.As(err, &pending) {
				t.Fatal(err)
			}
			receipt, err := s.Decide(ctx, pending.Approval.ID, true)
			want, note := "checked|checked-secret", "before"
			if failLive {
				if err == nil || receipt.Status != "failed" {
					t.Fatalf("restore failure: %+v %v", receipt, err)
				}
				want, note = "previous-live|previous-live-secret", "after"
			} else if err != nil {
				t.Fatal(err)
			}
			if got := liveBytes(t, s, "app"); got != want {
				t.Fatalf("restored runtime: got %q want %q", got, want)
			}
			if got, _ := readFile(filepath.Join(s.dataDirOf("app"), "files", "note")); got != note {
				t.Fatalf("restore data: got %q want %q", got, note)
			}
		})
	}
}

func TestEnvironmentCaptureFailureReportsUntouchedData(t *testing.T) {
	for _, action := range []string{"publish", "deploy", "restore"} {
		t.Run(action, func(t *testing.T) {
			s, _ := newTestService(t)
			ctx := t.Context()
			starts := 0
			s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
				starts++
				return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "version-%d", spec.Version) })}, nil
			})
			save := func(body string) {
				if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte(body)}}, SaveMeta{}, ViaAPI); err != nil {
					t.Fatal(err)
				}
			}
			save("first")
			if err := s.SetSecret(ctx, "app", "TOKEN", "do-not-leak-value", ViaCLI); err != nil {
				t.Fatal(err)
			}
			lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
			notePath := filepath.Join(s.dataDirOf("app"), "files", "note")
			if err := writeFile(notePath, "snapshot-data"); err != nil {
				t.Fatal(err)
			}
			save("second")
			lifecycleApprove(t, s, lifecycleRequest(t, s, "app"))
			if err := writeFile(notePath, "current-data"); err != nil {
				t.Fatal(err)
			}
			var pending *PendingApproval
			var err error
			wantVersion := 1
			switch action {
			case "publish":
				save("third")
				pending = lifecycleRequest(t, s, "app")
				wantVersion = 0 // publication number is allocated only after health passes
			case "deploy":
				_, err = s.Deploy(ctx, "app", 1, ViaConsole)
			case "restore":
				_, err = s.RollbackWithData(ctx, "app", 1, true, ViaConsole)
			}
			if action != "publish" && !errors.As(err, &pending) {
				t.Fatal(err)
			}
			// Corruption can arise from a mismatched restored host key/database pair.
			// It must fail before any health runtime or live data mutation.
			if err := s.st.PutSecret(ctx, "app", store.SealedSecret{Name: "TOKEN", Nonce: make([]byte, 12), Ciphertext: []byte("invalid-sealed-data"), UpdatedAt: s.now()}); err != nil {
				t.Fatal(err)
			}
			beforeStarts := starts
			receipt, err := s.Decide(ctx, pending.Approval.ID, true)
			var de *DeployError
			if !errors.As(err, &de) {
				t.Fatalf("missing DeployError: %v", err)
			}
			if de.Version != wantVersion || de.Previous != 2 || de.Data != dataUntouched {
				t.Fatalf("lost activation context: %+v", de)
			}
			dto := execution(t, receipt)
			if receipt.Status != "failed" || dto.DataImpact != "none" || dto.HealthData != "not_run" || dto.LiveData != "untouched" {
				t.Fatalf("incorrect receipt: %+v %+v", receipt, dto)
			}
			if starts != beforeStarts {
				t.Fatal("runtime started despite capture failure")
			}
			if got := liveBytes(t, s, "app"); got != "version-2" {
				t.Fatalf("previous live runtime changed: %q", got)
			}
			if got, _ := readFile(notePath); got != "current-data" {
				t.Fatalf("live data changed: %q", got)
			}
			if strings.Contains(err.Error(), "do-not-leak-value") || strings.Contains(receipt.Result, "do-not-leak-value") {
				t.Fatal("capture failure leaked secret")
			}
		})
	}
}
