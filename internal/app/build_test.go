package app

import (
	"testing"

	"github.com/gosuda/flats/internal/buildinfo"
)

// /api/status reports the running build, and keeps system.version for
// clients that read the version there.
func TestStatusReportsBuild(t *testing.T) {
	h := startLocal(t)
	var st struct {
		System struct {
			Version string         `json:"version"`
			Build   buildinfo.Info `json:"build"`
		} `json:"system"`
	}
	if code := call(t, "GET", "http://"+h.Addr()+"/api/status", nil, &st); code != 200 {
		t.Fatalf("status %d", code)
	}
	want := buildinfo.Get()
	if st.System.Build != want || st.System.Version != want.Version {
		t.Errorf("status build = %+v, version %q; want %+v", st.System.Build, st.System.Version, want)
	}
	if st.System.Build.Version == "" || st.System.Build.Go == "" {
		t.Errorf("status build = %+v; want a version and Go version", st.System.Build)
	}
}
