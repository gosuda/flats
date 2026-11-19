package console

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// The answer of a revocation whose public routes did not open again holds the phrase of core, and signin.js shows
// such an answer as an error by that phrase (rule 54). TestConsoleUI feeds one and expects the error toast.
func TestRevokeFailureMatchesCore(t *testing.T) {
	src := readStatic(t, "assets/js/signin.js")
	if decl := "export const SIGN_IN_ROUTES_NOT_REOPENED = '" + core.SignInRoutesNotReopened + "';"; !strings.Contains(src, decl) {
		t.Errorf("signin.js differs from internal/core; want %s", decl)
	}
	if use := "msg.includes(SIGN_IN_ROUTES_NOT_REOPENED) ? 'error' : 'success'"; !strings.Contains(src, use) {
		t.Errorf("signin.js does not choose the toast of a revocation by SIGN_IN_ROUTES_NOT_REOPENED; want %s", use)
	}
}

// The console reads the sign-in of a flat, issues access links and ends the sessions and links. It has no request that
// changes the setting and no route for the client secret: a change is requested with `flats signin set` on the host,
// and the client secret is set with `flats signin secret set` (docs/spec/portal-sign-in.md, rules 13, 25, 49, 54 and
// 63). TestConsoleUI checks what the page does with these routes.
func TestSignInRoutesOfTheConsole(t *testing.T) {
	src := readStatic(t, "assets/js/api.js")
	verb := regexp.MustCompile(`\b(get|post)\(\x60|request\('([A-Z]+)'`)
	var got []string
	for _, line := range strings.Split(src, "\n") {
		i := strings.Index(line, "/signin")
		if i < 0 || strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		m := verb.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("api.js names a sign-in route without a method: %s", line)
		}
		method := strings.ToUpper(m[1])
		if m[2] != "" {
			method = m[2]
		}
		path := line[i:]
		got = append(got, method+" "+path[:strings.IndexAny(path, "`'")])
	}
	slices.Sort(got)
	if want := []string{"GET /signin", "POST /signin/links", "POST /signin/revoke"}; !slices.Equal(got, want) {
		t.Errorf("api.js calls the sign-in routes %q, want %q", got, want)
	}
}
