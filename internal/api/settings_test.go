package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/core"
)

// configHost serves a Service whose settings are saved to a real
// config.json, with a clock the test moves.
type configHost struct {
	srv  *httptest.Server
	svc  *core.Service
	src  *core.SettingsSource
	path string
	dir  string
	mu   sync.Mutex
	now  time.Time
}

func (h *configHost) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

const (
	operatorFile = "/private/operator-credential-path"
	authKeyFile  = "/private/tailscale-authkey-path"
)

func setupConfig(t *testing.T, pin bool) *configHost {
	t.Helper()
	h := &configHost{dir: t.TempDir(), now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	h.path = filepath.Join(h.dir, "config.json")
	doc, err := config.New(config.NewInstanceID(), h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"credentials.operator_file": operatorFile, "network.permitted": "portal", "system.keep_versions": "10"} {
		if err := doc.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := config.Create(h.path, doc)
	if err != nil {
		t.Fatal(err)
	}
	h.src, err = core.NewSettingsSource(doc, &core.SettingsFile{Mode: "config", Hash: hash,
		Overrides: map[string]string{"host.management_addr": "127.0.0.1:9999"},
		Save:      config.NewWriter(h.path).Save,
		DiskHash: func() (string, error) {
			b, err := os.ReadFile(h.path)
			return config.Hash(b), err
		}})
	if err != nil {
		t.Fatal(err)
	}
	if pin {
		h.src.Pin(core.SetPortalRelays, "the service's --relays flag")
	}
	h.srv, h.svc, _ = setupWith(t, h.dir, func(c *core.Config) {
		c.Settings = h.src
		c.Now = func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.now
		}
	})
	return h
}

// settingsCall sends a console request and returns the status, the ETag
// header and the raw body.
func settingsCall(t *testing.T, h *configHost, method, path, body string, hdr map[string]string) (int, string, []byte) {
	t.Helper()
	r, _ := http.NewRequest(method, h.srv.URL+"/console/api"+path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("ETag"), raw
}

type settingsBody struct {
	Settings        map[string]string `json:"settings"`
	Applied         []string          `json:"applied"`
	RestartRequired []string          `json:"restart_required"`
	ETag            string            `json:"etag"`
	Error           string            `json:"error"`
	Category        string            `json:"category"`
	Config          *core.ConfigView  `json:"config"`
}

func decodeSettings(t *testing.T, raw []byte) settingsBody {
	t.Helper()
	var b settingsBody
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return b
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return config.Hash(b)
}

// GET settings describes config.json: mode, ETag, host, network, whether
// credentials are configured, sources and pins. It never returns a path.
func TestSettingsDescribeConfig(t *testing.T) {
	h := setupConfig(t, true)
	read := map[string]string{"X-Flats-Console": "1"}
	code, etag, raw := settingsCall(t, h, "GET", "/settings", "", read)
	if code != 200 {
		t.Fatalf("get settings: %d %s", code, raw)
	}
	b := decodeSettings(t, raw)
	c := b.Config
	hash := fileHash(t, h.path)
	if c == nil || c.Mode != "config" || c.SchemaVersion != config.CurrentSchemaVersion || c.ETag != hash || etag != strconv.Quote(hash) || c.ChangedOnDisk {
		t.Fatalf("config = %+v, ETag header %q, file hash %s", c, etag, hash)
	}
	if c.Host.ManagementAddr != "127.0.0.1:9999" || c.Host.LocalAddr != "127.0.0.1:7879" || c.Host.ConsoleHost != "flats" || !c.Host.ServerRuntime {
		t.Fatalf("host = %+v", c.Host)
	}
	if !slices.Equal(c.Network.Permitted, []string{"portal"}) || c.Network.PrivateBackend != "local" {
		t.Fatalf("network = %+v", c.Network)
	}
	if !c.Credentials.OperatorFile || c.Credentials.TailscaleAuthKeyFile {
		t.Fatalf("credentials = %+v", c.Credentials)
	}
	for k, want := range map[string]config.Source{"host.management_addr": config.SourceFlag, "host.local_addr": config.SourceDefault,
		"network.permitted": config.SourceFile, "system.keep_versions": config.SourceFile, "system.events_keep": config.SourceDefault,
		"credentials.operator_file": config.SourceFile, "credentials.tailscale_authkey_file": config.SourceDefault} {
		if c.Sources[k] != want {
			t.Errorf("source of %s = %q, want %q", k, c.Sources[k], want)
		}
	}
	if len(c.Sources) != len(config.Keys()) || c.Keys[core.SetPortalMaxRelay] != "portal.max_active_relays" || len(c.Keys) != len(core.Defaults) {
		t.Fatalf("sources %v, keys %v", c.Sources, c.Keys)
	}
	if c.Pinned[core.SetPortalRelays] != "the service's --relays flag" || len(c.Pinned) != 1 {
		t.Fatalf("pinned = %v", c.Pinned)
	}
	for _, secret := range []string{h.dir, operatorFile, `"path"`} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("settings response contains %q: %s", secret, raw)
		}
	}

	// An edit on disk is reported, and the ETag stays the loaded file's.
	edit(t, h.path, `"keep_versions": 10`, `"keep_versions": 11`)
	_, etag, raw = settingsCall(t, h, "GET", "/settings", "", read)
	if c := decodeSettings(t, raw).Config; !c.ChangedOnDisk || c.ETag != hash || etag != strconv.Quote(hash) {
		t.Fatalf("after edit: %+v %q", c, etag)
	}
	os.Remove(h.path)
	if _, _, raw = settingsCall(t, h, "GET", "/settings", "", read); !decodeSettings(t, raw).Config.ChangedOnDisk {
		t.Fatal("a removed file is not reported as changed")
	}

	// Settings kept in memory have no config.
	srv, _ := setup(t)
	_, out := req(t, "GET", srv.URL+"/console/api/settings", nil, read)
	if v, ok := out["config"]; !ok || v != nil {
		t.Fatalf("memory settings config = %v", out["config"])
	}
}

func edit(t *testing.T, path, from, to string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(from)) {
		t.Fatalf("%s has no %s:\n%s", path, from, b)
	}
	if err := os.WriteFile(path, bytes.Replace(b, []byte(from), []byte(to), 1), 0o600); err != nil {
		t.Fatal(err)
	}
}

// PUT settings with If-Match saves only against the ETag in effect, and
// reports which changes apply now and which after a restart.
func TestSettingsIfMatch(t *testing.T) {
	h := setupConfig(t, true)
	hdr := consoleHdr(t, h.srv)
	with := func(etag string) map[string]string {
		m := map[string]string{}
		for k, v := range hdr {
			m[k] = v
		}
		m["If-Match"] = etag
		return m
	}
	first := fileHash(t, h.path)

	code, etag, raw := settingsCall(t, h, "PUT", "/settings", `{"keep_versions":"4","portal_max_relays":"5","rate_limit_rps":"50"}`, with(strconv.Quote(first)))
	b := decodeSettings(t, raw)
	second := fileHash(t, h.path)
	if code != 200 || b.ETag != second || second == first || etag != strconv.Quote(second) {
		t.Fatalf("save: %d %s (file %s)", code, raw, second)
	}
	// rate_limit_rps was sent unchanged, so it is in neither list.
	if !slices.Equal(b.Applied, []string{"keep_versions"}) || !slices.Equal(b.RestartRequired, []string{"portal_max_relays"}) || b.Settings["keep_versions"] != "4" {
		t.Fatalf("applied %v, restart %v", b.Applied, b.RestartRequired)
	}
	if h.svc.UploadLimit() != 20<<20 {
		t.Fatal("unrelated setting changed")
	}

	// A second request with the first ETag cannot overwrite the first save.
	code, _, raw = settingsCall(t, h, "PUT", "/settings", `{"keep_versions":"7"}`, with(strconv.Quote(first)))
	if b := decodeSettings(t, raw); code != 412 || b.Category != "config_changed" || !strings.Contains(b.Error, "reload the page") {
		t.Fatalf("stale ETag: %d %s", code, raw)
	}
	// The unquoted form is accepted too.
	if code, _, raw := settingsCall(t, h, "PUT", "/settings", `{"keep_versions":"6"}`, with(second)); code != 200 {
		t.Fatalf("unquoted ETag: %d %s", code, raw)
	}
	third := fileHash(t, h.path)

	// A pinned setting is refused as before.
	code, _, raw = settingsCall(t, h, "PUT", "/settings", `{"portal_relays":"https://other.example"}`, with(strconv.Quote(third)))
	if b := decodeSettings(t, raw); code != 409 || b.Category != "config_overridden" {
		t.Fatalf("pinned: %d %s", code, raw)
	}

	// After an edit on disk, a save with the ETag in effect is refused with
	// a restart instruction, and the edit is kept.
	edit(t, h.path, `"keep_versions": 6`, `"keep_versions": 8`)
	edited := fileHash(t, h.path)
	code, _, raw = settingsCall(t, h, "PUT", "/settings", `{"events_keep":"100"}`, with(strconv.Quote(third)))
	if b := decodeSettings(t, raw); code != 412 || b.Category != "config_changed" || !strings.Contains(b.Error, "restart Flats") {
		t.Fatalf("edited on disk: %d %s", code, raw)
	}
	// An ETag that matches neither still names the edit on disk.
	code, _, raw = settingsCall(t, h, "PUT", "/settings", `{"events_keep":"100"}`, with(strconv.Quote(first)))
	if b := decodeSettings(t, raw); code != 412 || !strings.Contains(b.Error, "changed on disk") {
		t.Fatalf("stale ETag after edit: %d %s", code, raw)
	}
	// Without If-Match the conflict keeps its 409.
	code, _, raw = settingsCall(t, h, "PUT", "/settings", `{"events_keep":"100"}`, hdr)
	if b := decodeSettings(t, raw); code != 409 || b.Category != "conflict" {
		t.Fatalf("unconditional save after edit: %d %s", code, raw)
	}
	if fileHash(t, h.path) != edited {
		t.Fatal("a refused save changed the file")
	}
	if all, _ := h.svc.Settings(context.Background()); all["events_keep"] != "5000" || all["keep_versions"] != "6" {
		t.Fatalf("refused saves applied: %v", all)
	}
}

type impactBody struct {
	Impact   map[string]core.RetentionImpact `json:"impact"`
	Error    string                          `json:"error"`
	Category string                          `json:"category"`
}

func impactOf(t *testing.T, h *configHost, body string) (int, impactBody) {
	t.Helper()
	code, _, raw := settingsCall(t, h, "POST", "/settings/impact", body, consoleHdr(t, h.srv))
	var b impactBody
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return code, b
}

func put(t *testing.T, h *configHost, body string) {
	t.Helper()
	if code, _, raw := settingsCall(t, h, "PUT", "/settings", body, consoleHdr(t, h.srv)); code != 200 {
		t.Fatalf("PUT %s: %d %s", body, code, raw)
	}
}

func prunedVersions(t *testing.T, h *configHost, slug string) []int {
	t.Helper()
	vs, err := h.svc.ListVersions(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, v := range vs {
		if v.Pruned {
			out = append(out, v.Number)
		}
	}
	return out
}

func eventCount(t *testing.T, h *configHost, slug string) int64 {
	t.Helper()
	evs, err := h.svc.Events(context.Background(), slug, "", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(evs))
}

// The impact preview reports what the next pruning removes under the
// candidate value, and the pruning then removes exactly that.
func TestSettingsImpactMatchesPruning(t *testing.T) {
	h := setupConfig(t, false)
	ctx := context.Background()
	hash := fileHash(t, h.path)

	// Versions: v1..v5 published, v5 live.
	for i := 1; i <= 5; i++ {
		saveAndPublish(t, h.srv, "blog", fmt.Sprint("v", i))
	}
	saveAndPublish(t, h.srv, "notes", "n1")

	// Increases, equal values and other settings have no impact.
	if code, b := impactOf(t, h, `{"keep_versions":"20","events_keep":"5000","rate_limit_rps":"1","preview_ttl_seconds":"90000"}`); code != 200 || len(b.Impact) != 0 {
		t.Fatalf("no decrease: %d %+v", code, b)
	}
	if code, b := impactOf(t, h, `{"keep_versions":"0"}`); code != 200 || len(b.Impact) != 0 {
		t.Fatalf("keep_versions 0 keeps everything: %d %+v", code, b)
	}
	if code, b := impactOf(t, h, `{"events_keep":"0"}`); code != 400 || b.Category != "invalid" {
		t.Fatalf("invalid candidate: %d %+v", code, b)
	}
	if code, b := impactOf(t, h, `{"bogus":"1"}`); code != 400 {
		t.Fatalf("unknown setting: %d %+v", code, b)
	}

	_, b := impactOf(t, h, `{"keep_versions":"2"}`)
	vi := b.Impact["keep_versions"]
	if vi.Current != "10" || vi.Candidate != "2" || vi.Total != 2 || len(vi.Flats) != 1 || vi.Flats[0].Slug != "blog" ||
		!slices.Equal(vi.Flats[0].Versions, []int{2, 1}) || vi.Flats[0].Count != 2 {
		t.Fatalf("version impact = %+v", vi)
	}
	put(t, h, `{"keep_versions":"2"}`)
	if fileHash(t, h.path) == hash {
		t.Fatal("setting not saved")
	}
	// The next pruning runs after blog's next deploy.
	code, out := req(t, "POST", h.srv.URL+"/api/flats/blog/deploy", strings.NewReader(`{"version":5}`), nil)
	if code != 202 {
		t.Fatalf("redeploy: %d %v", code, out)
	}
	approveRequest(t, h.srv, out)
	if got := prunedVersions(t, h, "blog"); !slices.Equal(got, vi.Flats[0].Versions) {
		t.Fatalf("pruned %v, preview said %v", got, vi.Flats[0].Versions)
	}

	// Events: the next sweep trims each flat's log to the candidate.
	_, b = impactOf(t, h, `{"events_keep":"3"}`)
	ei := b.Impact["events_keep"]
	before := map[string]int64{"blog": eventCount(t, h, "blog"), "notes": eventCount(t, h, "notes")}
	if ei.Total != before["blog"]-3+before["notes"]-3 || len(ei.Flats) != 2 || ei.Flats[0].Slug != "blog" {
		t.Fatalf("event impact = %+v, events %v", ei, before)
	}
	put(t, h, `{"events_keep":"3"}`)
	h.svc.Sweep(ctx)
	for _, f := range ei.Flats {
		if removed := before[f.Slug] - eventCount(t, h, f.Slug); removed != f.Count {
			t.Fatalf("sweep removed %d events of %s, preview said %d", removed, f.Slug, f.Count)
		}
	}

	// Previews: a previewed version is kept on top of keep_versions.
	_, b = impactOf(t, h, `{"keep_versions":"1"}`)
	if vi := b.Impact["keep_versions"]; vi.Total != 1 || !slices.Equal(vi.Flats[0].Versions, []int{3}) {
		t.Fatalf("keep 1 = %+v", vi)
	}
	old := openPreview(t, h, "blog", 4)
	if _, b = impactOf(t, h, `{"keep_versions":"1"}`); b.Impact["keep_versions"].Total != 0 {
		t.Fatalf("previewed version pruned: %+v", b.Impact)
	}
	h.advance(2 * time.Hour)
	fresh := openPreview(t, h, "notes", 1)
	_, b = impactOf(t, h, `{"preview_ttl_seconds":"3600"}`)
	pi := b.Impact["preview_ttl_seconds"]
	if pi.Total != 1 || len(pi.Flats) != 1 || pi.Flats[0].Slug != "blog" || !slices.Equal(pi.Flats[0].Previews, []string{old}) {
		t.Fatalf("preview impact = %+v", pi)
	}
	put(t, h, `{"preview_ttl_seconds":"3600"}`)
	h.svc.Sweep(ctx)
	open := map[string]bool{}
	for _, slug := range []string{"blog", "notes"} {
		ps, err := h.svc.ListPreviews(ctx, slug)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			open[p.Host] = true
		}
	}
	if open[old] || !open[fresh] {
		t.Fatalf("open previews after the sweep: %v (closed %s, kept %s)", open, old, fresh)
	}
}

func openPreview(t *testing.T, h *configHost, slug string, version int) string {
	t.Helper()
	code, out := req(t, "POST", h.srv.URL+"/api/flats/"+slug+"/previews", strings.NewReader(fmt.Sprintf(`{"version":%d}`, version)), nil)
	if code != 201 {
		t.Fatalf("preview %s v%d: %d %v", slug, version, code, out)
	}
	return out["host"].(string)
}
