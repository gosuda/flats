package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/clientstate"
	"github.com/gosuda/flats/internal/selfupdate"
)

func clientFixture(t *testing.T) (*fakeAPI, string, clientstate.Manifest) {
	t.Helper()
	api, srv := newFakeAPI(t)
	m := clientstate.NewManifest(buildinfo.Info{Version: "v1.2.3", Release: true}, 1000)
	b, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	api.handle("GET /api/client", 200, string(b))
	return api, srv.URL, m
}
func requireResourceCode(t *testing.T, r result, code string) {
	t.Helper()
	var out struct {
		Error struct {
			Code string `json:"code"`
		}
	}
	if r.code != 1 || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Error.Code != code {
		t.Fatalf("want %s got %+v", code, r)
	}
}
func TestClientCheckResources(t *testing.T) {
	api, url, m := clientFixture(t)
	dir := t.TempDir()
	env := Env{ConfigDir: filepath.Join(dir, "config"), Executable: func() (string, error) { return "/standalone/flats", nil }}
	for _, name := range []string{"missing", "current", "unknown"} {
		t.Run(name, func(t *testing.T) {
			skill := filepath.Join(t.TempDir(), "SKILL.md")
			want := "refresh_required"
			if name == "current" {
				os.WriteFile(skill, []byte(m.Skill), 0644)
				want = "current"
			}
			if name == "unknown" {
				os.WriteFile(skill, []byte("old skill without revision"), 0644)
				want = "unknown"
			}
			r := runEnv(t, env, url, "client", "check", "--skill-file", skill, "--json")
			var out map[string]any
			if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || out["bootstrap_status"] != want || out["server_url"] != url || out["executable"] != "/standalone/flats" || out["cli_status"] != "compatible" {
				t.Fatalf("%+v", r)
			}
		})
	}
	if _, e := os.Stat(env.ConfigDir); !os.IsNotExist(e) {
		t.Fatalf("check wrote local config: %v", e)
	}
	if req := api.last("GET /api/client"); req.method != "GET" {
		t.Fatal(req)
	}
	r := run(t, url, "", "client", "check", "--skill-file", dir, "--json")
	requireResourceCode(t, r, "filesystem")
	api.handle("GET /api/client", 200, `{}`)
	requireResourceCode(t, run(t, url, "", "client", "check", "--json"), "invalid_manifest")
}
func TestClientCheckUsageJSON(t *testing.T) {
	r := run(t, "http://127.0.0.1:1", "", "client", "wat", "--json")
	if r.code != 2 || !strings.Contains(r.stdout, `"code": "invalid_target"`) {
		t.Fatalf("%+v", r)
	}
}
func TestSkillCommand(t *testing.T) {
	api, url, m := clientFixture(t)
	dir := t.TempDir()
	skill := filepath.Join(dir, "standalone/SKILL.md")
	env := Env{ConfigDir: filepath.Join(dir, "config")}
	r := runEnv(t, env, url, "skill", "sync", "--output", skill, "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"status": "updated"`) {
		t.Fatalf("%+v", r)
	}
	r = runEnv(t, env, url, "skill", "sync", "--output", skill, "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"reload_required": false`) {
		t.Fatalf("%+v", r)
	}
	os.WriteFile(skill, []byte("user edit"), 0644)
	requireResourceCode(t, runEnv(t, env, url, "skill", "sync", "--output", skill, "--json"), "file_conflict")
	r = runEnv(t, env, url, "skill", "sync", "--output", skill, "--expected-sha256", selfupdate.Digest([]byte("user edit")), "--json")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	os.WriteFile(skill, []byte("old"), 0644)
	n := 0
	api.routes["GET /api/client"] = func(w http.ResponseWriter, r *http.Request, b []byte) {
		n++
		if n > 1 {
			m.ClientContract++
		}
		json.NewEncoder(w).Encode(m)
	}
	requireResourceCode(t, runEnv(t, env, url, "skill", "sync", "--output", skill, "--expected-sha256", selfupdate.Digest([]byte("old")), "--json"), "server_changed")
	b, _ := os.ReadFile(skill)
	if string(b) != "old" {
		t.Fatalf("changed: %q", b)
	}
}

type updateTransport func(*http.Request) (*http.Response, error)

func (f updateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func cliUpdateEnv(t *testing.T) Env {
	t.Helper()
	// Service discovery must never read the developer's real Linux user unit.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	exe := filepath.Join(dir, "flats")
	os.WriteFile(exe, []byte("old"), 0755)
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "flats", Mode: 0755, Size: 3, Typeflag: tar.TypeReg})
	tw.Write([]byte("new"))
	tw.Close()
	gz.Close()
	raw := b.Bytes()
	return Env{ConfigDir: filepath.Join(dir, "config"), Home: filepath.Join(dir, "home"), Executable: func() (string, error) { return exe, nil }, ReleaseTransport: updateTransport(func(r *http.Request) (*http.Response, error) {
		body := raw
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			body = []byte(fmt.Sprintf("%s  flats_%s_%s.tar.gz\n", selfupdate.Digest(raw), runtime.GOOS, runtime.GOARCH))
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	}), CandidateProbe: func(context.Context, string) (selfupdate.Identity, error) {
		return selfupdate.Identity{Info: buildinfo.Info{Version: "v1.2.3", Release: true, OS: runtime.GOOS, Arch: runtime.GOARCH}, ClientContract: clientstate.ContractRevision + 1}, nil
	}}
}
func TestClientUpdateExplicitIndependent(t *testing.T) {
	env := cliUpdateEnv(t)
	r := runEnv(t, env, "http://127.0.0.1:1", "update", "--version", "v1.2.3", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"compatibility_assessed": false`) || !strings.Contains(r.stdout, `"reconnect_required": true`) || strings.Contains(r.stdout, `"server_url"`) {
		t.Fatalf("%+v", r)
	}
	p, _ := env.Executable()
	b, _ := os.ReadFile(p)
	if string(b) != "new" {
		t.Fatalf("%q", b)
	}
}
func TestClientUpdateDefaultAndRace(t *testing.T) {
	for _, name := range []string{"current", "updated", "race", "dev"} {
		t.Run(name, func(t *testing.T) {
			api, url, m := clientFixture(t)
			env := cliUpdateEnv(t)
			n := 0
			api.routes["GET /api/client"] = func(w http.ResponseWriter, r *http.Request, b []byte) {
				n++
				if name != "current" {
					m.ClientContract = clientstate.ContractRevision + 1
				}
				if name == "race" && n > 1 {
					m.ClientContract = clientstate.ContractRevision + 2
				}
				if name == "dev" {
					m.Server.Release = false
					m.Server.Version = "dev"
					m.CLITarget = ""
				}
				json.NewEncoder(w).Encode(m)
			}
			r := runEnv(t, env, url, "update", "--json")
			switch name {
			case "current", "updated":
				if r.code != 0 || !strings.Contains(r.stdout, `"status": "`+name+`"`) {
					t.Fatalf("%+v", r)
				}
			case "race":
				requireResourceCode(t, r, "server_changed")
			case "dev":
				requireResourceCode(t, r, "dev_target")
			}
			p, _ := env.Executable()
			b, _ := os.ReadFile(p)
			want := "old"
			if name == "updated" {
				want = "new"
			}
			if string(b) != want {
				t.Fatalf("want %s got %s", want, b)
			}
		})
	}
}
func TestClientVersionContract(t *testing.T) {
	r := run(t, "http://127.0.0.1:1", "", "--json", "version")
	if r.code != 0 || !strings.Contains(r.stdout, `"client_contract": 2`) {
		t.Fatalf("%+v", r)
	}
}
