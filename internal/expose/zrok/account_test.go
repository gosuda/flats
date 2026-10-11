package zrok

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// accountController emulates the zrok v2 enable and disable endpoints.
type accountController struct {
	mu       sync.Mutex
	token    string          // the only valid account token
	enabled  map[string]bool // environment identity -> registered
	next     int
	disabled []string
}

func (c *accountController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/zrok.v1+json")
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v2/clientVersionCheck":
		w.WriteHeader(http.StatusOK)
	case r.Method == "POST" && r.URL.Path == "/api/v2/enable":
		if r.Header.Get("X-TOKEN") != c.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c.next++
		id := "env-" + string(rune('a'+c.next))
		c.enabled[id] = true
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"identity": id, "cfg": `{"ztAPI":"https://ctrl.test"}`})
	case r.Method == "POST" && r.URL.Path == "/api/v2/disable":
		var body struct{ Identity string }
		json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("X-TOKEN") != c.token || !c.enabled[body.Identity] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		delete(c.enabled, body.Identity)
		c.disabled = append(c.disabled, body.Identity)
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func newAccountController(t *testing.T) *accountController {
	t.Helper()
	c := &accountController{token: "good-token", enabled: map[string]bool{}}
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	t.Setenv("ZROK2_API_ENDPOINT", srv.URL)
	return c
}

func TestConnectAndDisconnect(t *testing.T) {
	c := newAccountController(t)
	dir := filepath.Join(t.TempDir(), "zrok-environment")
	ctx := context.Background()
	if ok, err := Connected(dir); ok || err != nil {
		t.Fatalf("before connect: %v %v", ok, err)
	}
	if err := Connect(ctx, dir, "bad-token", "flats@test"); !errors.Is(err, ErrRejected) {
		t.Fatalf("rejected token: %v", err)
	}
	if ok, _ := Connected(dir); ok {
		t.Fatal("a rejected token left a connected environment")
	}
	if err := Connect(ctx, dir, "good-token", "flats@test"); err != nil {
		t.Fatal(err)
	}
	if ok, err := Connected(dir); !ok || err != nil {
		t.Fatalf("after connect: %v %v", ok, err)
	}
	info, err := os.Stat(filepath.Join(dir, "environment.json"))
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("environment file must be private: %v %v", info.Mode(), err)
	}
	if err := Connect(ctx, dir, "good-token", "flats@test"); !errors.Is(err, ErrConnected) {
		t.Fatalf("second connect: %v", err)
	}
	// A backend built on the managed environment loads it.
	if _, _, err := loadRoot(dir); err != nil {
		t.Fatalf("load connected environment: %v", err)
	}
	if err := Disconnect(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("environment directory kept: %v", err)
	}
	if len(c.disabled) != 1 || len(c.enabled) != 0 {
		t.Fatalf("controller: disabled=%v enabled=%v", c.disabled, c.enabled)
	}
	if err := Disconnect(ctx, dir); err != nil {
		t.Fatalf("disconnect without an environment: %v", err)
	}
}

func TestDisconnectToleratesRevokedEnvironment(t *testing.T) {
	c := newAccountController(t)
	dir := filepath.Join(t.TempDir(), "zrok-environment")
	ctx := context.Background()
	if err := Connect(ctx, dir, "good-token", "flats@test"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.token = "regenerated"
	c.mu.Unlock()
	if err := Disconnect(ctx, dir); err != nil {
		t.Fatalf("an environment zrok no longer accepts: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("environment directory kept")
	}
}

func TestDescribeAndReplace(t *testing.T) {
	c := newAccountController(t)
	dir := filepath.Join(t.TempDir(), "zrok-environment")
	ctx := context.Background()
	if a, err := Describe(dir); a.Connected || err != nil {
		t.Fatalf("before connect: %+v %v", a, err)
	}
	if err := Connect(ctx, dir, "good-token", "flats@test"); err != nil {
		t.Fatal(err)
	}
	first, err := Describe(dir)
	if err != nil || !first.Connected || first.EnvironmentID == "" || first.APIEndpoint == "" {
		t.Fatalf("after connect: %+v %v", first, err)
	}
	// A rejected token leaves the current account connected.
	if err := Replace(ctx, dir, "bad-token", "flats@test"); !errors.Is(err, ErrRejected) {
		t.Fatalf("rejected replacement: %v", err)
	}
	if a, _ := Describe(dir); a.EnvironmentID != first.EnvironmentID {
		t.Fatalf("a rejected replacement changed the environment: %+v", a)
	}
	// The same account under a new token.
	c.mu.Lock()

	c.token = "new-token"
	c.mu.Unlock()
	if err := Replace(ctx, dir, "new-token", "flats@test"); err != nil {
		t.Fatal(err)
	}
	second, err := Describe(dir)
	if err != nil || !second.Connected || second.EnvironmentID == first.EnvironmentID {
		t.Fatalf("after replace: %+v %v", second, err)
	}
	if _, err := os.Stat(dir + ".next"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the staging directory was kept")
	}
	if env, _ := os.ReadFile(filepath.Join(dir, "environment.json")); !strings.Contains(string(env), "new-token") {
		t.Fatal("the new token was not installed")
	}
}
