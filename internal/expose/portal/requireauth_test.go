package portal

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gosuda/portal-tunnel/v2/types"

	"github.com/gosuda/flats/internal/core"
)

// docs/spec/portal-require-auth.md, rules 7 and 8, on the Portal route: an open that carries the refusal of
// portal.require_auth opens nothing for a new entry, and stops and unregisters a served one.

const requireAuthDetail = "portal.require_auth is on and app has no sign-in. Run flats signin set app to turn sign-in on, or turn the setting off in the console."

func refused() *core.PortalSignIn { return &core.PortalSignIn{Seq: 1, Refuse: requireAuthDetail} }

// wantAuthError checks that err is the refusal, and says whether its stop was confirmed.
func wantAuthError(t *testing.T, err error, confirmed bool) *core.PortalAuthError {
	t.Helper()
	var ae *core.PortalAuthError
	if !errors.As(err, &ae) || ae.Detail != requireAuthDetail || (ae.StopErr == nil) != confirmed {
		t.Fatalf("error = %v, want the refusal with a confirmed stop %t", err, confirmed)
	}
	if !confirmed && !strings.HasSuffix(err.Error(), "; the stop of the route is not confirmed: "+ae.StopErr.Error()) {
		t.Fatalf("the text of an unconfirmed stop = %q", err)
	}
	if confirmed && err.Error() != requireAuthDetail {
		t.Fatalf("the text of the refusal = %q, want the detail alone", err)
	}
	return ae
}

func TestRequireAuthRefusesANewEntry(t *testing.T) {
	dir := t.TempDir()
	n, ff := newTestNet(t, Config{Discovery: true, Dir: dir})
	var runs atomic.Int32
	n.runHTTP = func(ctx context.Context, ln net.Listener, h http.Handler) error {
		runs.Add(1)
		<-ctx.Done()
		return nil
	}
	var flat flatRecorder
	_, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, refused())
	wantAuthError(t, err, true)
	if ff.count() != 0 || runs.Load() != 0 || n.entry("app") != nil {
		t.Fatalf("a refused open made a route: %d exposures, %d HTTP runs", ff.count(), runs.Load())
	}
	if _, err := os.Stat(filepath.Join(dir, "app.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused open made an identity file: %v", err)
	}
	for _, h := range n.Status().Hosts {
		if h.Host == "app" {
			t.Fatalf("a refused flat is a host of the backend: %+v", h)
		}
	}

	// The next open without the refusal opens the route (rule 14).
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, &core.PortalSignIn{Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if ff.count() != 1 || n.entry("app") == nil {
		t.Fatal("the open after the refusal did not open the route")
	}
}

// The mark Redirect is never refused, and an open that carries nothing is not refused: the backend cannot know the
// setting (rule 7).
func TestRequireAuthDoesNotRefuseTheRedirectOrAnOpenThatCarriesNothing(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	var flat flatRecorder
	if _, err := n.ServeSignIn(t.Context(), "old", flat.handler("redirect"), true, &core.PortalSignIn{Redirect: true, Refuse: requireAuthDetail}); err != nil {
		t.Fatalf("the redirect open = %v", err)
	}
	if _, err := n.Serve(t.Context(), "plain", flat.handler("plain"), false); err != nil {
		t.Fatalf("an open that carries nothing = %v", err)
	}
	if ff.count() != 2 || n.entry("old") == nil || n.entry("plain") == nil {
		t.Fatalf("%d exposures, want 2", ff.count())
	}
}

func TestRequireAuthStopsAServedEntry(t *testing.T) {
	var logs logLines
	n, ff := newTestNet(t, Config{Discovery: true, Logf: logs.logf})
	var flat flatRecorder
	v, f := serveGated(t, n, ff, flat.handler("flat"), credentialGate(1, "guest"))
	signInWith(t, n, v, "guest")
	wantFlat(t, v, "/", "flat /")
	e := n.entry("app")
	before := flat.count()

	_, err := n.ServeSignIn(t.Context(), "app", flat.handler("other"), false, refused())
	wantAuthError(t, err, true)
	// The route stopped and unregistered, and it never serves the flat again: the front answers 503.
	if f.closeCount() != 1 || n.entry("app") != nil {
		t.Fatalf("the served route did not stop: %d closes", f.closeCount())
	}
	if code := drained(e, v); code != http.StatusServiceUnavailable || flat.count() != before {
		t.Fatalf("a request after the refusal = %d, %d flat calls, want 503 and none", code, flat.count()-before)
	}
	// The endpoint of the flat is the manager's, so the backend holds no failure of the gate for it.
	for _, h := range n.Status().Hosts {
		if h.Host == "app" {
			t.Fatalf("a refused flat is a host of the backend: %+v", h)
		}
	}
	if lines := logs.with("sign-in gate"); len(lines) != 0 {
		t.Fatalf("a refusal wrote a line of a gate failure: %q", lines)
	}

	// The next open with a gate opens the route again.
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, credentialGate(2, "guest")); err != nil {
		t.Fatal(err)
	}
	if n.entry("app") == nil || ff.count() != 2 {
		t.Fatal("the route did not open again")
	}
}

// The stop of a served entry that is not confirmed keeps the route registered, and the front answers 503 and never the
// flat (rule 8).
func TestRequireAuthStopThatIsNotConfirmed(t *testing.T) {
	n, ff := newTestNet(t, Config{Discovery: true})
	n.expose = func(ctx context.Context, id types.Identity, relays []string, maxActive int, meta types.LeaseMetadata) (exposure, error) {
		exp, err := ff.expose(ctx, id, relays, maxActive, meta)
		return failingClose{exp}, err
	}
	var flat flatRecorder
	v, _ := serveGated(t, n, ff, flat.handler("flat"), credentialGate(1, "guest"))
	signInWith(t, n, v, "guest")
	wantFlat(t, v, "/", "flat /")
	before := flat.count()

	_, err := n.ServeSignIn(t.Context(), "app", flat.handler("other"), false, refused())
	ae := wantAuthError(t, err, false)
	if !strings.Contains(ae.StopErr.Error(), "relay refused unregister") {
		t.Fatalf("the stop error = %v", ae.StopErr)
	}
	e := n.entry("app")
	if e == nil {
		t.Fatal("the entry of an unconfirmed stop is gone")
	}
	if code := drained(e, v); code != http.StatusServiceUnavailable || flat.count() != before {
		t.Fatalf("a request after the refusal = %d, %d flat calls, want 503 and none", code, flat.count()-before)
	}
}

// A refusal clears the record of an earlier gate failure of the slug, so the status shows one reason.
func TestRequireAuthClearsAnEarlierGateFailure(t *testing.T) {
	n, _ := newTestNet(t, Config{Discovery: true})
	var flat flatRecorder
	if _, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, &core.PortalSignIn{Seq: 1, Err: errors.New("the setting cannot be read")}); err == nil {
		t.Fatal("the gate failure did not fail")
	}
	wantGateError(t, n, "cannot be read")
	_, err := n.ServeSignIn(t.Context(), "app", flat.handler("flat"), false, refused())
	wantAuthError(t, err, true)
	for _, h := range n.Status().Hosts {
		if h.Host == "app" || strings.Contains(h.Detail, "sign-in gate") {
			t.Fatalf("the status still holds the gate failure: %+v", h)
		}
	}
}
