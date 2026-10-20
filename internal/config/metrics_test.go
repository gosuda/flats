package config

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestMetricsDefaults(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("schema_%d", version), func(t *testing.T) {
			raw := strings.Replace(fileWith(), `"schema_version": 5`, fmt.Sprintf(`"schema_version": %d`, version), 1)
			loaded, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := loaded.Doc.Effective(nil)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Metrics.Enabled || !slices.Equal(cfg.Metrics.ExposeOn, []string{"loopback"}) || cfg.Metrics.PerFlat {
				t.Fatalf("Metrics = %+v, want enabled, loopback only and no per-flat series", cfg.Metrics)
			}
			for _, key := range []string{"metrics.enabled", "metrics.expose_on", "metrics.per_flat"} {
				if _, present := loaded.Doc.Stored(key); present {
					t.Errorf("missing %s was persisted", key)
				}
				if _, source, ok := cfg.Lookup(key); !ok || source != SourceDefault {
					t.Errorf("Lookup(%s) source = %q, %t; want default", key, source, ok)
				}
			}
			if bytes.Contains(loaded.Doc.Encode(), []byte(`"metrics"`)) {
				t.Fatal("defaults added metrics to sparse document")
			}
		})
	}
}

func TestMetricsParse(t *testing.T) {
	tests := []struct {
		name, members, wantPath string
		want                    MetricsConfig
	}{
		{name: "empty section", want: MetricsConfig{Enabled: true, ExposeOn: []string{"loopback"}}},
		{name: "previous behavior", members: `"enabled": true, "expose_on": ["external", "loopback", "console"], "per_flat": true`,
			want: MetricsConfig{Enabled: true, ExposeOn: []string{"loopback", "console", "external"}, PerFlat: true}},
		{name: "disabled independently", members: `"enabled": false, "per_flat": true`,
			want: MetricsConfig{ExposeOn: []string{"loopback"}, PerFlat: true}},
		{name: "empty exposure", members: `"expose_on": []`, want: MetricsConfig{Enabled: true, ExposeOn: []string{}}},
		{name: "enabled string", members: `"enabled": "true"`, wantPath: "metrics.enabled"},
		{name: "enabled number", members: `"enabled": 1`, wantPath: "metrics.enabled"},
		{name: "enabled null", members: `"enabled": null`, wantPath: "metrics.enabled"},
		{name: "per-flat string", members: `"per_flat": "false"`, wantPath: "metrics.per_flat"},
		{name: "per-flat number", members: `"per_flat": 0`, wantPath: "metrics.per_flat"},
		{name: "per-flat null", members: `"per_flat": null`, wantPath: "metrics.per_flat"},
		{name: "exposure string", members: `"expose_on": "loopback"`, wantPath: "metrics.expose_on"},
		{name: "exposure object", members: `"expose_on": {}`, wantPath: "metrics.expose_on"},
		{name: "exposure null", members: `"expose_on": null`, wantPath: "metrics.expose_on"},
		{name: "exposure member number", members: `"expose_on": [1]`, wantPath: "metrics.expose_on[0]"},
		{name: "exposure member null", members: `"expose_on": [null]`, wantPath: "metrics.expose_on[0]"},
		{name: "unknown exposure", members: `"expose_on": ["public"]`, wantPath: "metrics.expose_on[0]"},
		{name: "case-sensitive exposure", members: `"expose_on": ["Loopback"]`, wantPath: "metrics.expose_on[0]"},
		{name: "duplicate exposure", members: `"expose_on": ["console", "console"]`, wantPath: "metrics.expose_on[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loaded, err := Parse([]byte(fileWith("metrics", tt.members)))
			if tt.wantPath != "" {
				if err == nil || errPath(err) != tt.wantPath {
					t.Fatalf("Parse error = %v, want path %s", err, tt.wantPath)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := loaded.Doc.Effective(nil)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Metrics.Enabled != tt.want.Enabled || cfg.Metrics.PerFlat != tt.want.PerFlat ||
				!slices.Equal(cfg.Metrics.ExposeOn, tt.want.ExposeOn) {
				t.Fatalf("Metrics = %+v, want %+v", cfg.Metrics, tt.want)
			}
			if tt.name == "empty exposure" && cfg.Metrics.ExposeOn == nil {
				t.Fatal("explicit empty exposure became nil")
			}
		})
	}
}

func TestMetricsSetRoundTripAndUnset(t *testing.T) {
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []struct{ key, value string }{
		{"metrics.enabled", "true"},
		{"metrics.expose_on", "external, loopback, console"},
		{"metrics.per_flat", "false"},
	} {
		if err := doc.Set(setting.key, setting.value); err != nil {
			t.Fatal(err)
		}
	}
	encoded := doc.Encode()
	for _, member := range []string{`"enabled": true`, `"expose_on": ["loopback", "console", "external"]`, `"per_flat": false`} {
		if !bytes.Contains(encoded, []byte(member)) {
			t.Fatalf("encoded config omits %s:\n%s", member, encoded)
		}
	}
	loaded, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Doc.Encode(), encoded) {
		t.Fatal("round trip changed document")
	}
	for _, key := range []string{"metrics.enabled", "metrics.expose_on", "metrics.per_flat"} {
		cfg, err := loaded.Doc.Effective(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, source, _ := cfg.Lookup(key); source != SourceFile {
			t.Errorf("explicit %s source = %q, want file", key, source)
		}
		if err := loaded.Doc.Unset(key); err != nil {
			t.Fatal(err)
		}
	}
	if bytes.Contains(loaded.Doc.Encode(), []byte(`"metrics"`)) {
		t.Fatal("Unset retained metrics section")
	}
	cfg, err := loaded.Doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Metrics.Enabled || cfg.Metrics.PerFlat || !slices.Equal(cfg.Metrics.ExposeOn, []string{"loopback"}) {
		t.Fatalf("Unset defaults = %+v", cfg.Metrics)
	}
	for _, value := range []string{"[]", ""} {
		if err := doc.Set("metrics.expose_on", value); err != nil {
			t.Fatal(err)
		}
		loaded, err := Parse(doc.Encode())
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := loaded.Doc.Effective(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Metrics.ExposeOn) != 0 || cfg.Metrics.ExposeOn == nil {
			t.Fatalf("Set(%q) exposure = %#v, want []", value, cfg.Metrics.ExposeOn)
		}
	}
}

func TestMetricsSetRejectsInvalidValues(t *testing.T) {
	tests := []struct{ key, value, wantPath string }{
		{"metrics.enabled", "1", "metrics.enabled"},
		{"metrics.enabled", "null", "metrics.enabled"},
		{"metrics.per_flat", "False", "metrics.per_flat"},
		{"metrics.per_flat", "null", "metrics.per_flat"},
		{"metrics.expose_on", "null", "metrics.expose_on[0]"},
		{"metrics.expose_on", "public", "metrics.expose_on[0]"},
		{"metrics.expose_on", "loopback,loopback", "metrics.expose_on[1]"},
		{"metrics.expose_on", `["external", "external"]`, "metrics.expose_on[1]"},
		{"metrics.expose_on", `[1]`, "metrics.expose_on[0]"},
		{"metrics.expose_on", `[null]`, "metrics.expose_on[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"/"+tt.value, func(t *testing.T) {
			doc, err := New(testID, "/var/lib/flats")
			if err != nil {
				t.Fatal(err)
			}
			before := doc.Encode()
			if err := doc.Set(tt.key, tt.value); err == nil || errPath(err) != tt.wantPath {
				t.Fatalf("Set error = %v, want path %s", err, tt.wantPath)
			}
			if !bytes.Equal(before, doc.Encode()) {
				t.Fatal("rejected value changed document")
			}
		})
	}
}

func TestMetricsExposureSlicesAreIndependent(t *testing.T) {
	v, _ := Default("metrics.expose_on")
	v.([]string)[0] = "external"
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Metrics.ExposeOn[0] = "console"
	v, _, _ = cfg.Lookup("metrics.expose_on")
	v.([]string)[0] = "external"
	cfg, err = doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Metrics.ExposeOn, []string{"loopback"}) {
		t.Fatalf("default exposure mutated: %v", cfg.Metrics.ExposeOn)
	}
	if err := doc.Set("metrics.expose_on", "console"); err != nil {
		t.Fatal(err)
	}
	clone := doc.Clone()
	clone.values["metrics.expose_on"].([]string)[0] = "external"
	if v, _ := doc.Stored("metrics.expose_on"); !slices.Equal(v.([]string), []string{"console"}) {
		t.Fatal("clone shares exposure list")
	}
}
