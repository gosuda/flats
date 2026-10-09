package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	const commit = "dba5279b25052e8b31a4fd15c6c3765de68fc497"
	vcs := func(main string, dirty bool) *debug.BuildInfo {
		modified := "false"
		if dirty {
			modified = "true"
		}
		return &debug.BuildInfo{Main: debug.Module{Version: main}, Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: commit}, {Key: "vcs.modified", Value: modified}}}
	}
	module := func(main string) *debug.BuildInfo { return &debug.BuildInfo{Main: debug.Module{Version: main}} }
	for _, tc := range []struct {
		name    string
		stamped string
		bi      *debug.BuildInfo
		version string
		release bool
		commit  string
		dirty   bool
	}{
		{"release archive", "v0.2.0", vcs("v0.0.0-20261006012941-dba5279b2505", false), "v0.2.0", true, commit, false},
		{"release prerelease", "v1.3.0-rc.1", vcs("v1.3.0-rc.1", false), "v1.3.0-rc.1", true, commit, false},
		{"go install at a tag", "", module("v0.2.0"), "v0.2.0", true, "", false},
		{"go build of a clean tagged checkout", "", vcs("v0.2.0", false), "v0.2.0", true, commit, false},
		{"go build of a modified tagged checkout", "", vcs("v0.2.0+dirty", true), "dba5279-dirty", false, commit, true},
		{"go build of a checkout", "", vcs("v0.2.1-0.20261006012941-dba5279b2505", false), "dba5279", false, commit, false},
		{"go build of a modified checkout", "", vcs("v0.2.1-0.20261006012941-dba5279b2505+dirty", true), "dba5279-dirty", false, commit, true},
		{"go build without module version", "", vcs("(devel)", false), "dba5279", false, commit, false},
		{"go install at main", "", module("v0.2.1-0.20261006012941-dba5279b2505"), "dba5279", false, "", false},
		{"go install at a prerelease pseudo-version", "", module("v1.3.0-rc.1.0.20261006012941-dba5279b2505"), "dba5279", false, "", false},
		{"go install at an untagged module", "", module("v0.0.0-20261006012941-dba5279b2505"), "dba5279", false, "", false},
		{"stamped dev", "dev", vcs("(devel)", true), "dba5279-dirty", false, commit, true},
		{"main commit image", "v0.0.0-sha-dba5279", vcs("v0.2.1-0.20261006012941-dba5279b2505", false), "dba5279", false, commit, false},
		{"main commit image without VCS stamp", "v0.0.0-sha-dba5279", module("(devel)"), "dba5279", false, "", false},
		{"local image build", "v0.0.0-dev", vcs("(devel)", false), "dba5279", false, commit, false},
		{"stamped non-version", "nightly", vcs("(devel)", false), "dba5279", false, commit, false},
		{"nothing known", "", module("(devel)"), "dev", false, "", false},
		{"no build info", "", nil, "dev", false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(tc.stamped, tc.bi)
			if got.Version != tc.version || got.Release != tc.release || got.Commit != tc.commit || got.Dirty != tc.dirty {
				t.Errorf("resolve = %+v; want version %q release %v commit %q dirty %v", got, tc.version, tc.release, tc.commit, tc.dirty)
			}
			if got.Go == "" || got.OS == "" || got.Arch == "" {
				t.Errorf("resolve = %+v; want the Go version, OS and architecture", got)
			}
		})
	}
}
