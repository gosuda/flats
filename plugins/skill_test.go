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
	versions := map[int]string{1: "0.3.0"}
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
