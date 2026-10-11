package zrok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// accountController emulates the zrok v2 enable and disable endpoints with
// accounts whose tokens can be regenerated.
type accountController struct {
	mu          sync.Mutex
	accounts    map[string]string // token -> account
	envs        map[string]string // environment identity -> account
	next        int
	disabled    []string
	failDisable bool
}

func (c *accountController) rotate(from, to string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accounts[to] = c.accounts[from]
	delete(c.accounts, from)
}

func (c *accountController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/zrok.v1+json")
	c.mu.Lock()
	defer c.mu.Unlock()
	account, known := c.accounts[r.Header.Get("X-TOKEN")]
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v2/clientVersionCheck":
		w.WriteHeader(http.StatusOK)
	case r.Method == "POST" && r.URL.Path == "/api/v2/enable":
		if !known {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c.next++
		id := fmt.Sprintf("env-%d", c.next)
		c.envs[id] = account
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"identity": id, "cfg": `{"ztAPI":"https://ctrl.test"}`})
	case r.Method == "POST" && r.URL.Path == "/api/v2/disable":
		var body struct{ Identity string }
		json.NewDecoder(r.Body).Decode(&body)
		if c.failDisable {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !known || c.envs[body.Identity] != account {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		delete(c.envs, body.Identity)
		c.disabled = append(c.disabled, body.Identity)
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func newAccountController(t *testing.T) *accountController {
	t.Helper()
	c := &accountController{accounts: map[string]string{"good-token": "a", "other-account": "b"}, envs: map[string]string{}}
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
	if err := Disconnect(ctx, dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("environment directory kept: %v", err)
	}
	if len(c.disabled) != 1 || len(c.envs) != 0 {
		t.Fatalf("controller: disabled=%v envs=%v", c.disabled, c.envs)
	}
	if err := Disconnect(ctx, dir, false); err != nil {
		t.Fatalf("disconnect without an environment: %v", err)
	}
}

func TestDisconnectKeepsStaleEnvironmentUnlessForced(t *testing.T) {
	c := newAccountController(t)
	dir := filepath.Join(t.TempDir(), "zrok-environment")
	ctx := context.Background()
	if err := Connect(ctx, dir, "good-token", "flats@test"); err != nil {
		t.Fatal(err)
	}
	c.rotate("good-token", "regenerated") // the stored token no longer works
	if err := Disconnect(ctx, dir, false); !errors.Is(err, ErrStale) {
		t.Fatalf("disconnect with a refused token: %v", err)
	}
	if ok, _ := Connected(dir); !ok {
		t.Fatal("a possibly registered environment was forgotten")
	}
	if err := Disconnect(ctx, dir, true); err != nil {
		t.Fatalf("forced disconnect: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("environment directory kept after a forced disconnect")
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
	// The same account under a regenerated token: the old token is refused,
	// and the new one unregisters the old environment.
	c.rotate("good-token", "new-token")
	if err := Replace(ctx, dir, "new-token", "flats@test"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.disabled, first.EnvironmentID) {
		t.Fatalf("the old environment was not unregistered: %v", c.disabled)
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

func TestReplaceRefusesWhenTheOldEnvironmentCannotBeUnregistered(t *testing.T) {
	c := newAccountController(t)
	dir := filepath.Join(t.TempDir(), "zrok-environment")
	ctx := context.Background()
	if err := Connect(ctx, dir, "good-token", "flats@test"); err != nil {
		t.Fatal(err)
	}
	before, _ := Describe(dir)
	// The old account's token changed elsewhere, and the new token belongs to
	// another account: nothing can unregister the old environment.
	c.rotate("good-token", "lost")
	if err := Replace(ctx, dir, "other-account", "flats@test"); !errors.Is(err, ErrStale) {
		t.Fatalf("replace: %v", err)
	}
	if after, _ := Describe(dir); after.EnvironmentID != before.EnvironmentID {
		t.Fatalf("the current environment changed: %+v", after)
	}
	if _, err := os.Stat(dir + ".next"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the staged environment was kept")
	}
	if len(c.envs) != 1 {
		t.Fatalf("the staged environment stayed registered: %v", c.envs)
	}
}

func TestReplaceUnregistersALeftoverStagedEnvironment(t *testing.T) {
	c := newAccountController(t)
	dir := filepath.Join(t.TempDir(), "zrok-environment")
	ctx := context.Background()
	if err := Connect(ctx, dir, "good-token", "flats@test"); err != nil {
		t.Fatal(err)
	}
	// An earlier replacement registered a staged environment and could not
	// roll it back.
	if err := Connect(ctx, dir+".next", "good-token", "flats@test"); err != nil {
		t.Fatal(err)
	}
	staged, _ := Describe(dir + ".next")
	if err := Replace(ctx, dir, "good-token", "flats@test"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.disabled, staged.EnvironmentID) {
		t.Fatalf("the leftover staged environment was not unregistered: %v", c.disabled)
	}
	if len(c.envs) != 1 {
		t.Fatalf("registered environments = %v", c.envs)
	}
}

func TestConnectKeepsCredentialsWhenRollbackFails(t *testing.T) {
	c := newAccountController(t)
	dir := filepath.Join(t.TempDir(), "zrok-environment")
	ctx := context.Background()
	// Saving the identity fails: its directory name is taken by a file.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identities"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.failDisable = true
	c.mu.Unlock()
	if err := Connect(ctx, dir, "good-token", "flats@test"); err == nil {
		t.Fatal("Connect succeeded without saving the identity")
	}
	if len(c.envs) != 1 {
		t.Fatalf("registered environments = %v", c.envs)
	}
	if _, err := os.Stat(filepath.Join(dir, "environment.json")); err != nil {
		t.Fatalf("the credentials of a still-registered environment were deleted: %v", err)
	}
	c.mu.Lock()
	c.failDisable = false
	c.mu.Unlock()
	if err := Disconnect(ctx, dir, false); err != nil {
		t.Fatalf("Disconnect could not clean up: %v", err)
	}
	if len(c.envs) != 0 {
		t.Fatalf("environment left registered: %v", c.envs)
	}
}
