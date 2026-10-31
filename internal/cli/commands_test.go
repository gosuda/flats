package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestPrintURLsPublicInstanceOffEmptyPrivateURL(t *testing.T) {
	var out bytes.Buffer
	printURLs(&out, flatView{Visibility: "private"})
	if out.Len() != 0 {
		t.Fatalf("off-mode URLs = %q, want no output", out.String())
	}
}

func TestListPublicInstanceOffEmptyPrivateURL(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("GET /api/flats", 200, `{"flats":[{"slug":"site","type":"flat","visibility":"private","live_version":0,"versions":0,"private_url":""}]}`)
	r := run(t, srv.URL, "", "list")
	if r.code != ExitOK || r.stderr != "" {
		t.Fatalf("list: %+v", r)
	}
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("list = %q, want header and one row", r.stdout)
	}
	if fields := strings.Fields(lines[1]); !slices.Equal(fields, []string{"site", "flat", "private", "-", "0"}) {
		t.Fatalf("off-mode row = %q, want an empty URL cell", lines[1])
	}
}
