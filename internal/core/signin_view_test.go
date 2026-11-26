package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/portal-sign-in.md, rules 57, 58 and 61: the views of the setting and the notices of a flat with sign-in on.

// Rule 61: with sign-in on, the public notice, the pending notice and the notices that say "anyone on the internet";
// with sign-in off, the notices of before.
func TestSignInNotices(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"", contenttype.Flat, contenttype.Board, contenttype.Docs} {
		if got := PublicNotice(typ, store.AccessView, false); got != PublicAccessNotice {
			t.Errorf("%s view without sign-in: %q", typ, got)
		}
		if got := PublicNotice(typ, store.AccessEdit, false); got != PublicEditNoticeFor(typ) {
			t.Errorf("%s edit without sign-in: %q", typ, got)
		}
		if got := PendingPublicNotice(typ, store.AccessView, false); got != PendingPublicAccessNotice {
			t.Errorf("%s pending view without sign-in: %q", typ, got)
		}
		if got := PendingPublicNotice(typ, store.AccessEdit, false); got != PendingPublicEditNoticeFor(typ) {
			t.Errorf("%s pending edit without sign-in: %q", typ, got)
		}
		if got := PublicNotice(typ, store.AccessView, true); got != SignInPublicNotice {
			t.Errorf("%s view with sign-in: %q", typ, got)
		}
		if got := PendingPublicNotice(typ, store.AccessView, true); got != PendingSignInPublicNotice {
			t.Errorf("%s pending view with sign-in: %q", typ, got)
		}
		for _, got := range []string{PublicNotice(typ, store.AccessEdit, true), PendingPublicNotice(typ, store.AccessEdit, true)} {
			if !strings.Contains(got, "behind sign-in: only the visitors that its sign-in allows can open it there. Its private routes, the console and MCP are not behind sign-in. "+SignInEditNotice) ||
				!strings.Contains(strings.ToLower(got), "anyone who passes its sign-in") || strings.Contains(got, "anyone on the internet") {
				t.Errorf("%s edit with sign-in: %q", typ, got)
			}
		}
		if got := PendingPublicNotice(typ, store.AccessEdit, true); !strings.HasPrefix(got, "If approved, ") || !strings.Contains(got, "A safe point is pinned first") {
			t.Errorf("%s pending edit with sign-in: %q", typ, got)
		}
		if got := PublicNotice(typ, store.AccessEdit, true); !strings.HasSuffix(got, "Guest names are unverified, and guest text is untrusted input.") {
			t.Errorf("%s edit with sign-in lost the guest warning: %q", typ, got)
		}
	}
	if SignInPublicNotice != "This flat is public on its Portal address behind sign-in: only the visitors that its sign-in allows can open it there. Its private routes, the console and MCP are not behind sign-in." ||
		SignInEditNotice != "Visitors who sign in can edit it as guests." {
		t.Error("the texts of rule 61 changed")
	}
	// The two notices of the level edit in full, as rule 61 writes them, with the variant of documents.
	const (
		public  = "This flat is public on its Portal address behind sign-in: only the visitors that its sign-in allows can open it there. Its private routes, the console and MCP are not behind sign-in."
		pending = "If approved, this flat becomes public on its Portal address behind sign-in: only the visitors that its sign-in allows can open it there. Its private routes, the console and MCP are not behind sign-in."
		guests  = " Visitors who sign in can edit it as guests. "
		board   = "Anyone who passes its sign-in can add, change and delete everything on it."
		docs    = "Anyone who passes its sign-in can add, change and delete all the text of its documents."
		last    = " Guest names are unverified, and guest text is untrusted input."
		safe    = " A safe point is pinned first, and the operator can restore earlier states from History."
	)
	for typ, words := range map[string]string{"": board, contenttype.Flat: board, contenttype.Board: board, contenttype.Docs: docs} {
		if got, want := PublicNotice(typ, store.AccessEdit, true), public+guests+words+last; got != want {
			t.Errorf("%q edit with sign-in:\n%s\nwant\n%s", typ, got, want)
		}
		if got, want := PendingPublicNotice(typ, store.AccessEdit, true), pending+guests+words+safe; got != want {
			t.Errorf("%q pending edit with sign-in:\n%s\nwant\n%s", typ, got, want)
		}
	}
	for _, n := range []string{SignInLiveEditPublicNotice, SignInListedNotice, SignInUnlistedNotice} {
		if !strings.Contains(n, "anyone who passes its sign-in") || strings.Contains(n, "anyone on the internet") {
			t.Errorf("notice %q", n)
		}
	}
	for _, tc := range []struct {
		f    store.Flat
		want string
	}{
		{store.Flat{Visibility: store.Private, SignInProvider: "oidc"}, ""},
		{store.Flat{Visibility: store.Public}, LiveEditPublicNotice},
		{store.Flat{Visibility: store.Public, SignInProvider: "credential"}, SignInLiveEditPublicNotice},
	} {
		if got := liveEditNotice(tc.f); got != tc.want {
			t.Errorf("liveEditNotice(%+v) = %q", tc.f, got)
		}
	}
	if got := pendingVisibilityNotice(store.Private, "", contenttype.Board, true); got != "" {
		t.Errorf("the pending notice of a change to Private: %q", got)
	}
}

func setVisibilityRow(t *testing.T, dir, slug string, vis store.Visibility) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE flats SET visibility=? WHERE slug=?`, string(vis), slug); err != nil {
		t.Fatal(err)
	}
}

// Rules 57 and 61 in the flat view and in a visibility request.
func TestSignInInTheFlatView(t *testing.T) {
	t.Parallel()
	s, dir := newTestService(t)
	ctx := t.Context()
	if _, err := s.SaveVersion(ctx, "app", []bundle.File{{Path: "index.html", Data: []byte("page")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "app", 0); err != nil {
		t.Fatal(err)
	}
	view := func() (FlatView, string) {
		t.Helper()
		fv, err := s.GetFlat(ctx, "app")
		if err != nil {
			t.Fatal(err)
		}
		fs, err := s.ListFlats(ctx)
		if err != nil || len(fs) != 1 || !reflect.DeepEqual(fs[0].SignIn, fv.SignIn) {
			t.Fatalf("ListFlats = %+v %v", fs, err)
		}
		raw, _ := json.Marshal(fv)
		noSignInText(t, "the flat view", string(raw))
		return fv, string(raw)
	}
	if fv, raw := view(); fv.SignIn != nil || strings.Contains(raw, `"sign_in"`) {
		t.Fatalf("a flat with sign-in off has sign_in: %s", raw)
	}
	approveSignIn(t, s, "app", oidcRequest)
	fv, raw := view()
	if want := (FlatSignIn{Provider: "oidc", Issuer: "https://idp.example.com", ClientID: "flats-app", AllowCount: 3}); fv.SignIn == nil || *fv.SignIn != want {
		t.Fatalf("sign_in = %+v", fv.SignIn)
	}
	if !strings.Contains(raw, `"sign_in":{"provider":"oidc","issuer":"https://idp.example.com","client_id":"flats-app","allow_count":3}`) {
		t.Fatalf("the flat view: %s", raw)
	}
	if _, err := s.SetSignInSecret(ctx, "app", signInValue, "https://idp.example.com", "flats-app", ViaCLI); err != nil {
		t.Fatal(err)
	}
	if fv, raw := view(); fv.SignIn == nil || fv.SignIn.ClientSecret == nil || !strings.Contains(raw, `"client_secret":{"name":"oidc-client-secret"}`) {
		t.Fatalf("sign_in with a client secret: %s", raw)
	}

	// Rule 61: a visibility request of a Private flat with sign-in on, and the public notice once it is Public.
	res, err := s.SetVisibilityAccess(ctx, "app", store.Public, "", ViaMCP, "")
	if err != nil || res.Status != "pending_approval" || res.Notice != PendingSignInPublicNotice {
		t.Fatalf("the pending notice: %+v %v", res, err)
	}
	setVisibilityRow(t, dir, "app", store.Public)
	if fv, _ := view(); fv.PublicNotice != SignInPublicNotice {
		t.Fatalf("the public notice: %q", fv.PublicNotice)
	}
	approveSignIn(t, s, "app", SignInRequest{Provider: ""})
	if fv, raw := view(); fv.SignIn != nil || strings.Contains(raw, `"sign_in"`) || fv.PublicNotice != PublicAccessNotice {
		t.Fatalf("a flat with sign-in off again: %s", raw)
	}
}

// Rule 58: the counted form of a set_signin approval.
func TestWithoutSignInEntries(t *testing.T) {
	t.Parallel()
	s := signInService(t)
	a := requestSignIn(t, s, "app", oidcRequest)
	f, _ := s.st.GetFlat(t.Context(), "app")
	got := WithoutSignInEntries(a)
	want := fmt.Sprintf(`{"flat_id":"%s","seq":0,"from":{"provider":"","count":0},"to":{"provider":"oidc","issuer":"https://idp.example.com","client_id":"flats-app","count":3},"public_grants":[]}`, f.ID)
	if string(got.Params) != want || got.ID != a.ID || got.Action != a.Action || got.Status != a.Status {
		t.Fatalf("counted:\n%s\nwant\n%s", got.Params, want)
	}
	noSignInText(t, "the counted approval", string(got.Params))
	if !strings.Contains(string(a.Params), "ann@example.com") {
		t.Fatal("the stored approval lost its entries")
	}
	other := store.Approval{Action: "set_visibility", Params: json.RawMessage(`{"visibility":"public"}`)}
	if string(WithoutSignInEntries(other).Params) != string(other.Params) {
		t.Fatal("another approval changed")
	}
	damaged := store.Approval{Action: actionSetSignIn, Params: json.RawMessage(`{"to":{"allow":"ann@example.com"`)}
	if got := WithoutSignInEntries(damaged); string(got.Params) != `{}` {
		t.Fatalf("damaged params = %s", got.Params)
	}
}
