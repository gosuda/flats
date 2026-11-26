package core_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/portal-require-auth.md, rule 9: a host without a provider manager, which is a test seam, refuses the open of a
// flat without sign-in. The visit of a change needs a provider manager, so a route that is already served keeps serving
// until the next restart.
func TestRequireAuthOnTheLegacyPath(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for _, slug := range []string{"fresh", "served"} {
		if _, err := e.svc.SaveVersion(ctx, slug, files("index.html", "hi"), core.SaveMeta{}, core.ViaMCP); err != nil {
			t.Fatal(err)
		}
		publish(t, e.svc, slug)
		permitPortal(t, e.svc, slug)
	}
	makePublic(t, e, "served")
	if _, served := e.pub.Hidden("served"); !served {
		t.Fatal("the test did not serve a flat on Portal")
	}

	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetRequireAuth: "true"}); err != nil {
		t.Fatal(err)
	}
	// The visit needs a provider manager: a served flat keeps its route, which is the stated limit of rule 9.
	if _, served := e.pub.Hidden("served"); !served {
		t.Fatal("the legacy path stopped a served route; the spec says it keeps serving until the next restart")
	}

	// A flat that is not served is not served now: the approval of its visibility fails with the text of rule 10, and
	// the backend never serves it.
	r, err := e.svc.SetVisibility(ctx, "fresh", store.Public, core.ViaConsole, "")
	if err != nil || r.Approval == nil {
		t.Fatalf("request = %+v, %v", r, err)
	}
	if _, err := e.svc.Decide(ctx, r.Approval.ID, true); err == nil || !strings.Contains(err.Error(), "portal.require_auth is on and fresh has no sign-in") {
		t.Fatalf("the approval of fresh = %v, want the refusal", err)
	}
	if _, served := e.pub.Hidden("fresh"); served {
		t.Fatal("a refused open served the flat on the legacy backend")
	}
	if f, _ := e.svc.GetFlat(ctx, "fresh"); f.Visibility != store.Private {
		t.Fatalf("fresh is %s, want Private", f.Visibility)
	}

	// With the setting off the same request applies.
	if _, err := e.svc.UpdateSettings(ctx, map[string]string{core.SetRequireAuth: "false"}); err != nil {
		t.Fatal(err)
	}
	makePublic(t, e, "fresh")
	if _, served := e.pub.Hidden("fresh"); !served {
		t.Fatal("the flat is not served with the setting off")
	}
}
