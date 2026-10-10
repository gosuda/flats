package zrok

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openziti/zrok/v2/environment/env_core"
)

// fakeRoot is the part of an enabled zrok environment sdkBackend reads. The
// embedded interface is nil, so any other method would panic.
type fakeRoot struct {
	env_core.Root
	endpoint string
}

func (r fakeRoot) ApiEndpoint() (string, string) { return r.endpoint, "test" }

func (r fakeRoot) Environment() *env_core.Environment {
	return &env_core.Environment{AccountToken: "account-token", ZitiIdentity: "env-new", ApiEndpoint: r.endpoint}
}

// controller emulates the zrok v2 REST paths sdkBackend calls.
type controller struct {
	versionChecks atomic.Int32
	shares        map[string]string // token -> environment that owns it
	nameConflict  string            // reason for a CreateShareName conflict; "-" = no reason
}

func (c *controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/zrok.v1+json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v2/clientVersionCheck":
		c.versionChecks.Add(1)
		w.WriteHeader(http.StatusOK)
	case r.Method == "DELETE" && r.URL.Path == "/api/v2/unshare":
		var body struct{ EnvZID, ShareToken string }
		json.NewDecoder(r.Body).Decode(&body)
		if env, ok := c.shares[body.ShareToken]; ok && env == body.EnvZID {
			delete(c.shares, body.ShareToken)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v2/detail/share/"):
		token := strings.TrimPrefix(r.URL.Path, "/api/v2/detail/share/")
		env, ok := c.shares[token]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"shareToken": token, "envZId": env, "target": "flats:blog", "frontendEndpoints": []string{}})
	case r.Method == "POST" && r.URL.Path == "/api/v2/share/name":
		if c.nameConflict == "" {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusConflict)
		if c.nameConflict != "-" {
			json.NewEncoder(w).Encode(c.nameConflict)
		}
	default:
		http.NotFound(w, r)
	}
}

func newTestBackend(t *testing.T, c *controller) *sdkBackend {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return &sdkBackend{root: fakeRoot{endpoint: srv.URL}}
}

func TestUnshareNotFoundChecksOtherEnvironments(t *testing.T) {
	c := &controller{shares: map[string]string{"mine": "env-new", "old": "env-old"}}
	b := newTestBackend(t, c)
	ctx := context.Background()
	if err := b.Unshare(ctx, "mine"); err != nil {
		t.Fatalf("own share: %v", err)
	}
	if err := b.Unshare(ctx, "gone"); err != nil {
		t.Fatalf("deleted share: %v", err)
	}
	err := b.Unshare(ctx, "old")
	if !errors.Is(err, errOtherEnvironment) || !strings.Contains(err.Error(), "env-old") {
		t.Fatalf("share of another environment: %v", err)
	}
	if _, ok := c.shares["old"]; !ok {
		t.Fatal("the other environment's share was reported gone")
	}
	if n := c.versionChecks.Load(); n != 1 {
		t.Fatalf("version checks = %d, want 1 (the client is reused)", n)
	}
}

func TestShareOwnedNeedsEnvironmentAndTarget(t *testing.T) {
	c := &controller{shares: map[string]string{"mine": "env-new", "old": "env-old"}}
	b := newTestBackend(t, c)
	ctx := context.Background()
	for token, want := range map[string]bool{"mine": true, "old": false, "gone": false} {
		got, err := b.ShareOwned(ctx, token, "flats:blog")
		if err != nil || got != want {
			t.Errorf("ShareOwned(%s) = %v, %v; want %v", token, got, err, want)
		}
	}
	if got, _ := b.ShareOwned(ctx, "mine", "flats:other"); got {
		t.Error("a share with another target was owned")
	}
}

func TestCreateNameConflicts(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, &controller{nameConflict: "-"})
	if err := b.CreateName(ctx, "public", "blog"); !errors.Is(err, errNameExists) {
		t.Fatalf("existing name: %v", err)
	}
	b = newTestBackend(t, &controller{nameConflict: "names limit reached; cannot reserve additional names"})
	if err := b.CreateName(ctx, "public", "blog"); err == nil || errors.Is(err, errNameExists) || !strings.Contains(err.Error(), "names limit") {
		t.Fatalf("limit: %v", err)
	}
	b = newTestBackend(t, &controller{})
	if err := b.CreateName(ctx, "public", "blog"); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestVersionCheckFollowsContext(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	b := &sdkBackend{root: fakeRoot{endpoint: srv.URL}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Unshare(ctx, "x"); err == nil {
		t.Fatal("a cancelled context did not stop the version check")
	}
}
