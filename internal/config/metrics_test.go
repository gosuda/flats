package config

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestMetricsDefaults(t *testing.T) {
	if value, ok := Default("metrics.per_flat"); !ok || value != true {
		t.Fatalf("Default(metrics.per_flat) = %v, %t; want true", value, ok)
	}
	for version := 1; version <= CurrentSchemaVersion; version++ {
		t.Run(fmt.Sprintf("schema_%d", version), func(t *testing.T) {
			raw := strings.Replace(fileWith(), fmt.Sprintf(`"schema_version": %d`, CurrentSchemaVersion),
				fmt.Sprintf(`"schema_version": %d`, version), 1)
			loaded, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if loaded.FileVersion != version {
				t.Fatalf("FileVersion = %d, want %d", loaded.FileVersion, version)
			}
			cfg, err := loaded.Doc.Effective(nil)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Metrics.Enabled || !slices.Equal(cfg.Metrics.ExposeOn, []string{"localhost"}) || !cfg.Metrics.PerFlat {
				t.Fatalf("Metrics = %+v, want enabled, localhost only and per-flat series", cfg.Metrics)
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

func TestMetricsExplicitPerFlatFalseAcrossSchemas(t *testing.T) {
	for version := 1; version <= CurrentSchemaVersion; version++ {
		t.Run(fmt.Sprintf("schema_%d", version), func(t *testing.T) {
			raw := strings.Replace(fileWith("metrics", `"per_flat":false`),
				fmt.Sprintf(`"schema_version": %d`, CurrentSchemaVersion),
				fmt.Sprintf(`"schema_version": %d`, version), 1)
			loaded, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if loaded.FileVersion != version {
				t.Fatalf("FileVersion = %d, want %d", loaded.FileVersion, version)
			}
			cfg, err := loaded.Doc.Effective(nil)
			if err != nil {
				t.Fatal(err)
			}
			value, source, ok := cfg.Lookup("metrics.per_flat")
			if cfg.Metrics.PerFlat || !ok || value != false || source != SourceFile {
				t.Fatalf("explicit false = %v, %v, %s, %t", cfg.Metrics.PerFlat, value, source, ok)
			}
			encoded := loaded.Doc.Encode()
			if !bytes.Contains(encoded, []byte(`"per_flat": false`)) {
				t.Fatalf("explicit false dropped from encoding:\n%s", encoded)
			}
			again, err := Parse(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again.Doc.Encode(), encoded) {
				t.Fatal("explicit false changed after round trip")
			}
			if err := again.Doc.Unset("metrics.per_flat"); err != nil {
				t.Fatal(err)
			}
			cfg, err = again.Doc.Effective(nil)
			if err != nil {
				t.Fatal(err)
			}
			value, source, ok = cfg.Lookup("metrics.per_flat")
			if !cfg.Metrics.PerFlat || !ok || value != true || source != SourceDefault {
				t.Fatalf("unset default = %v, %v, %s, %t", cfg.Metrics.PerFlat, value, source, ok)
			}
			if bytes.Contains(again.Doc.Encode(), []byte(`"metrics"`)) {
				t.Fatal("Unset retained metrics section")
			}
		})
	}
}

func TestMetricsDefaultsPreserveMigratedConsoleAddress(t *testing.T) {
	raw := fmt.Sprintf(`{"schema_version":4,"host":{"instance_id":%q,"data_dir":"/var/lib/flats","management_addr":"100.64.0.7:9000"}}`, testID)
	loaded, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.FileVersion != 4 || loaded.Doc.SchemaVersion() != CurrentSchemaVersion || !loaded.Migrated || loaded.NeedsConfirm {
		t.Fatalf("migration metadata = %+v", loaded)
	}
	cfg, err := loaded.Doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host.ConsoleAddr != "100.64.0.7:9000" || !cfg.Metrics.Enabled || !cfg.Metrics.PerFlat ||
		!slices.Equal(cfg.Metrics.ExposeOn, []string{"localhost"}) {
		t.Fatalf("migrated host = %+v, metrics = %+v", cfg.Host, cfg.Metrics)
	}
	if value, present := loaded.Doc.Stored("host.console_addr"); !present || value != "100.64.0.7:9000" {
		t.Fatalf("stored canonical address = %v, %t", value, present)
	}
	if bytes.Contains(loaded.Doc.Encode(), []byte("management_addr")) || bytes.Contains(loaded.Doc.Encode(), []byte(`"metrics"`)) {
		t.Fatalf("migrated config retained old key or stored metrics defaults:\n%s", loaded.Doc.Encode())
	}
}

func TestMetricsParse(t *testing.T) {
	tests := []struct {
		name, members, wantPath string
		want                    MetricsConfig
	}{
		{name: "empty section", want: MetricsConfig{Enabled: true, ExposeOn: []string{"localhost"}, PerFlat: true}},
		{name: "previous behavior", members: `"enabled": true, "expose_on": ["external", "localhost", "console"], "per_flat": true`,
			want: MetricsConfig{Enabled: true, ExposeOn: []string{"localhost", "console", "external"}, PerFlat: true}},
		{name: "disabled independently", members: `"enabled": false, "per_flat": true`,
			want: MetricsConfig{ExposeOn: []string{"localhost"}, PerFlat: true}},
		{name: "empty exposure", members: `"expose_on": []`, want: MetricsConfig{Enabled: true, ExposeOn: []string{}, PerFlat: true}},
		{name: "enabled string", members: `"enabled": "true"`, wantPath: "metrics.enabled"},
		{name: "enabled number", members: `"enabled": 1`, wantPath: "metrics.enabled"},
		{name: "enabled null", members: `"enabled": null`, wantPath: "metrics.enabled"},
		{name: "per-flat disabled explicitly", members: `"per_flat": false`, want: MetricsConfig{Enabled: true, ExposeOn: []string{"localhost"}}},
		{name: "per-flat string", members: `"per_flat": "false"`, wantPath: "metrics.per_flat"},
		{name: "per-flat number", members: `"per_flat": 0`, wantPath: "metrics.per_flat"},
		{name: "per-flat null", members: `"per_flat": null`, wantPath: "metrics.per_flat"},
		{name: "exposure string", members: `"expose_on": "localhost"`, wantPath: "metrics.expose_on"},
		{name: "exposure object", members: `"expose_on": {}`, wantPath: "metrics.expose_on"},
		{name: "exposure null", members: `"expose_on": null`, wantPath: "metrics.expose_on"},
		{name: "exposure member number", members: `"expose_on": [1]`, wantPath: "metrics.expose_on[0]"},
		{name: "exposure member null", members: `"expose_on": [null]`, wantPath: "metrics.expose_on[0]"},
		{name: "legacy loopback exposure", members: `"expose_on": ["loopback"]`, wantPath: "metrics.expose_on[0]"},
		{name: "unknown exposure", members: `"expose_on": ["public"]`, wantPath: "metrics.expose_on[0]"},
		{name: "case-sensitive exposure", members: `"expose_on": ["Localhost"]`, wantPath: "metrics.expose_on[0]"},
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
		{"metrics.expose_on", "external, localhost, console"},
		{"metrics.per_flat", "false"},
	} {
		if err := doc.Set(setting.key, setting.value); err != nil {
			t.Fatal(err)
		}
	}
	encoded := doc.Encode()
	for _, member := range []string{`"enabled": true`, `"expose_on": ["localhost", "console", "external"]`, `"per_flat": false`} {
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
	if !cfg.Metrics.Enabled || !cfg.Metrics.PerFlat || !slices.Equal(cfg.Metrics.ExposeOn, []string{"localhost"}) {
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
		{"metrics.expose_on", "loopback", "metrics.expose_on[0]"},
		{"metrics.expose_on", "public", "metrics.expose_on[0]"},
		{"metrics.expose_on", "localhost,localhost", "metrics.expose_on[1]"},
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
	if !slices.Equal(cfg.Metrics.ExposeOn, []string{"localhost"}) {
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

func TestMetricsRejectsOldFlatMetricsKey(t *testing.T) {
	for _, version := range []int{4, CurrentSchemaVersion} {
		for _, members := range []string{`"flat_metrics":true`, `"flat_metrics":false,"per_flat":true`, `"per_flat":false,"flat_metrics":true`} {
			t.Run(fmt.Sprintf("schema_%d/%s", version, members), func(t *testing.T) {
				raw := strings.Replace(fileWith("metrics", members),
					fmt.Sprintf(`"schema_version": %d`, CurrentSchemaVersion),
					fmt.Sprintf(`"schema_version": %d`, version), 1)
				before := []byte(raw)
				input := bytes.Clone(before)
				if _, err := Parse(input); errPath(err) != "metrics.flat_metrics" || !strings.Contains(err.Error(), "unknown key") {
					t.Fatalf("Parse error = %v, want unknown metrics.flat_metrics", err)
				}
				if !bytes.Equal(input, before) {
					t.Fatal("rejected config input was modified")
				}
			})
		}
	}
	doc, err := New(testID, "/var/lib/flats")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("metrics.per_flat", "true"); err != nil {
		t.Fatal(err)
	}
	before := doc.Encode()
	if err := doc.Set("metrics.flat_metrics", "false"); errPath(err) != "metrics.flat_metrics" {
		t.Fatalf("old-key Set = %v", err)
	}
	if !bytes.Equal(doc.Encode(), before) {
		t.Fatal("rejected Set changed canonical metrics value")
	}
	if err := doc.Unset("metrics.flat_metrics"); errPath(err) != "metrics.flat_metrics" {
		t.Fatalf("old-key Unset = %v", err)
	}
	if !bytes.Equal(doc.Encode(), before) {
		t.Fatal("rejected Unset changed canonical metrics value")
	}
	cfg, err := doc.Effective(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Metrics.PerFlat {
		t.Fatal("old-key operations disabled canonical per_flat")
	}
	if _, _, ok := cfg.Lookup("metrics.flat_metrics"); ok {
		t.Fatal("old-key Lookup succeeded")
	}
	if _, ok := Default("metrics.flat_metrics"); ok {
		t.Fatal("old key has a default")
	}
	if !slices.Contains(Keys(), "metrics.per_flat") || slices.Contains(Keys(), "metrics.flat_metrics") {
		t.Fatal("key discovery exposes wrong metrics key")
	}
}
