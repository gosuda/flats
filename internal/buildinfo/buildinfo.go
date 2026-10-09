// Package buildinfo reports which build of Flats is running: the release
// version for a release, otherwise the commit it was built from.
package buildinfo

import (
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Version is the release version, set by scripts/build-release.sh with
// -ldflags "-X github.com/gosuda/flats/internal/buildinfo.Version=v1.2.3".
var Version = ""

// ShortCommit is the number of hex digits of a commit shown in a version,
// as git abbreviates it and as the image's sha-<commit> tag is named.
const ShortCommit = 7

// Info describes the running build.
type Info struct {
	// Version is the release (v1.2.3) for a release build, else the short
	// commit with "-dirty" for uncommitted changes, else "dev".
	Version string `json:"version"`
	// Release is true when Version is a release.
	Release bool `json:"release"`
	// Commit is the full commit the binary was built from, when known.
	Commit string `json:"commit"`
	// Dirty is true when the source had uncommitted changes.
	Dirty bool   `json:"dirty"`
	Go    string `json:"go"`
	OS    string `json:"os"`
	Arch  string `json:"arch"`
}

var get = sync.OnceValue(func() Info {
	bi, _ := debug.ReadBuildInfo()
	return resolve(Version, bi)
})

// Get returns the running build.
func Get() Info { return get() }

// pseudo matches the timestamp and commit of a Go pseudo-version such as
// v0.2.1-0.20261006012941-dba5279b2505 (after any +dirty is removed).
var pseudo = regexp.MustCompile(`(?:^|[-.])\d{14}-([0-9a-f]{12})$`)

// release matches a release tag: v1.2.3 or v1.3.0-rc.1, without build metadata.
var release = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

func resolve(stamped string, bi *debug.BuildInfo) Info {
	info := Info{Version: "dev", Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	mod := ""
	if bi != nil {
		mod = bi.Main.Version
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.modified":
				info.Dirty = s.Value == "true"
			}
		}
	}
	// Go stamps a build of a modified checkout with a +dirty module version.
	if v, ok := strings.CutSuffix(mod, "+dirty"); ok {
		mod, info.Dirty = v, true
	}
	m := pseudo.FindStringSubmatch(mod)
	switch {
	case stamped != "" && stamped != "dev":
		// A release archive; the tag names the version.
		info.Version, info.Release = stamped, true
	case m == nil && release.MatchString(mod) && !info.Dirty:
		// go install ...@v1.2.3, or go build of a clean tagged checkout.
		info.Version, info.Release = mod, true
	case info.Commit != "":
		info.Version = commitVersion(info.Commit, info.Dirty)
	case m != nil:
		// go install ...@main: no VCS stamp, only the pseudo-version's commit
		// prefix, so Commit stays unknown.
		info.Version = commitVersion(m[1], info.Dirty)
	}
	return info
}

func commitVersion(commit string, dirty bool) string {
	v := strings.ToLower(commit)
	if len(v) > ShortCommit {
		v = v[:ShortCommit]
	}
	if dirty {
		v += "-dirty"
	}
	return v
}
