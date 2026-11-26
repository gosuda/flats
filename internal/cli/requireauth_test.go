package cli

import (
	"fmt"
	"strings"
	"testing"
)

// docs/spec/portal-require-auth.md, rule 11: `flats status` adds one note line when portal.require_auth is on, and names
// the flats that Portal does not serve.

func TestStatusNoteOfRequireAuth(t *testing.T) {
	twelve := make([]string, 12)
	for i := range twelve {
		twelve[i] = fmt.Sprintf(`"flat-%02d"`, i+1)
	}
	const on = "  note: portal.require_auth is on, so Portal serves only flats that have sign-in."
	for name, tc := range map[string]struct {
		system string
		want   string // the whole note line, or "" for no line
	}{
		"the setting is off":          {`"portal_require_auth": false, "portal_refused": []`, ""},
		"an older host":               {`"version": "1"`, ""},
		"on, no refused flat":         {`"portal_require_auth": true, "portal_refused": []`, on},
		"on, two refused flats":       {`"portal_require_auth": true, "portal_refused": ["blog", "shop"]`, on + " Portal does not serve these flats: blog, shop. The Networks section of each flat in the console says what to do, or turn the setting off in the console."},
		"on, more than ten flats":     {`"portal_require_auth": true, "portal_refused": [` + strings.Join(twelve, ",") + `]`, on + " Portal does not serve these flats: flat-01, flat-02, flat-03, flat-04, flat-05, flat-06, flat-07, flat-08, flat-09, flat-10, and 2 more. The Networks section of each flat in the console says what to do, or turn the setting off in the console."},
		"on, the list cannot be read": {`"portal_require_auth": true, "portal_refused": null`, on + " The refused flats cannot be read now."},
	} {
		t.Run(name, func(t *testing.T) {
			api, srv := newFakeAPI(t)
			api.handle("GET /api/status", 200, `{"ok": true, "system": {`+tc.system+`}}`)
			r := runEnv(t, Env{GOOS: "linux"}, srv.URL, "status")
			if r.code != 0 {
				t.Fatalf("status = %d\n%s%s", r.code, r.stdout, r.stderr)
			}
			var lines []string
			for _, l := range strings.Split(r.stdout, "\n") {
				if strings.Contains(l, "portal.require_auth is on") {
					lines = append(lines, l)
				}
			}
			switch {
			case tc.want == "" && len(lines) != 0:
				t.Fatalf("a note without the setting:\n%s", r.stdout)
			case tc.want != "" && (len(lines) != 1 || lines[0] != tc.want):
				t.Fatalf("note lines = %q, want %q", lines, tc.want)
			}
		})
	}
}
