package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testID = "63b57046-582f-48d9-a64f-877ae28d12aa"

// fileWith returns a minimal v1 file with extra raw JSON members added to
// the named sections, e.g. fileWith("system", `"keep_versions": 3`).
func fileWith(pairs ...string) string {
	sections := map[string][]string{"host": {`"instance_id": "` + testID + `"`, `"data_dir": "/var/lib/flats"`}}
	order := []string{"host"}
	for i := 0; i+1 < len(pairs); i += 2 {
		if _, ok := sections[pairs[i]]; !ok {
			order = append(order, pairs[i])
		}
		sections[pairs[i]] = append(sections[pairs[i]], pairs[i+1])
	}
	var b strings.Builder
	b.WriteString(`{"schema_version": 1`)
	for _, s := range order {
		fmt.Fprintf(&b, `, %q: {%s}`, s, strings.Join(sections[s], ", "))
	}
	b.WriteString("}")
	return b.String()
}

func errPath(err error) string {
	var fe *FieldError
	if errors.As(err, &fe) {
		return fe.Path
	}
	return ""
}

func TestValidation(t *testing.T) {
	tests := []struct {
		section, member string
		wantPath        string // "" = valid
	}{
		{"host", `"instance_id": "0f1e2d3c-4b5a-4978-8a6b-5c4d3e2f1a0b"`, "host.instance_id"}, // duplicate key
		// Every address `flats serve --listen` accepted stays valid, as written.
		{"host", `"management_addr": "127.0.0.1:7878"`, ""},
		{"host", `"management_addr": "127.1.2.3:80"`, ""},
		{"host", `"management_addr": "[::1]:9000"`, ""},
		{"host", `"management_addr": "localhost:7878"`, ""},
		{"host", `"management_addr": "0.0.0.0:7878"`, ""},
		{"host", `"management_addr": "10.0.0.1:7878"`, ""},
		{"host", `"management_addr": ":7878"`, ""},
		{"host", `"management_addr": "flats.example:7878"`, ""},
		{"host", `"management_addr": "127.0.0.1:0"`, ""},
		{"host", `"management_addr": "127.0.0.1:"`, ""},
		{"host", `"management_addr": "127.0.0.1:07878"`, ""},
		{"host", `"management_addr": "[::ffff:127.0.0.1]:7878"`, ""},
		{"host", `"management_addr": "[fe80::1%lo0]:7878"`, ""},
		{"host", `"management_addr": "127.0.0.1:65535"`, ""},
		{"host", `"management_addr": "127.0.0.1:65536"`, "host.management_addr"},
		{"host", `"management_addr": "127.0.0.1:-1"`, "host.management_addr"},
		{"host", `"management_addr": "127.0.0.1:http"`, "host.management_addr"},
		{"host", `"management_addr": "127.0.0.1"`, "host.management_addr"},
		{"host", `"management_addr": "::1:7878"`, "host.management_addr"},
		{"host", `"management_addr": 7878`, "host.management_addr"},
		{"host", `"local_addr": "127.0.0.2:7879"`, ""},
		{"host", `"local_addr": "192.168.0.1:7879"`, ""},
		{"host", `"local_addr": "127.0.0.1:7878"`, "host.local_addr"}, // equals default management_addr
		{"host", `"console_host": "my-console"`, ""},
		{"host", `"console_host": "Flats"`, ""},
		{"host", `"console_host": "a.flats"`, ""},
		{"host", `"console_host": 1`, "host.console_host"},
		{"host", `"server_runtime": false`, ""},
		{"host", `"server_runtime": "false"`, "host.server_runtime"},
		{"host", `"server_runtime": 0`, "host.server_runtime"},
		{"network", `"permitted": []`, ""},
		{"network", `"permitted": ["portal", "tailscale-funnel", "tailscale"]`, ""},
		{"network", `"permitted": ["local"]`, "network.permitted[0]"},
		{"network", `"permitted": ["funnel"]`, "network.permitted[0]"},
		{"network", `"permitted": ["portal", "portal"]`, "network.permitted[1]"},
		{"network", `"permitted": "portal"`, "network.permitted"},
		{"network", `"permitted": [1]`, "network.permitted[0]"},
		{"network", `"private_backend": "local"`, ""},
		{"network", `"private_backend": "tailscale"`, "network.private_backend"},
		{"network", `"private_backend": "tailscale-funnel"`, "network.private_backend"},
		{"system", `"upload_max_bytes": 1`, ""},
		{"system", `"upload_max_bytes": 9007199254740991`, ""},
		{"system", `"upload_max_bytes": 0`, "system.upload_max_bytes"},
		{"system", `"upload_max_bytes": 9007199254740992`, "system.upload_max_bytes"},
		{"system", `"keep_versions": 0`, ""},
		{"system", `"keep_versions": 9007199254740991`, ""},
		{"system", `"keep_versions": -1`, "system.keep_versions"},
		{"system", `"keep_versions": 9007199254740992`, "system.keep_versions"},
		{"system", `"disk_quota_bytes": 0`, ""},
		{"system", `"disk_quota_bytes": 9007199254740991`, ""},
		{"system", `"disk_quota_bytes": 9007199254740992`, "system.disk_quota_bytes"},
		{"system", `"disk_quota_bytes": 99999999999999999999`, "system.disk_quota_bytes"},
		{"system", `"preview_ttl_seconds": 1`, ""},
		{"system", `"preview_ttl_seconds": 9007199254740991`, ""},
		{"system", `"preview_ttl_seconds": 0`, "system.preview_ttl_seconds"},
		{"system", `"preview_ttl_seconds": 9007199254740992`, "system.preview_ttl_seconds"},
		{"system", `"rate_limit_rps": 1`, ""},
		{"system", `"rate_limit_rps": 9007199254740991`, ""},
		{"system", `"rate_limit_rps": 0`, "system.rate_limit_rps"},
		{"system", `"rate_limit_rps": 9007199254740992`, "system.rate_limit_rps"},
		{"system", `"redirect_days": 0`, ""},
		{"system", `"redirect_days": 9007199254740991`, ""},
		{"system", `"redirect_days": -1`, "system.redirect_days"},
		{"system", `"redirect_days": 9007199254740992`, "system.redirect_days"},
		{"system", `"events_keep": 1`, ""},
		{"system", `"events_keep": 9007199254740991`, ""},
		{"system", `"events_keep": 0`, "system.events_keep"},
		{"system", `"events_keep": 9007199254740992`, "system.events_keep"},
		{"portal", `"relays": []`, ""},
		{"portal", `"relays": ["https://relay.example.com", "relay.example.com:8443"]`, ""},
		{"portal", `"relays": ["http://example.com"]`, "portal.relays[0]"},
		{"portal", `"relays": ["https://user@example.com"]`, "portal.relays[0]"},
		{"portal", `"relays": ["https://relay.example.com", "https://relay.example.com/"]`, "portal.relays[1]"},
		{"portal", `"relays": ["https://relay.example.com", "relay.example.com"]`, "portal.relays[1]"},
		{"portal", `"discovery": true`, ""},
		{"portal", `"discovery": false`, "portal.discovery"},
		{"portal", `"max_active_relays": 1`, ""},
		{"portal", `"max_active_relays": 2147483647`, ""},
		{"portal", `"max_active_relays": 0`, "portal.max_active_relays"},
		{"portal", `"max_active_relays": 2147483648`, "portal.max_active_relays"},
		{"credentials", `"operator_file": "/etc/flats/operator"`, ""},
		{"credentials", `"operator_file": "operator"`, "credentials.operator_file"},
		{"credentials", `"tailscale_authkey_file": "/etc/flats/authkey"`, ""},
		{"credentials", `"tailscale_authkey_file": ""`, "credentials.tailscale_authkey_file"},
	}
	for _, tt := range tests {
		t.Run(tt.member, func(t *testing.T) {
			_, err := Parse([]byte(fileWith(tt.section, tt.member)))
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("Parse = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Parse = nil, want error at %s", tt.wantPath)
			}
			if got := errPath(err); got != tt.wantPath {
				t.Fatalf("error path = %q, want %q (%v)", got, tt.wantPath, err)
			}
			if !strings.Contains(err.Error(), "(") {
				t.Errorf("error %q has no fix hint", err)
			}
		})
	}
}

func TestCrossKeyRules(t *testing.T) {
	valid := []string{
		fileWith("network", `"permitted": ["tailscale"]`, "network", `"private_backend": "tailscale"`),
		fileWith("portal", `"relays": ["https://relay.example.com"]`, "portal", `"discovery": false`),
		fileWith("host", `"management_addr": "127.0.0.1:9000"`, "host", `"local_addr": "127.0.0.1:7878"`),
	}
	for _, f := range valid {
		if _, err := Parse([]byte(f)); err != nil {
			t.Errorf("Parse(%s) = %v", f, err)
		}
	}
	f := fileWith("host", `"management_addr": "127.0.0.1:9000"`, "host", `"local_addr": "127.0.0.1:9000"`)
	if _, err := Parse([]byte(f)); errPath(err) != "host.local_addr" {
		t.Errorf("same addresses: err = %v", err)
	}
}

func TestStrictParse(t *testing.T) {
	big := fileWith("credentials", `"operator_file": "/`+strings.Repeat("a", MaxFileSize)+`"`)
	tests := []struct {
		name, file string
		wantPath   string // "" = any error
	}{
		{"duplicate nested key", fileWith("system", `"keep_versions": 1`, "system", `"keep_versions": 2`), "system.keep_versions"},
		{"duplicate section", `{"schema_version": 1, "host": {}, "host": {}}`, "host"},
		{"duplicate schema_version", `{"schema_version": 1, "schema_version": 1}`, "schema_version"},
		{"unknown nested key", fileWith("system", `"keep_version": 1`), "system.keep_version"},
		{"unknown section", fileWith("hosts", `"x": 1`), "hosts"},
		{"unknown top-level scalar", `{"schema_version": 1, "comment": "x"}`, "comment"},
		{"null value", fileWith("system", `"keep_versions": null`), "system.keep_versions"},
		{"null in array", fileWith("portal", `"relays": [null]`), "portal.relays[0]"},
		{"null section", `{"schema_version": 1, "system": null}`, "system"},
		{"float", fileWith("system", `"keep_versions": 1.0`), "system.keep_versions"},
		{"fraction", fileWith("system", `"keep_versions": 1.5`), "system.keep_versions"},
		{"exponent", fileWith("system", `"keep_versions": 1e3`), "system.keep_versions"},
		{"quoted int", fileWith("system", `"keep_versions": "10"`), "system.keep_versions"},
		{"section not object", `{"schema_version": 1, "system": []}`, "system"},
		{"missing schema_version", `{"host": {}}`, "schema_version"},
		{"schema_version zero", `{"schema_version": 0}`, "schema_version"},
		{"schema_version float", `{"schema_version": 1.0}`, "schema_version"},
		{"schema_version string", `{"schema_version": "1"}`, "schema_version"},
		{"missing instance_id", `{"schema_version": 1, "host": {"data_dir": "/x"}}`, "host.instance_id"},
		{"missing data_dir", `{"schema_version": 1, "host": {"instance_id": "` + testID + `"}}`, "host.data_dir"},
		{"invalid UTF-8", fileWith("host", "\"console_host\": \"fl\xffats\""), ""},
		{"oversize", big, ""},
		{"not an object", `[1]`, ""},
		{"trailing data", fileWith() + ` {}`, ""},
		{"truncated", `{"schema_version": 1`, ""},
		{"empty", ``, ""},
		{"byte order mark", "\ufeff" + fileWith(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.file))
			if err == nil {
				t.Fatal("Parse = nil, want error")
			}
			if tt.wantPath != "" && errPath(err) != tt.wantPath {
				t.Fatalf("error path = %q, want %q (%v)", errPath(err), tt.wantPath, err)
			}
		})
	}
}

func TestEffectiveDefaults(t *testing.T) {
	l, err := Parse([]byte(fileWith("system", `"keep_versions": 10`)))
	if err != nil {
		t.Fatal(err)
	}
	c, err := l.Doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := HostConfig{testID, "/var/lib/flats", "127.0.0.1:7878", "127.0.0.1:7879", "flats", true}
	if c.Host != want {
		t.Errorf("Host = %+v, want %+v", c.Host, want)
	}
	if !slices.Equal(c.Network.Permitted, []string{}) || c.Network.PrivateBackend != "local" {
		t.Errorf("Network = %+v", c.Network)
	}
	if c.System != (SystemConfig{20 << 20, 10, 30 << 30, 86400, 50, 7, 5000}) {
		t.Errorf("System = %+v", c.System)
	}
	if len(c.Portal.Relays) != 0 || !c.Portal.Discovery || c.Portal.MaxActiveRelays != 3 {
		t.Errorf("Portal = %+v", c.Portal)
	}
	if c.Credentials != (CredentialsConfig{}) {
		t.Errorf("Credentials = %+v", c.Credentials)
	}
	for _, key := range Keys() {
		_, src, ok := c.Lookup(key)
		want := SourceDefault
		switch key {
		case "host.instance_id", "host.data_dir", "system.keep_versions":
			want = SourceFile
		}
		if !ok || src != want {
			t.Errorf("Lookup(%s) source = %q, want %q", key, src, want)
		}
	}
	if v, _, _ := c.Lookup("credentials.operator_file"); v != nil {
		t.Errorf("credentials.operator_file = %v, want nil", v)
	}
	if _, _, ok := c.Lookup("system.nope"); ok {
		t.Error("Lookup of an unknown key succeeded")
	}
	// A value equal to its default stays in the file.
	if !bytes.Contains(l.Doc.Encode(), []byte(`"keep_versions": 10`)) {
		t.Errorf("explicit default dropped:\n%s", l.Doc.Encode())
	}
	// Lookup and the typed fields must not share slices with the document.
	if err := l.Doc.Set("portal.relays", "https://relay.example.com"); err != nil {
		t.Fatal(err)
	}
	c, _ = l.Doc.Effective(nil)
	v, _, _ := c.Lookup("portal.relays")
	v.([]string)[0] = "changed"
	c.Portal.Relays[0] = "changed"
	if v, _, _ := c.Lookup("portal.relays"); v.([]string)[0] != "https://relay.example.com" {
		t.Errorf("Lookup returned a shared slice")
	}
	if !bytes.Contains(l.Doc.Encode(), []byte(`"https://relay.example.com"`)) {
		t.Errorf("typed field shares the document slice")
	}
}

func TestOverrides(t *testing.T) {
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	before := doc.Encode()
	c, err := doc.Effective(map[string]string{
		"host.management_addr": "127.0.0.1:18000",
		"host.local_addr":      "[::1]:18001",
		"host.console_host":    "gate-console",
		"host.server_runtime":  "false",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := HostConfig{testID, "/var/lib/flats", "127.0.0.1:18000", "[::1]:18001", "gate-console", false}
	if c.Host != want {
		t.Errorf("Host = %+v, want %+v", c.Host, want)
	}
	for _, key := range []string{"host.management_addr", "host.local_addr", "host.console_host", "host.server_runtime"} {
		if _, src, _ := c.Lookup(key); src != SourceFlag {
			t.Errorf("%s source = %q, want flag", key, src)
		}
	}
	if !bytes.Equal(doc.Encode(), before) {
		t.Error("override changed the document")
	}

	rejected := []struct {
		key, value, path string
	}{
		{"host.data_dir", "/tmp/other", "host.data_dir"},
		{"host.instance_id", testID, "host.instance_id"},
		{"network.permitted", "portal", "network.permitted"},
		{"system.keep_versions", "3", "system.keep_versions"},
		{"portal.relays", "https://relay.example.com", "portal.relays"},
		{"credentials.operator_file", "/x", "credentials.operator_file"},
		{"host.nope", "x", "host.nope"},
		{"host.management_addr", "127.0.0.1", "host.management_addr"},
		{"host.local_addr", "127.0.0.1:7878", "host.local_addr"}, // same as management_addr
		{"host.server_runtime", "yes", "host.server_runtime"},
	}
	for _, r := range rejected {
		_, err := doc.Effective(map[string]string{r.key: r.value})
		if errPath(err) != r.path {
			t.Errorf("override %s=%s: err = %v, want path %s", r.key, r.value, err, r.path)
		}
	}
}

func TestSetUnset(t *testing.T) {
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	sets := []struct{ key, value string }{
		{"network.permitted", "portal, tailscale"},
		{"portal.relays", `["relay-a.example.com", "https://relay.example.com/relay"]`},
		{"system.keep_versions", "10"},
		{"host.server_runtime", "false"},
		{"credentials.operator_file", "/etc/flats/operator"},
	}
	for _, s := range sets {
		if err := doc.Set(s.key, s.value); err != nil {
			t.Fatalf("Set(%s, %s) = %v", s.key, s.value, err)
		}
	}
	want := `{
  "schema_version": 1,
  "host": {
    "instance_id": "` + testID + `",
    "data_dir": "/var/lib/flats",
    "server_runtime": false
  },
  "network": {
    "permitted": ["tailscale", "portal"]
  },
  "system": {
    "keep_versions": 10
  },
  "portal": {
    "relays": ["https://relay-a.example.com", "https://relay.example.com"]
  },
  "credentials": {
    "operator_file": "/etc/flats/operator"
  }
}
`
	if got := string(doc.Encode()); got != want {
		t.Fatalf("Encode =\n%s\nwant\n%s", got, want)
	}
	if err := doc.Set("network.permitted", ""); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(doc.Encode(), []byte(`"permitted": []`)) {
		t.Error(`Set("") did not store an empty list`)
	}

	bad := []struct{ key, value string }{
		{"system.keep_versions", "1e3"},
		{"system.keep_versions", "+5"},
		{"system.keep_versions", "05"},
		{"system.keep_versions", " 5"},
		{"system.keep_versions", "9007199254740992"},
		{"host.server_runtime", "yes"},
		{"host.server_runtime", "True"},
		{"network.permitted", "portal,portal"},
		{"network.permitted", `["portal", null]`},
		{"network.permitted", `["portal"`},
		{"network.permitted", `[1]`},
		{"host.data_dir", "relative"},
		{"host.data_dir", "/srv/fl\xffats"},
		{"portal.relays", "https://rly.\xffbest"},
		{"schema_version", "2"},
		{"system.unknown", "1"},
	}
	for _, b := range bad {
		if err := doc.Set(b.key, b.value); err == nil {
			t.Errorf("Set(%s, %q) = nil, want error", b.key, b.value)
		}
	}

	for _, key := range []string{"host.instance_id", "host.data_dir"} {
		if err := doc.Unset(key); err == nil {
			t.Errorf("Unset(%s) = nil, want error", key)
		}
	}
	if err := doc.Unset("system.nope"); err == nil {
		t.Error("Unset of an unknown key succeeded")
	}
	for _, key := range []string{"network.permitted", "system.keep_versions", "system.keep_versions", "portal.relays", "credentials.operator_file", "host.server_runtime"} {
		if err := doc.Unset(key); err != nil {
			t.Errorf("Unset(%s) = %v", key, err)
		}
	}
	min, _ := New(testID, "/var/lib/flats")
	if !bytes.Equal(doc.Encode(), min.Encode()) {
		t.Errorf("after Unset:\n%s", doc.Encode())
	}

	// Cross-key rules are checked by Validate, not by Set.
	if err := doc.Set("network.private_backend", "tailscale"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); errPath(err) != "network.private_backend" {
		t.Errorf("Validate = %v", err)
	}
}

func TestNew(t *testing.T) {
	id := NewInstanceID()
	if _, err := checkUUID(id); err != nil || id[14] != '4' {
		t.Fatalf("NewInstanceID() = %q: %v", id, err)
	}
	if id == NewInstanceID() {
		t.Fatal("NewInstanceID repeated")
	}
	if _, err := New("not-a-uuid", "/x"); errPath(err) != "host.instance_id" {
		t.Errorf("New with a bad id: %v", err)
	}
	if _, err := New(id, "x"); errPath(err) != "host.data_dir" {
		t.Errorf("New with a relative dir: %v", err)
	}
}

// TestFixtures round-trips every golden fixture. Each schema version up to
// the current one must keep a fixture directory; files below the current
// version need a <name>.want.json with the migrated result.
func TestFixtures(t *testing.T) {
	for v := 1; v <= CurrentSchemaVersion; v++ {
		dir := filepath.Join("testdata", "config", fmt.Sprintf("v%d", v))
		inputs, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		inputs = slices.DeleteFunc(inputs, func(p string) bool { return strings.HasSuffix(p, ".want.json") })
		if len(inputs) == 0 {
			t.Fatalf("no fixtures in %s: add example files for schema_version %d", dir, v)
		}
		for _, in := range inputs {
			data, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			want := data
			if v < CurrentSchemaVersion {
				if want, err = os.ReadFile(strings.TrimSuffix(in, ".json") + ".want.json"); err != nil {
					t.Fatalf("%s: %v", in, err)
				}
			}
			l, err := Parse(data)
			if err != nil {
				t.Fatalf("%s: %v", in, err)
			}
			if l.FileVersion != v || l.Migrated != (v < CurrentSchemaVersion) {
				t.Errorf("%s: FileVersion %d Migrated %v", in, l.FileVersion, l.Migrated)
			}
			got := l.Doc.Encode()
			if !bytes.Equal(got, want) {
				t.Errorf("%s: encoded\n%s\nwant\n%s", in, got, want)
			}
			again, err := Parse(got)
			if err != nil || again.Migrated || !bytes.Equal(again.Doc.Encode(), got) {
				t.Errorf("%s: migrated result does not round-trip: %v", in, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join("testdata", "config", fmt.Sprintf("v%d", CurrentSchemaVersion+1))); err == nil {
		t.Errorf("fixtures exist for schema_version %d but CurrentSchemaVersion is %d", CurrentSchemaVersion+1, CurrentSchemaVersion)
	}
}

func TestProductionRegistry(t *testing.T) {
	if migrations.current != CurrentSchemaVersion {
		t.Fatalf("registry current = %d", migrations.current)
	}
	for v := 1; v < CurrentSchemaVersion; v++ {
		if !slices.ContainsFunc(migrations.steps, func(m Migration) bool { return m.From == v }) {
			t.Errorf("no migration from schema_version %d", v)
		}
	}
}

// Two addresses that pick free ports do not collide; equal fixed ports do.
func TestFreePortAddresses(t *testing.T) {
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	c, err := doc.Effective(map[string]string{"host.management_addr": "127.0.0.1:0", "host.local_addr": "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Host.ManagementAddr != "127.0.0.1:0" || c.Host.LocalAddr != "127.0.0.1:0" {
		t.Fatalf("Host = %+v", c.Host)
	}
	if _, err := doc.Effective(map[string]string{"host.management_addr": "localhost:9000", "host.local_addr": "localhost:9000"}); err == nil {
		t.Error("equal fixed addresses accepted")
	}
	// Values are stored exactly as written, so flag comparison is exact.
	if err := doc.Set("host.management_addr", "localhost:07878"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(doc.Encode(), []byte(`"management_addr": "localhost:07878"`)) {
		t.Errorf("address rewritten:\n%s", doc.Encode())
	}
}

func TestDefaultAndClone(t *testing.T) {
	if v, ok := Default("system.keep_versions"); !ok || v != int64(10) {
		t.Fatalf("Default(keep_versions) = %v, %t", v, ok)
	}
	if v, ok := Default("credentials.operator_file"); !ok || v != nil {
		t.Fatalf("Default(operator_file) = %v, %t", v, ok)
	}
	if _, ok := Default("nope"); ok {
		t.Fatal("unknown key has a default")
	}
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("portal.relays", "https://relay.example.com"); err != nil {
		t.Fatal(err)
	}
	c := doc.Clone()
	if err := c.Set("system.keep_versions", "3"); err != nil {
		t.Fatal(err)
	}
	c.values["portal.relays"].([]string)[0] = "changed"
	if bytes.Contains(doc.Encode(), []byte("keep_versions")) || !bytes.Contains(doc.Encode(), []byte("relay.example.com")) {
		t.Fatalf("clone shares state:\n%s", doc.Encode())
	}
}
