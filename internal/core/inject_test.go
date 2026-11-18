package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/egress"
	"github.com/gosuda/flats/internal/store"
)

// The service checks of docs/spec/injected-fetch-secrets.md.

const coreInjected = "Bearer distinctive-6d1e"

var apiBinding = &SecretInject{Origin: "https://api.example.com", Header: "authorization"}

// injectService is a service with the flat app, the readable secret PLAIN and the injected secret TOKEN.
func injectService(t *testing.T) (*Service, string) {
	t.Helper()
	s, dir := newTestService(t)
	ctx := t.Context()
	if _, err := s.CreateFlat(ctx, "app", "", ViaCLI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(ctx, "app", "PLAIN", "plain-value", ViaCLI); err != nil {
		t.Fatal(err)
	}
	got, err := s.SetSecretBinding(ctx, "app", "TOKEN", coreInjected, apiBinding, ViaCLI)
	if err != nil || got == nil || *got != (SecretInject{Origin: "https://api.example.com", Header: "Authorization"}) {
		t.Fatalf("SetSecretBinding = %+v, %v", got, err)
	}
	return s, dir
}

func noValue(t *testing.T, what, text string) {
	t.Helper()
	if strings.Contains(text, "distinctive-6d1e") {
		t.Errorf("%s holds the injected value: %s", what, text)
	}
}

func TestSetInjectedSecretRefusals(t *testing.T) {
	t.Parallel()
	s, _ := injectService(t)
	ctx := t.Context()
	other := &SecretInject{Origin: "https://other.example.com", Header: "Authorization"}
	for name, tc := range map[string]struct {
		secret, value string
		inject        *SecretInject
		via           Via
		kind          error
		says          []string
		hides         []string
	}{
		"an agent":                    {"NEW", coreInjected, apiBinding, ViaMCP, ErrForbidden, nil, nil},
		"an http origin":              {"NEW", coreInjected, &SecretInject{Origin: "http://api.example.com", Header: "X-Key"}, ViaCLI, ErrInvalid, []string{"https"}, nil},
		"an origin with credentials":  {"NEW", coreInjected, &SecretInject{Origin: "https://user:pass-secret@api.example.com/?q=query-secret", Header: "X-Key"}, ViaCLI, ErrInvalid, nil, []string{"pass-secret", "query-secret"}},
		"a header that the host sets": {"NEW", coreInjected, &SecretInject{Origin: "https://api.example.com", Header: "Host"}, ViaCLI, ErrInvalid, []string{"Host"}, nil},
		"content type":                {"NEW", coreInjected, &SecretInject{Origin: "https://api.example.com", Header: "content-type"}, ViaCLI, ErrInvalid, []string{"Content-Type"}, nil},
		"a new value with a newline":  {"NEW", "Bearer distinctive-6d1e\nX: y", other, ViaCLI, ErrInvalid, []string{"flats secret set app NEW"}, nil},
		"a binding on PLAIN":          {"PLAIN", coreInjected, apiBinding, ViaCLI, ErrConflict, []string{"env.PLAIN", "flats secret rm app PLAIN", "new credential"}, nil},
		"another binding":             {"TOKEN", coreInjected, other, ViaCLI, ErrConflict, []string{"Authorization to https://api.example.com", "flats secret rm app TOKEN"}, nil},
		"a binding of TOKEN":          {"NEW", coreInjected, apiBinding, ViaCLI, ErrConflict, []string{`"TOKEN"`, "flats secret rm app TOKEN", "flats secret set app TOKEN"}, nil},
		"an empty value of TOKEN":     {"TOKEN", "", nil, ViaConsole, ErrInvalid, []string{"cannot be empty", "Nothing changed"}, nil},
		"a control character":         {"TOKEN", "Bearer distinctive-6d1e\x01", nil, ViaConsole, ErrInvalid, []string{"control character"}, nil},
		"a trailing space":            {"TOKEN", "Bearer distinctive-6d1e ", nil, ViaCLI, ErrInvalid, []string{"space"}, nil},
	} {
		_, err := s.SetSecretBinding(ctx, "app", tc.secret, tc.value, tc.inject, tc.via)
		if !errors.Is(err, tc.kind) {
			t.Errorf("%s: %v, want %v", name, err, tc.kind)
			continue
		}
		for _, part := range tc.says {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("%s: the message lacks %q: %v", name, part, err)
			}
		}
		for _, part := range tc.hides {
			if strings.Contains(err.Error(), part) {
				t.Errorf("%s: the message repeats %q: %v", name, part, err)
			}
		}
		noValue(t, name, err.Error())
	}
	// Nothing changed: TOKEN keeps its value and binding, and the capture passes.
	var spec RuntimeSpec
	s.cfg.Runtime = lifecycleRuntime(func(got RuntimeSpec) (Instance, error) {
		spec = got
		return lifecycleInstance{http.NotFoundHandler()}, nil
	})
	d, err := s.build(ctx, "app", store.Version{Kind: "server", Manifest: []byte(`{"entry":"server.js"}`)}, "unused", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.inst.Stop()
	want := []egress.Header{{Origin: "https://api.example.com", Name: "Authorization", Value: coreInjected}}
	if fmt.Sprint(spec.FetchHeaders) != fmt.Sprint(want) {
		t.Fatalf("FetchHeaders = %+v", spec.FetchHeaders)
	}
	secs, err := s.SecretNames(ctx, "app")
	if err != nil || len(secs) != 2 {
		t.Fatalf("SecretNames = %+v, %v", secs, err)
	}
	for _, sec := range secs {
		if sec.Name == "NEW" {
			t.Error("a refused set made NEW")
		}
	}
}

func TestSecretLimitAndEnvName(t *testing.T) {
	t.Parallel()
	s, _ := injectService(t)
	ctx := t.Context()
	for i := 1; i < store.MaxInjectedSecrets; i++ {
		if _, err := s.SetSecretBinding(ctx, "app", fmt.Sprintf("K%d", i), "v", &SecretInject{Origin: "https://api.example.com", Header: fmt.Sprintf("X-Key-%d", i)}, ViaCLI); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.SetSecretBinding(ctx, "app", "MORE", "v", &SecretInject{Origin: "https://api.example.com", Header: "X-More"}, ViaCLI)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "flats secret ls app") || !strings.Contains(err.Error(), "flats secret rm") {
		t.Fatalf("one more: %v", err)
	}
	if err := s.SetEnv(ctx, "app", "TOKEN", "x", ViaCLI); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an ordinary variable with the name of an injected secret: %v", err)
	}
}

func TestInjectedSecretCapture(t *testing.T) {
	t.Parallel()
	s, _ := injectService(t)
	ctx := t.Context()
	var specs []RuntimeSpec
	s.cfg.Runtime = lifecycleRuntime(func(spec RuntimeSpec) (Instance, error) {
		specs = append(specs, spec)
		spec.Log("info", "upstream said "+coreInjected)
		return lifecycleInstance{http.NotFoundHandler()}, nil
	})
	server := store.Version{Kind: "server", Manifest: []byte(`{"entry":"server.js"}`)}
	build := func(slug string, v store.Version) RuntimeSpec {
		t.Helper()
		d, err := s.build(ctx, slug, v, "unused", nil)
		if err != nil {
			t.Fatal(err)
		}
		d.inst.Stop()
		return specs[len(specs)-1]
	}
	spec := build("app", server)
	if spec.Env["PLAIN"] != "plain-value" || len(spec.Env) != 1 {
		t.Fatalf("Env = %v", spec.Env)
	}
	if len(spec.FetchHeaders) != 1 || spec.FetchHeaders[0].Value != coreInjected || spec.FetchHeaders[0].Name != "Authorization" {
		t.Fatalf("FetchHeaders = %+v", spec.FetchHeaders)
	}
	events, err := s.st.ListEvents(ctx, "app", "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	logged := false
	for _, ev := range events {
		noValue(t, "an event", ev.Message)
		logged = logged || strings.Contains(ev.Message, "upstream said [redacted]")
	}
	if !logged {
		t.Error("the log line of the worker is missing")
	}
	// A new value without a binding keeps the binding.
	if err := s.SetSecret(ctx, "app", "TOKEN", "Bearer second-value", ViaConsole); err != nil {
		t.Fatal(err)
	}
	if spec := build("app", server); len(spec.FetchHeaders) != 1 || spec.FetchHeaders[0].Value != "Bearer second-value" || spec.Env["TOKEN"] != "" {
		t.Fatalf("after a new value: %+v %v", spec.FetchHeaders, spec.Env)
	}
	// A WASI flat gets no list.
	if spec := build("app", store.Version{Kind: "server", Manifest: []byte(`{"entry":"server.wasm"}`)}); spec.FetchHeaders != nil || spec.Env["TOKEN"] != "" {
		t.Fatalf("WASI: %+v %v", spec.FetchHeaders, spec.Env)
	}
	// A rename keeps the binding.
	if _, err := s.RenameSlug(ctx, "app", "renamed", ViaConsole); err != nil {
		t.Fatal(err)
	}
	if spec := build("renamed", server); len(spec.FetchHeaders) != 1 || spec.FetchHeaders[0].Origin != "https://api.example.com" {
		t.Fatalf("after a rename: %+v", spec.FetchHeaders)
	}
}

func TestDocsAppGetsNoInjectedSecret(t *testing.T) {
	t.Parallel()
	s, dir := newTestService(t)
	rt := &contentRuntime{}
	s.cfg.Runtime = rt
	s.cfg.DocsApp = testDocsApp()
	v := saveDocsDraft(t, s, "notes")
	if _, err := s.SetSecretBinding(t.Context(), "notes", "TOKEN", coreInjected, apiBinding, ViaCLI); err != nil {
		t.Fatal(err)
	}
	d, err := s.build(context.Background(), "notes", v, filepath.Join(dir, "isolated"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.inst.Stop()
	if spec := rt.specs[0]; spec.FetchHeaders != nil || spec.NetworkOrigins != nil || spec.Env["TOKEN"] != "" {
		t.Fatalf("docs app: %+v", spec)
	}
}

// rawDB opens flats.db beside the store, for rows that only a direct write can make.
func rawDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestStoredBindingsThatRefuseTheActivation(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		sql    []string
		secret string
	}{
		"only an origin":     {[]string{`UPDATE secrets SET fetch_header='' WHERE name='TOKEN'`}, "TOKEN"},
		"only a header":      {[]string{`UPDATE secrets SET fetch_origin='' WHERE name='TOKEN'`}, "TOKEN"},
		"an http origin":     {[]string{`UPDATE secrets SET fetch_origin='http://api.example.com' WHERE name='TOKEN'`}, "TOKEN"},
		"a bad header name":  {[]string{`UPDATE secrets SET fetch_header='Host' WHERE name='TOKEN'`}, "TOKEN"},
		"a bad value":        {[]string{`UPDATE secrets SET fetch_origin='https://other.example.com', fetch_header='X-Key' WHERE name='MULTI'`}, "MULTI"},
		"two with a binding": {[]string{`DROP INDEX secrets_fetch_binding`, `UPDATE secrets SET fetch_origin='https://api.example.com', fetch_header='Authorization' WHERE name='SECOND'`}, "TOKEN"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, dir := injectService(t)
			ctx := t.Context()
			if err := s.SetSecret(ctx, "app", "MULTI", "line one\nline two", ViaCLI); err != nil {
				t.Fatal(err)
			}
			if err := s.SetSecret(ctx, "app", "SECOND", "Bearer second", ViaCLI); err != nil {
				t.Fatal(err)
			}
			db := rawDB(t, dir)
			for _, q := range tc.sql {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			started := false
			s.cfg.Runtime = lifecycleRuntime(func(RuntimeSpec) (Instance, error) {
				started = true
				return lifecycleInstance{http.NotFoundHandler()}, nil
			})
			_, err := s.build(ctx, "app", store.Version{Kind: "server", Manifest: []byte(`{"entry":"server.js"}`)}, "unused", nil)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `"`+tc.secret+`"`) || !strings.Contains(err.Error(), "flats secret rm app "+tc.secret) {
				t.Fatalf("build: %v", err)
			}
			if started {
				t.Fatal("the runtime started")
			}
			noValue(t, "the refusal", err.Error())
			if strings.Contains(err.Error(), "line one") || strings.Contains(err.Error(), "Bearer second") {
				t.Fatalf("the refusal holds a value: %v", err)
			}
		})
	}
	t.Run("more than 32", func(t *testing.T) {
		t.Parallel()
		s, dir := injectService(t)
		ctx := t.Context()
		db := rawDB(t, dir)
		for i := range store.MaxInjectedSecrets {
			name := fmt.Sprintf("K%d", i)
			if err := s.SetSecret(ctx, "app", name, "v", ViaCLI); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE secrets SET fetch_origin='https://api.example.com', fetch_header=? WHERE name=?`, fmt.Sprintf("X-Key-%d", i), name); err != nil {
				t.Fatal(err)
			}
		}
		_, err := s.build(ctx, "app", store.Version{Kind: "server", Manifest: []byte(`{"entry":"server.js"}`)}, "unused", nil)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "flats secret rm") {
			t.Fatalf("build: %v", err)
		}
	})
}

// No text of the host holds an injected value (rule 23): the refusals, a capture refusal through a deploy, the
// listings, the flat events and the audit log.
func TestNoHostTextHoldsAnInjectedValue(t *testing.T) {
	t.Parallel()
	s, dir := injectService(t)
	ctx := t.Context()
	s.cfg.Runtime = lifecycleRuntime(func(RuntimeSpec) (Instance, error) {
		return lifecycleInstance{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })}, nil
	})
	for _, tc := range []struct {
		name, value string
		inject      *SecretInject
	}{
		{"PLAIN", coreInjected, apiBinding},
		{"TOKEN", coreInjected, &SecretInject{Origin: "https://other.example.com", Header: "X-Key"}},
		{"NEW", coreInjected, apiBinding},
		{"TOKEN", coreInjected + " ", nil},
		{"NEW", coreInjected + "\n", &SecretInject{Origin: "https://other.example.com", Header: "X-Key"}},
	} {
		if _, err := s.SetSecretBinding(ctx, "app", tc.name, tc.value, tc.inject, ViaCLI); err == nil {
			t.Fatalf("%s: accepted", tc.name)
		} else {
			noValue(t, "a refusal", err.Error())
		}
	}
	secs, err := s.SecretNames(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(secs)
	noValue(t, "the listing", string(b))
	if !strings.Contains(string(b), `"inject":{"origin":"https://api.example.com","header":"Authorization"}`) {
		t.Errorf("the listing lacks the binding: %s", b)
	}
	// A deploy whose capture refuses a stored binding.
	if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "flats.json", Data: []byte(`{"kind":"server","entry":"server.js"}`)}, {Path: "server.js", Data: []byte(`export default {}`)}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	r, err := s.Publish(ctx, "app", 0, "", ViaConsole)
	if err != nil || r.Approval == nil {
		t.Fatalf("publish: %+v %v", r, err)
	}
	if _, err := s.Decide(ctx, r.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB(t, dir).Exec(`UPDATE secrets SET fetch_header='' WHERE name='TOKEN'`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Deploy(ctx, "app", 1, ViaConsole)
	var pending *PendingApproval
	if !errors.As(err, &pending) {
		t.Fatalf("deploy: %v", err)
	}
	receipt, err := s.Decide(ctx, pending.Approval.ID, true)
	var de *DeployError
	if !errors.As(err, &de) || receipt.Status != "failed" || !strings.Contains(err.Error(), "flats secret rm app TOKEN") {
		t.Fatalf("the deploy did not fail at the capture: %+v %v", receipt, err)
	}
	noValue(t, "the decide error", err.Error())
	noValue(t, "the approval result", receipt.Result)
	noValue(t, "the approval data", string(receipt.ResultData))
	events, err := s.st.ListEvents(ctx, "app", "", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		noValue(t, "an event", ev.Message)
	}
	rd, err := store.OpenAuditReader(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	audit, err := rd.List(ctx, store.AuditQuery{}, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range audit {
		b, _ := json.Marshal(e)
		noValue(t, "an audit event", string(b))
	}
}
