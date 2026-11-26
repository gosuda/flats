package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/webhook"
)

// docs/spec/event-webhook.md: the changes that rules 1 and 21 leave out send no event and write no row of the three audit
// kinds. An event body holds no operator key (rule 5), its console link follows the request (rule 4), and a receiver that
// does not answer does not hold a visibility change (rule 15).

// grantRows lists the audit rows of the three kinds of rule 21, oldest first.
func grantRows(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, e := range auditOf(t, dir) {
		switch e.Kind {
		case store.AuditVisibilitySet, store.AuditNetworkSet, store.AuditProviderSet:
			out = append(out, e.Kind+" "+e.Flat+"/"+e.Name+" "+e.Via)
		}
	}
	return out
}

// The public instance mode is no visibility change and no grant (rules 1 and 21). Turning it on and off, a flat that it
// creates Public on the default public network, and a save that it publishes at once send no event and write none of
// the three audit kinds.
func TestPublicInstanceModeSendsNoEventAndWritesNoGrantRow(t *testing.T) {
	t.Parallel()
	s, dir, rec := webhookService(t)
	s.cfg.Lifecycle = newLifecycleRouteNet()
	ctx := t.Context()
	if _, err := s.SetPermittedNetworks([]string{"portal", "tailscale"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDefaultPublic: "portal"}); err != nil {
		t.Fatal(err)
	}
	if st, err := s.SetPublicInstance(ctx, true, "portal", "operator"); err != nil || !st.Enabled {
		t.Fatalf("turning the mode on = %+v, %v", st, err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Errorf("turning the mode on sent %v", shorts(got))
	}

	if _, err := s.CreateFlat(ctx, "site", "", ViaMCP); err != nil {
		t.Fatal(err)
	}
	f, err := s.st.GetFlat(ctx, "site")
	if err != nil || f.Visibility != store.Public || f.PublicAccess != store.AccessView {
		t.Fatalf("the flat of the mode is %s %q, %v; the test needs a flat that was created Public", f.Visibility, f.PublicAccess, err)
	}
	if ok, err := s.st.ProviderPermitted(ctx, "site", store.ProviderPortal); err != nil || !ok {
		t.Fatalf("portal is permitted = %t, %v; the test needs the grant that the creation writes", ok, err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Errorf("a creation in the mode sent %v", shorts(got))
	}

	// A save of such a flat is published at once, with no approval.
	lifecycleSave(t, s, "site", "one")
	if f, err := s.st.GetFlat(ctx, "site"); err != nil || f.LiveVersion != 1 || !f.Visibility.Public() {
		t.Fatalf("after the save the flat has live version %d and visibility %s, %v; the test needs the publication of the mode", f.LiveVersion, f.Visibility, err)
	}
	if approvals, err := s.st.ListApprovals(ctx, ""); err != nil || len(approvals) != 0 {
		t.Fatalf("the mode made %d approvals, %v; want none", len(approvals), err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Errorf("a save that the mode published sent %v", shorts(got))
	}

	if st, err := s.SetPublicInstance(ctx, false, "", "operator"); err != nil || st.Enabled {
		t.Fatalf("turning the mode off = %+v, %v", st, err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Errorf("turning the mode off sent %v", shorts(got))
	}
	if rows := grantRows(t, dir); len(rows) != 0 {
		t.Errorf("the mode wrote the audit rows %v, want none of the three kinds", rows)
	}
}

// When guest editing is no longer allowed, the host lowers the stored public access level of a board from edit to view
// with no approval. The owner decided that this is no visibility change (decision 1, rules 1 and 21): it sends no event
// and writes no visibility.set row. The approval that made the board editable before is announced and audited as usual.
//
// The test is serial, as the other tests of the board fixture are.
func TestTheGuestEditingDowngradeSendsNoEventAndWritesNoRow(t *testing.T) {
	ctx := context.Background()
	s, _ := newPublicBoard(t)
	dir := s.cfg.DataDir
	rec := &recorder{}
	s.SetNotifier(rec)
	setAccess(t, s, store.Public, store.AccessEdit)
	want := []string{"approval.created set_visibility pending", "visibility.changed set_visibility applied by operator", "approval.decided set_visibility approved by operator"}
	if got := shorts(rec.take()); !slices.Equal(got, want) {
		t.Fatalf("the approval of anyone-can-edit sent %v, want %v", got, want)
	}
	before := grantRows(t, dir)
	if n := len(before); n == 0 || !strings.HasPrefix(before[n-1], store.AuditVisibilitySet+" plan/") {
		t.Fatalf("audit rows after the approval = %v; the test needs the visibility.set row of the approval", before)
	}

	// As `flats config set` would leave it: an unlimited revision quota, which guest editing does not allow.
	zero := 0
	if err := s.st.ContentTx(ctx, "plan", func(tx *store.ContentTx) error { return tx.SetQuotas(ctx, &zero, nil) }); err != nil {
		t.Fatal(err)
	}
	if err := s.downgradeEditWhen(ctx, "by a config reload"); err != nil {
		t.Fatal(err)
	}
	f, err := s.st.GetFlat(ctx, "plan")
	if err != nil || f.Visibility != store.Public || f.PublicAccess != store.AccessView {
		t.Fatalf("after the downgrade the board is %s %q, %v; want public and view", f.Visibility, f.PublicAccess, err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Errorf("the downgrade sent %v", shorts(got))
	}
	if after := grantRows(t, dir); !slices.Equal(after, before) {
		t.Errorf("audit rows after the downgrade = %v, want the rows from before it %v", after, before)
	}
}

// The host-wide list of permitted networks is a setting of the host and no grant of a flat (decision 1, rules 1 and
// 21): a change of the list sends no event and writes none of the three audit kinds. The grants that a new flat gets
// from the defaults of the host are written by the creation, outside the setter, and write none either.
func TestTheHostWideNetworkListSendsNoEventAndWritesNoGrantRow(t *testing.T) {
	t.Parallel()
	s, dir, rec := webhookService(t)
	ctx := t.Context()
	// setList changes the list and checks that the host has it. The host keeps the list in an order of its own.
	setList := func(ids ...string) {
		t.Helper()
		if _, err := s.SetPermittedNetworks(ids, ""); err != nil {
			t.Fatal(err)
		}
		got := slices.Clone(s.PermittedNetworks())
		slices.Sort(got)
		if !slices.Equal(got, ids) {
			t.Fatalf("the list is %v after a set of %v", got, ids)
		}
	}
	setList("portal", "tailscale")
	setList("portal", "tailscale", "zrok")
	if _, err := s.UpdateSettings(ctx, map[string]string{SetDefaultPrivate: "tailscale", SetDefaultPublic: "portal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFlat(ctx, "app", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if got, err := s.st.PermittedProviders(ctx, "app"); err != nil || !slices.Equal(got, []string{"local", "portal", "tailscale"}) {
		t.Fatalf("the providers of the new flat = %v, %v; the test needs the two default grants", got, err)
	}
	// Turning a network off for the host leaves the grant of the flat as it is, so it is no change of that grant.
	setList("tailscale")
	if ok, err := s.st.ProviderPermitted(ctx, "app", store.ProviderPortal); err != nil || !ok {
		t.Errorf("the portal grant of the flat = %t, %v; want it kept", ok, err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Errorf("the changes of the host-wide list and the creation sent %v", shorts(got))
	}
	if rows := grantRows(t, dir); len(rows) != 0 {
		t.Errorf("the changes of the host-wide list and the creation wrote the audit rows %v, want none of the three kinds", rows)
	}
}

// legacyDraftFixture leaves in s what a Flats from before drafts left: a saved version of flat that was never published
// and a pending approval that names it by its number. migrateLegacyDrafts turns the version into a draft and
// AdoptLegacyDraft fails the approval.
func legacyDraftFixture(t *testing.T, s *Service, flat string, number int, approval string) {
	t.Helper()
	ctx := context.Background()
	v := store.Version{Flat: flat, Number: number, Hash: "saved-only-" + approval, Kind: "static", Manifest: json.RawMessage(`{"kind":"static","entry":"index.html"}`), CreatedAt: time.Now()}
	if err := s.st.InsertVersion(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(s.versionDir(flat, number), "index.html"), "saved-only"); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]int{"version": number})
	if err := s.st.InsertApproval(ctx, store.Approval{ID: approval, Flat: flat, Action: "activate", Params: params, Status: "pending", Via: "api", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

// AdoptLegacyDraft sets a pending approval of a legacy version to failed. Rule 1 leaves that status out: the start runs
// it inside New, where the service has no notifier yet (rule 17), and the adoption itself hands no event over even
// when a notifier is in place.
func TestALegacyDraftAdoptionSendsNoEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: t.Logf}
	first, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleSave(t, first, "legacy", "published")
	lifecycleApprove(t, first, lifecycleRequest(t, first, "legacy"))
	legacyDraftFixture(t, first, "legacy", 2, "legacy-at-start")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	rec := &recorder{}
	second.SetNotifier(rec)
	failed := func(id string) {
		t.Helper()
		a, err := second.GetApproval(ctx, id)
		if err != nil || a.Status != "failed" || !strings.Contains(a.Result, "legacy version migrated to draft") {
			t.Fatalf("the approval %s = %s (%s), %v; want the failure that AdoptLegacyDraft writes", id, a.Status, a.Result, err)
		}
	}
	failed("legacy-at-start")
	if got := rec.take(); len(got) != 0 {
		t.Errorf("the adoption at the start sent %v", shorts(got))
	}

	legacyDraftFixture(t, second, "legacy", 3, "legacy-with-notifier")
	if err := second.migrateLegacyDrafts(ctx); err != nil {
		t.Fatal(err)
	}
	failed("legacy-with-notifier")
	if got := rec.take(); len(got) != 0 {
		t.Errorf("the adoption with a notifier in place sent %v", shorts(got))
	}
	if rows := grantRows(t, dir); len(rows) != 0 {
		t.Errorf("the adoption wrote the audit rows %v, want none of the three kinds", rows)
	}
}

// No event body holds the value of an operator key: not the key of a provider and not the two webhook keys themselves
// (rule 5, "no key"). The three audit kinds hold none either.
func TestNoEventBodyHoldsAnOperatorKeyValue(t *testing.T) {
	t.Parallel()
	s, dir, rec := webhookService(t)
	s.cfg.Lifecycle = newLifecycleRouteNet()
	ctx := t.Context()
	keyFile := NewKeyFile(dir, "")
	// Each value has a part that no JSON encoder rewrites, so a search of a body finds it in any field.
	keys := []struct{ id, value, canary string }{
		{HostKeyZrok, "CANARY-zrok-token-41d7", "CANARY-zrok-token-41d7"},
		{HostKeyWebhookURL, "https://hooks.example.com/in?token=CANARY-url-token-5c1e", "CANARY-url-token-5c1e"},
		{HostKeyWebhookSecret, "whsec_CANARY-secret-77aa0123456789", "CANARY-secret-77aa0123456789"},
	}
	for _, k := range keys {
		if err := PutHostKeyValue(ctx, keyFile, s.st, k.id, k.value, KeySourceCLI, time.Now(), store.Audit{Via: store.AuditViaCommand}); err != nil {
			t.Fatalf("store the %s key: %v", k.id, err)
		}
		if got, _, err := StoredHostKey(ctx, keyFile, s.st, k.id); err != nil || got != k.value {
			t.Fatalf("the stored %s key opens to %q, %v; the test needs the value in the store", k.id, got, err)
		}
	}

	makeLive(t, s, rec, "notes")
	if err := s.SetProviderPermission(ctx, "notes", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNetworkPolicy(ctx, "notes", []string{"https://api.example.com"}, ViaConsole); err != nil {
		t.Fatal(err)
	}
	req, err := s.SetVisibility(ctx, "notes", store.Public, ViaAPI, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("SetVisibility = %+v, %v", req, err)
	}
	if _, err := s.Decide(ctx, req.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	got := rec.take()
	seen := map[string]bool{}
	for _, p := range got {
		seen[p.Type] = true
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if strings.Contains(string(raw), k.canary) {
				t.Errorf("the body of %s holds the value of the %s key: %s", p.Type, k.id, raw)
			}
		}
	}
	for _, typ := range []string{eventApprovalCreated, eventApprovalDecided, eventVisibilityChange, eventNetworkChange} {
		if !seen[typ] {
			t.Errorf("no %s event among %v; the test needs one of each type", typ, shorts(got))
		}
	}
	rows := 0
	for _, e := range auditOf(t, dir) {
		if e.Kind != store.AuditVisibilitySet && e.Kind != store.AuditNetworkSet && e.Kind != store.AuditProviderSet {
			continue
		}
		rows++
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if strings.Contains(string(raw), k.canary) {
				t.Errorf("the %s row holds the value of the %s key: %s", e.Kind, k.id, raw)
			}
		}
	}
	if rows != 3 {
		t.Errorf("%d audit rows of the three kinds, want one of each", rows)
	}
}

// The console link of an event has the console address of the request that caused the event, and the address of the
// host when there is no request (rule 4, console_url). The host tells the two apart by the context of the operation.
func TestTheConsoleLinkOfAnEventFollowsTheRequest(t *testing.T) {
	t.Parallel()
	s, _, rec := webhookService(t)
	s.cfg.Lifecycle = newLifecycleRouteNet()
	type throughDomain struct{}
	const hostBase, requestBase = "http://console.test:9", "https://via.example.net"
	s.cfg.ConsoleURLFor = func(ctx context.Context) string {
		if ctx.Value(throughDomain{}) != nil {
			return requestBase + "/"
		}
		return s.cfg.ConsoleURL()
	}
	plain := t.Context()
	request := context.WithValue(plain, throughDomain{}, true)
	makeLive(t, s, rec, "notes")
	if err := s.SetProviderPermission(plain, "notes", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	// one returns the only event that step made.
	one := func(step string) webhook.Payload {
		t.Helper()
		got := rec.take()
		if len(got) != 1 {
			t.Fatalf("%s sent %v, want one event", step, shorts(got))
		}
		return got[0]
	}
	if p := one("a grant without a request"); p.ConsoleURL != hostBase+"/flats/notes" {
		t.Errorf("a grant without a request has the link %q, want the address of the host", p.ConsoleURL)
	}
	if err := s.SetNetworkPolicy(request, "notes", []string{"https://api.example.com"}, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if p := one("a grant of a request"); p.ConsoleURL != requestBase+"/flats/notes" {
		t.Errorf("a grant of a request has the link %q, want the address of the request", p.ConsoleURL)
	}

	// An approval that a request makes and that is decided through another address: each event has the address of
	// the operation that made it.
	req, err := s.SetVisibility(request, "notes", store.Public, ViaAPI, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("SetVisibility = %+v, %v", req, err)
	}
	if p := one("the request of an approval"); p.Type != eventApprovalCreated || p.ConsoleURL != requestBase+"/approvals/"+req.Approval.ID {
		t.Errorf("approval.created has the link %q, want the approval link of the address of the request", p.ConsoleURL)
	}
	if _, err := s.Decide(plain, req.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	got := rec.take()
	if want := []string{"visibility.changed set_visibility applied by operator", "approval.decided set_visibility approved by operator"}; !slices.Equal(shorts(got), want) {
		t.Fatalf("the decision sent %v, want %v", shorts(got), want)
	}
	if got[0].ConsoleURL != hostBase+"/flats/notes" || got[1].ConsoleURL != hostBase+"/approvals/"+req.Approval.ID {
		t.Errorf("a decision without a request has the links %q and %q, want the address of the host", got[0].ConsoleURL, got[1].ConsoleURL)
	}

	back, err := s.SetVisibility(plain, "notes", store.Private, ViaAPI, "")
	if err != nil || back.Approval == nil {
		t.Fatalf("SetVisibility = %+v, %v", back, err)
	}
	rec.take()
	if _, err := s.Decide(request, back.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	got = rec.take()
	if len(got) != 2 || got[0].Type != eventVisibilityChange || got[1].Type != eventApprovalDecided {
		t.Fatalf("the second decision sent %v", shorts(got))
	}
	if got[0].ConsoleURL != requestBase+"/flats/notes" || got[1].ConsoleURL != requestBase+"/approvals/"+back.Approval.ID {
		t.Errorf("a decision of a request has the links %q and %q, want the address of the request", got[0].ConsoleURL, got[1].ConsoleURL)
	}
}

// A receiver that does not answer does not hold a visibility change (rule 15). With the real sender and a receiver that
// blocks every request, the decision returns approved and the flat is Public while the receiver still holds the first
// delivery.
func TestAVisibilityChangeDoesNotWaitForABlockedWebhookReceiver(t *testing.T) {
	t.Parallel()
	s, _, rec := webhookService(t)
	s.cfg.Lifecycle = newLifecycleRouteNet()
	makeLive(t, s, rec, "notes")
	if err := s.SetProviderPermission(t.Context(), "notes", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}

	entered := make(chan string, 16)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.Header.Get("X-Flats-Event")
		<-release
	}))
	// One attempt with a limit that the test never reaches: a delivery ends only when the receiver is released, and
	// the receiver sees each event once.
	sender := webhook.New(webhook.Config{
		Keys: func(context.Context) (webhook.Credentials, error) {
			return webhook.Credentials{URL: srv.URL + "/hook", Secret: "whsec_0123456789abcdef"}, nil
		},
		Client:       srv.Client(),
		Waits:        []time.Duration{},
		AttemptLimit: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		sender.Run(ctx)
	}()
	// The order cannot hang: the handlers end first, so no delivery is held, then the sender stops, and the server
	// closes last, because its Close waits for the handlers.
	t.Cleanup(func() {
		close(release)
		cancel()
		<-stopped
		srv.Close()
	})
	s.SetNotifier(sender)

	// arrived waits for the next delivery that the receiver holds.
	arrived := func() string {
		t.Helper()
		select {
		case typ := <-entered:
			return typ
		case <-time.After(30 * time.Second):
			t.Fatal("no delivery reached the receiver")
			return ""
		}
	}
	// returns runs an operation of the service and fails the test when it does not come back. No delivery ends before
	// the cleanup, so an operation that waits for one would never return.
	returns := func(step string, op func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			op()
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s waits for a delivery that the receiver holds", step)
		}
	}
	var req ActionResult
	var a store.Approval
	var err error
	returns("the request of the visibility change", func() {
		req, err = s.SetVisibility(context.Background(), "notes", store.Public, ViaAPI, "")
	})
	if err != nil || req.Approval == nil {
		t.Fatalf("SetVisibility with a blocked receiver = %+v, %v", req, err)
	}
	if typ := arrived(); typ != eventApprovalCreated {
		t.Fatalf("the receiver holds %q first, want approval.created", typ)
	}
	returns("the decision of the visibility change", func() {
		a, err = s.Decide(context.Background(), req.Approval.ID, true)
	})
	if err != nil || a.Status != "approved" {
		t.Fatalf("Decide with a blocked receiver = %s (%s), %v; want approved", a.Status, a.Result, err)
	}
	if f, err := s.st.GetFlat(t.Context(), "notes"); err != nil || f.Visibility.Canonical() != store.Public {
		t.Errorf("the flat is %s, %v; want public while the receiver holds its request", f.Visibility, err)
	}
	// The two events of the decision were handed over: they reach the receiver while it still holds the first one.
	// Only the cleanup releases it.
	types := []string{arrived(), arrived()}
	slices.Sort(types)
	if want := []string{eventApprovalDecided, eventVisibilityChange}; !slices.Equal(types, want) {
		t.Errorf("the receiver got %v after the decision, want %v", types, want)
	}
}
