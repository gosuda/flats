package plugins_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/gosuda/flats/internal/clientstate"
	"github.com/gosuda/flats/plugins"
)

func TestBootstrapReleaseContract(t *testing.T) {
	revision, err := clientstate.SkillRevision(plugins.Skill)
	if err != nil {
		t.Fatal(err)
	}
	if revision != clientstate.BootstrapRevision {
		t.Fatalf("skill revision %d != server revision %d", revision, clientstate.BootstrapRevision)
	}
	// Every bootstrap behavior revision is paired with an explicit plugin release.
	versions := map[int]string{1: "0.3.0", 2: "0.4.0"}
	want, ok := versions[revision]
	if !ok {
		t.Fatal("add the plugin release paired with the new bootstrap revision")
	}
	for _, path := range []string{"flats/.claude-plugin/plugin.json", "flats/.codex-plugin/plugin.json", "flats/.cursor-plugin/plugin.json"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var plugin struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(b, &plugin); err != nil {
			t.Fatal(err)
		}
		if plugin.Version != want {
			t.Errorf("%s version=%s, bootstrap %d requires %s", path, plugin.Version, revision, want)
		}
	}
}

func TestMarketplaceReleaseContract(t *testing.T) {
	// Claude pins the plugin release; Cursor versions the marketplace metadata.
	for _, tc := range []struct {
		name, path string
		metadata   bool
	}{
		{name: "Claude", path: "../.claude-plugin/marketplace.json"},
		{name: "Cursor", path: "../.cursor-plugin/marketplace.json", metadata: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			var catalog struct {
				Metadata struct {
					Version string `json:"version"`
				} `json:"metadata"`
				Plugins []struct{ Name, Version string } `json:"plugins"`
			}
			if err := json.Unmarshal(b, &catalog); err != nil {
				t.Fatal(err)
			}
			b, err = os.ReadFile("flats/.codex-plugin/plugin.json")
			if err != nil {
				t.Fatal(err)
			}
			var plugin struct {
				Version string `json:"version"`
			}
			if err := json.Unmarshal(b, &plugin); err != nil {
				t.Fatal(err)
			}
			version := catalog.Metadata.Version
			if !tc.metadata {
				version = ""
				for _, p := range catalog.Plugins {
					if p.Name == "flats" {
						version = p.Version
					}
				}
			}
			if version != plugin.Version {
				t.Fatalf("catalog release %q != plugin release %q", version, plugin.Version)
			}
		})
	}
	// The Codex catalog has no version field; its local source must select the
	// same versioned plugin directory rather than a stale alternate copy.
	b, err := os.ReadFile("../.agents/plugins/marketplace.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Plugins []struct {
			Name   string                        `json:"name"`
			Source struct{ Source, Path string } `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, plugin := range catalog.Plugins {
		if plugin.Name == "flats" {
			if plugin.Source.Source != "local" || plugin.Source.Path != "./plugins/flats" {
				t.Fatalf("unexpected Codex source: %+v", plugin.Source)
			}
			return
		}
	}
	t.Fatal("Codex catalog lacks Flats")
}
