package core

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/webhook"
)

// docs/spec/event-webhook.md: the events that the service hands to its notifier (rules 1 to 5) and the audit rows of the
// visibility and grant changes (rule 21).

// recorder is a Notifier that keeps what it is given.
type recorder struct {
	mu  sync.Mutex
	got []webhook.Payload
}

func (r *recorder) Notify(p webhook.Payload) {
	r.mu.Lock()
	r.got = append(r.got, p)
	r.mu.Unlock()
}

func (r *recorder) take() []webhook.Payload {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.got
	r.got = nil
	return out
}

// short renders an event for a comparison: type, operation and state, and the grant or decider where there is one.
func short(p webhook.Payload) string {
	s := p.Type + " " + p.Operation + " " + p.State
	if p.DecidedBy != "" {
		s += " by " + p.DecidedBy
	}
	return s
}

func shorts(ps []webhook.Payload) []string {
	var out []string
	for _, p := range ps {
		out = append(out, short(p))
	}
	return out
}

// webhookService is a service with a recorder and a console address.
func webhookService(t *testing.T) (*Service, string, *recorder) {
	t.Helper()
	s, dir := newTestService(t)
	rec := &recorder{}
	s.SetNotifier(rec)
	s.cfg.ConsoleURL = func() string { return "http://console.test:9/" }
	return s, dir, rec
}

// makeLive saves, requests and approves the first version of a flat, so that it can be made public. It discards the
// events that this makes.
func makeLive(t *testing.T, s *Service, rec *recorder, slug string) store.Flat {
	t.Helper()
	lifecycleSave(t, s, slug, "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, slug))
	rec.take()
	f, err := s.st.GetFlat(t.Context(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// An approval sends approval.created when it is made, and a request that repeats an open one sends nothing. The operator's
// decision sends approval.decided, whether it approves or rejects (rule 1).
func TestApprovalEventsAreSentForCreationAndDecision(t *testing.T) {
	t.Parallel()
	s, _, rec := webhookService(t)
	lifecycleSave(t, s, "notes", "one")
	flat, err := s.st.GetFlat(t.Context(), "notes")
	if err != nil {
		t.Fatal(err)
	}
	p := lifecycleRequest(t, s, "notes")
	got := rec.take()
	if len(got) != 1 {
		t.Fatalf("a new approval sent %v", shorts(got))
	}
	e := got[0]
	wantURL := "http://console.test:9/approvals/" + p.Approval.ID
	if e.Type != "approval.created" || e.Operation != "create_version" || e.State != "pending" || e.ApprovalID != p.Approval.ID || e.Via != "console" ||
		e.DecidedBy != "" || e.Flat.Slug != "notes" || e.Flat.ID != flat.ID || flat.ID == "" || e.ConsoleURL != wantURL || e.Change != nil {
		t.Errorf("created event = %+v, want console_url %s and flat id %s", e, wantURL, flat.ID)
	}
	if !strings.HasSuffix(e.At, "Z") || len(e.At) != len("2006-01-02T15:04:05.000Z") {
		t.Errorf("at = %q, want RFC 3339 in UTC with milliseconds", e.At)
	}

	lifecycleRequest(t, s, "notes") // the same request again
	if got := rec.take(); len(got) != 0 {
		t.Errorf("a repeated open request sent %v", shorts(got))
	}

	if _, err := s.Decide(t.Context(), p.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	got = rec.take()
	if !slices.Equal(shorts(got), []string{"approval.decided create_version approved by operator"}) {
		t.Fatalf("an approval sent %v", shorts(got))
	}
	if got[0].Via != "" || got[0].ApprovalID != p.Approval.ID || got[0].ConsoleURL != wantURL {
		t.Errorf("decided event = %+v", got[0])
	}

	lifecycleSave(t, s, "notes", "two")
	rejected := lifecycleRequest(t, s, "notes")
	rec.take()
	if _, err := s.Decide(t.Context(), rejected.Approval.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := shorts(rec.take()); !slices.Equal(got, []string{"approval.decided create_version rejected by operator"}) {
		t.Errorf("a rejection sent %v", got)
	}
}

// An approved visibility change sends visibility.changed and then approval.decided, and writes a visibility.set row with the
// console identity (rules 1, 4 and 21).
func TestVisibilityChangeSendsItsEventAndWritesItsAuditRow(t *testing.T) {
	t.Parallel()
	s, dir, rec := webhookService(t)
	s.cfg.Lifecycle = newLifecycleRouteNet()
	f := makeLive(t, s, rec, "notes")
	if err := s.SetProviderPermission(t.Context(), "notes", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 1 || got[0].Type != "network.changed" || got[0].Operation != "set_provider_permission" || got[0].State != "applied" ||
		got[0].Change == nil || got[0].Change.Grant != "provider" || got[0].Change.Provider != store.ProviderPortal || !got[0].Change.Permitted ||
		got[0].Flat.ID != f.ID || got[0].ConsoleURL != "http://console.test:9/flats/notes" || got[0].ApprovalID != "" || got[0].DecidedBy != "" {
		t.Fatalf("a provider grant sent %+v", got)
	}
	req, err := s.SetVisibility(t.Context(), "notes", store.Public, ViaAPI, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("SetVisibility = %+v, %v", req, err)
	}
	ctx := WithAuditActor(t.Context(), `tailnet user "ann@example.org"`)
	if _, err := s.Decide(ctx, req.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	got := rec.take()
	if want := []string{"approval.created set_visibility pending", "visibility.changed set_visibility applied by operator", "approval.decided set_visibility approved by operator"}; !slices.Equal(shorts(got), want) {
		t.Fatalf("events = %v, want %v", shorts(got), want)
	}
	v := got[1]
	if v.ApprovalID != req.Approval.ID || v.Flat.ID != f.ID || v.ConsoleURL != "http://console.test:9/flats/notes" || v.Change == nil || v.Change.From == nil || v.Change.To == nil ||
		*v.Change.From != (webhook.Access{Visibility: "private", Access: ""}) || *v.Change.To != (webhook.Access{Visibility: "public", Access: "view"}) {
		t.Errorf("visibility event = %+v", v)
	}
	evs := auditOf(t, dir)
	var rows []store.AuditEvent
	for _, e := range evs {
		if e.Kind == store.AuditVisibilitySet {
			rows = append(rows, e)
		}
	}
	if len(rows) != 1 || rows[0].Via != "console" || rows[0].Actor != `tailnet user "ann@example.org"` || rows[0].Flat != "notes" || rows[0].FlatID != f.ID {
		t.Fatalf("visibility.set rows = %+v", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil || data["approval"] != req.Approval.ID {
		t.Errorf("row data = %s, %v", rows[0].Data, err)
	}
}

// An agent's decision under Autopilot is announced as autopilot and audited with the Autopilot text (rule 21).
func TestAutopilotDecisionIsAnnouncedAsAutopilot(t *testing.T) {
	t.Parallel()
	s, dir, rec := webhookService(t)
	s.cfg.Lifecycle = newLifecycleRouteNet()
	makeLive(t, s, rec, "notes")
	if err := s.SetProviderPermission(t.Context(), "notes", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	allowAutopilot(t, s, AutopilotOps...)
	rec.take()
	req, err := s.SetVisibility(t.Context(), "notes", store.Public, ViaMCP, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("SetVisibility = %+v, %v", req, err)
	}
	// The stored record of the decision holds the name, the address and the login of the agent. No body may hold them.
	agent := AgentDecision{Agent: "agent-CANARY", Arrival: Arrival{Network: "local", IP: "100.99.98.97", Login: "canary-login@example.org"}}
	if _, err := s.DecideAsAgent(t.Context(), req.Approval.ID, true, agent); err != nil {
		t.Fatal(err)
	}
	if a, err := s.st.GetApproval(t.Context(), req.Approval.ID); err != nil || !strings.Contains(a.DecidedBy, "100.99.98.97") || !strings.Contains(a.DecidedBy, "canary-login") {
		t.Fatalf("the stored record of the decision is %q, %v; the test needs the address and the login in it", a.DecidedBy, err)
	}
	got := rec.take()
	if want := []string{"approval.created set_visibility pending", "visibility.changed set_visibility applied by autopilot", "approval.decided set_visibility approved by autopilot"}; !slices.Equal(shorts(got), want) {
		t.Fatalf("events = %v, want %v", shorts(got), want)
	}
	if got[0].Via != "mcp" {
		t.Errorf("via = %q, want mcp", got[0].Via)
	}
	var rows []store.AuditEvent
	for _, e := range auditOf(t, dir) {
		if e.Kind == store.AuditVisibilitySet {
			rows = append(rows, e)
		}
	}
	if len(rows) != 1 || rows[0].Via != store.AuditViaAutopilot || rows[0].Actor != DeciderText(EncodeAgentDecider(agent)) {
		t.Fatalf("rows = %+v, want one with via autopilot and the Autopilot text %q", rows, DeciderText(EncodeAgentDecider(agent)))
	}
	for _, p := range got {
		raw, _ := json.Marshal(p)
		if strings.Contains(string(raw), "CANARY") || strings.Contains(string(raw), "100.99.98.97") || strings.Contains(string(raw), "canary-login") || strings.Contains(string(raw), "Local") {
			t.Errorf("the body names the agent, its address or its login: %s", raw)
		}
	}
}

// The origin grants of a flat send network.changed when they change, and the refused surfaces and no-ops send nothing
// (rules 1 and 2), and each change writes a network.set row (rule 21).
func TestOriginGrantChangeSendsItsEvent(t *testing.T) {
	t.Parallel()
	s, dir, rec := webhookService(t)
	f, err := s.CreateFlat(t.Context(), "notes", "", ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.st.GetFlat(t.Context(), "notes")
	if err != nil || f.Slug != "notes" {
		t.Fatal(err)
	}
	origins := []string{"https://api.example.com"}
	if err := s.SetNetworkPolicy(WithAuditActor(t.Context(), "console actor"), "notes", origins, ViaConsole); err != nil {
		t.Fatal(err)
	}
	got := rec.take()
	if len(got) != 1 || got[0].Type != "network.changed" || got[0].Operation != "set_network_origins" || got[0].State != "applied" || got[0].Flat.ID != stored.ID ||
		got[0].Change == nil || got[0].Change.Grant != "origins" || got[0].Change.Count != 1 || got[0].Change.Truncated || !slices.Equal(got[0].Change.Origins, origins) {
		t.Fatalf("origin grant sent %+v", got)
	}
	if err := s.SetNetworkPolicy(t.Context(), "notes", origins, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNetworkPolicy(t.Context(), "notes", []string{"https://other.example.com"}, ViaMCP); err == nil {
		t.Fatal("an agent set the origin grants")
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("a no-op and a refusal sent %v", shorts(got))
	}
	if err := s.SetNetworkPolicy(t.Context(), "notes", nil, ViaCLI); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 1 || got[0].Change == nil || len(got[0].Change.Origins) != 0 {
		t.Fatalf("clearing the list sent %+v", got)
	}
	var kinds, vias []string
	for _, e := range auditOf(t, dir) {
		kinds = append(kinds, e.Kind)
		vias = append(vias, e.Via)
	}
	if !slices.Equal(kinds, []string{store.AuditNetworkSet, store.AuditNetworkSet}) || !slices.Equal(vias, []string{"console", "cli"}) {
		t.Errorf("audit rows: kinds %v vias %v", kinds, vias)
	}
	// A provider permission to the value it has, and a revocation of one that was never granted, send nothing.
	if err := s.SetProviderPermission(t.Context(), "notes", store.ProviderPortal, false, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Errorf("a revocation of a provider that was never granted sent %v", shorts(got))
	}
}

// The delete that an agent requested, once approved, sends approval.decided with the flat that is gone, and the other
// pending approvals of that flat that the deletion fails each send approval.decided with state failed and no decider
// (rule 1).
func TestDeletingAFlatSendsTheDecisionsItCauses(t *testing.T) {
	t.Parallel()
	s, _, rec := webhookService(t)
	s.cfg.Lifecycle = newLifecycleRouteNet()
	f := makeLive(t, s, rec, "doomed")
	if err := s.SetProviderPermission(t.Context(), "doomed", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	lifecycleSave(t, s, "doomed", "two")
	pendingPublish := lifecycleRequest(t, s, "doomed")
	pendingVisibility, err := s.SetVisibility(t.Context(), "doomed", store.Public, ViaAPI, "")
	if err != nil || pendingVisibility.Approval == nil {
		t.Fatalf("SetVisibility = %+v, %v", pendingVisibility, err)
	}
	req, err := s.Delete(t.Context(), "doomed", ViaMCP, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("Delete = %+v, %v", req, err)
	}
	rec.take()
	if _, err := s.Decide(t.Context(), req.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	got := rec.take()
	if len(got) != 3 {
		t.Fatalf("events = %v, want a failed decision for each of the two pending approvals and the approved delete", shorts(got))
	}
	failed := []string{short(got[0]) + " " + got[0].ApprovalID, short(got[1]) + " " + got[1].ApprovalID}
	slices.Sort(failed)
	wantFailed := []string{"approval.decided create_version failed " + pendingPublish.Approval.ID, "approval.decided set_visibility failed " + pendingVisibility.Approval.ID}
	slices.Sort(wantFailed)
	if !slices.Equal(failed, wantFailed) || got[0].DecidedBy != "" || got[1].DecidedBy != "" {
		t.Errorf("failed events = %v, want %v with no decider", failed, wantFailed)
	}
	if short(got[2]) != "approval.decided delete approved by operator" || got[2].Flat.Slug != "doomed" || got[2].Flat.ID != f.ID || got[2].ApprovalID != req.Approval.ID {
		t.Errorf("delete event = %+v, want the slug and the ID %s of the flat that is gone", got[2], f.ID)
	}
	if _, err := s.st.GetFlat(t.Context(), "doomed"); err == nil {
		t.Error("the flat is still there")
	}
}

// No body of any type holds the content of a flat, a secret, the reason, params or result of an approval, or an
// operator key, and every body has only the fields of rule 4 for its type (rule 5).
func TestEventBodiesHoldNoContentAndNoSecret(t *testing.T) {
	t.Parallel()
	s, _, rec := webhookService(t)
	s.cfg.Lifecycle = newLifecycleRouteNet()
	const canary = "CANARY-9f3a1c"
	lifecycleSave(t, s, "notes", canary+"-content")
	if _, err := s.CreateFlat(t.Context(), "other", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(t.Context(), "other", "API_KEY", canary+"-secret", ViaConsole); err != nil {
		t.Fatal(err)
	}
	lifecycleApprove(t, s, lifecycleRequest(t, s, "notes"))
	if err := s.SetProviderPermission(t.Context(), "notes", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNetworkPolicy(t.Context(), "notes", []string{"https://api.example.com"}, ViaConsole); err != nil {
		t.Fatal(err)
	}
	req, err := s.SetVisibility(t.Context(), "notes", store.Public, ViaAPI, canary+"-reason")
	if err != nil || req.Approval == nil {
		t.Fatalf("SetVisibility = %+v, %v", req, err)
	}
	if _, err := s.Decide(t.Context(), req.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	del, err := s.Delete(t.Context(), "other", ViaMCP, canary+"-delete")
	if err != nil || del.Approval == nil {
		t.Fatalf("Delete = %+v, %v", del, err)
	}
	if _, err := s.Decide(t.Context(), del.Approval.ID, false); err != nil {
		t.Fatal(err)
	}
	got := rec.take()
	if len(got) < 7 {
		t.Fatalf("only %d events: %v", len(got), shorts(got))
	}
	approvals, err := s.st.ListApprovals(t.Context(), "")
	if err != nil || len(approvals) < 3 {
		t.Fatalf("approvals = %d, %v", len(approvals), err)
	}
	var texts []string
	for _, a := range approvals {
		if len(a.Params) > 2 {
			texts = append(texts, string(a.Params))
		}
		if a.Result != "" {
			texts = append(texts, a.Result)
		}
	}
	if len(texts) < 4 {
		t.Fatalf("only %d params and results to look for: %q", len(texts), texts)
	}
	allowed := map[string][]string{
		"approval.created":   {"id", "type", "at", "flat", "operation", "state", "approval_id", "via", "console_url"},
		"approval.decided":   {"id", "type", "at", "flat", "operation", "state", "approval_id", "decided_by", "console_url"},
		"visibility.changed": {"id", "type", "at", "flat", "operation", "state", "approval_id", "decided_by", "change", "console_url"},
		"network.changed":    {"id", "type", "at", "flat", "operation", "state", "change", "console_url"},
	}
	for _, p := range got {
		if p.ID != "" {
			t.Errorf("the service set an ID, which the sender owns: %+v", p)
		}
		p.ID = "evt_test"
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), canary) || strings.Contains(string(raw), "API_KEY") {
			t.Errorf("the body of %s holds content, a secret or an approval text: %s", p.Type, raw)
		}
		for _, text := range texts {
			if strings.Contains(string(raw), text) {
				t.Errorf("the body of %s holds the params or the result of an approval (%q): %s", p.Type, text, raw)
			}
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for name := range fields {
			if !slices.Contains(allowed[p.Type], name) {
				t.Errorf("the body of %s has the field %q that rule 4 does not give it: %s", p.Type, name, raw)
			}
		}
	}
}

// A service without a notifier, and one whose notifier was set to nil, make no event and do not fail.
func TestEventsAreOffWithoutANotifier(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	lifecycleSave(t, s, "notes", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "notes"))
	s.SetNotifier(nil)
	if err := s.SetNetworkPolicy(context.Background(), "notes", []string{"https://api.example.com"}, ViaConsole); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetVisibility(context.Background(), "notes", store.Public, ViaAPI, ""); err != nil {
		t.Fatal(err)
	}
}

// The time of a visibility.set row is taken immediately before the store writes it, after the exposure was prepared, and
// not when the approval built its change. The export relies on that: a row whose time is before its cutoff is committed
// already (rules 21 and 25).
func TestTheVisibilityAuditTimeIsTakenAtTheWrite(t *testing.T) {
	t.Parallel()
	s, dir, rec := webhookService(t)
	s.cfg.Lifecycle = newLifecycleRouteNet()
	makeLive(t, s, rec, "notes")
	if err := s.SetProviderPermission(t.Context(), "notes", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	req, err := s.SetVisibility(t.Context(), "notes", store.Public, ViaAPI, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("SetVisibility = %+v, %v", req, err)
	}
	// Every reading of the clock is one second later than the one before it.
	base := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	var tick atomic.Int64
	s.now = func() time.Time { return base.Add(time.Duration(tick.Add(1)) * time.Second) }
	if _, err := s.Decide(t.Context(), req.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	f, err := s.st.GetFlat(t.Context(), "notes")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range auditOf(t, dir) {
		if e.Kind != store.AuditVisibilitySet {
			continue
		}
		// The row has a reading of the replaced clock: whole seconds after the base, later than the reading of the
		// approval's change and not later than the last reading. A row with the time of the machine fails this.
		last := base.Add(time.Duration(tick.Load()) * time.Second)
		if !e.Time.After(f.UpdatedAt) || e.Time.After(last) || e.Time.Sub(base)%time.Second != 0 {
			t.Errorf("the row has the time %s; want a reading of the replaced clock after %s and not after %s", e.Time, f.UpdatedAt, last)
		}
		return
	}
	t.Fatal("no visibility.set row")
}

// The changes that rule 1 leaves out send nothing: the creation of a flat, a secret (also an injected one), and a rename.
func TestExcludedChangesSendNoEvent(t *testing.T) {
	t.Parallel()
	s, _, rec := webhookService(t)
	ctx := t.Context()
	if _, err := s.CreateFlat(ctx, "app", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(ctx, "app", "API_KEY", "a-secret-value", ViaConsole); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecret(ctx, "app", "API_KEY", ViaConsole); err != nil {
		t.Fatal(err)
	}
	// An injected secret is a secret too (docs/spec/injected-fetch-secrets.md, rule 30).
	if _, err := s.SetSecretBinding(ctx, "app", "TOKEN", "Bearer a-secret-value", &SecretInject{Origin: "https://api.example.com", Header: "Authorization"}, ViaCLI); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecret(ctx, "app", "TOKEN", ViaCLI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameSlug(ctx, "app", "renamed", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Errorf("events = %v, want none", shorts(got))
	}
}

// The longest list of origins that the host accepts is stored whole, and its event fits in a body: it carries the count
// of the whole list and the first origins that fit, and says that the list is cut (rule 4).
func TestTheLongestOriginListIsStoredAndItsEventIsCut(t *testing.T) {
	t.Parallel()
	s, _, rec := webhookService(t)
	if _, err := s.CreateFlat(t.Context(), "notes", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	origins := make([]string, 32)
	for i := range origins {
		label := func(size int) string { return fmt.Sprintf("h%02d", i) + strings.Repeat("a", size-3) }
		origins[i] = "https://" + label(63) + "." + label(63) + "." + label(63) + "." + label(61)
	}
	if err := s.SetNetworkPolicy(t.Context(), "notes", origins, ViaConsole); err != nil {
		t.Fatalf("the longest list of origins was refused: %v", err)
	}
	stored, err := s.NetworkPolicy(t.Context(), "notes")
	if err != nil || len(stored.Origins) != 32 {
		t.Fatalf("stored %d origins, %v; want 32", len(stored.Origins), err)
	}
	got := rec.take()
	if len(got) != 1 || got[0].Change == nil {
		t.Fatalf("events = %+v", got)
	}
	c := got[0].Change
	if c.Count != 32 || !c.Truncated || len(c.Origins) == 0 || len(c.Origins) >= 32 || !slices.Equal(c.Origins, stored.Origins[:len(c.Origins)]) {
		t.Errorf("change: count %d, truncated %t, %d origins; want 32, true and the first origins that fit", c.Count, c.Truncated, len(c.Origins))
	}
	p := got[0]
	p.ID = "evt_abcdefghijklmnopqrstuvwxyz"
	p.ConsoleURL = "https://" + strings.Repeat("c", 253) + "/flats/" + strings.Repeat("s", 63)
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > 4096 {
		t.Errorf("the body has %d bytes with the longest console address, %v; want at most 4096", len(raw), err)
	}
}

// When the audit write fails, the change does not happen and no event of the change is sent (rule 21): the origin
// grants and the provider permission stay as they were, and a visibility approval fails with the flat still Private.
func TestAFailedAuditWriteStopsTheChangeAndItsEvent(t *testing.T) {
	t.Parallel()
	s, dir, rec := webhookService(t)
	s.cfg.Lifecycle = newLifecycleRouteNet()
	makeLive(t, s, rec, "notes")
	ctx := t.Context()
	if err := s.SetProviderPermission(ctx, "notes", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	req, err := s.SetVisibility(ctx, "notes", store.Public, ViaAPI, "")
	if err != nil || req.Approval == nil {
		t.Fatalf("SetVisibility = %+v, %v", req, err)
	}
	rec.take()
	boardExecDB(t, filepath.Join(dir, "flats.db"), "CREATE TRIGGER audit_fail BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit write failed'); END")

	if err := s.SetNetworkPolicy(ctx, "notes", []string{"https://api.example.com"}, ViaConsole); err == nil || !strings.Contains(err.Error(), "audit write failed") {
		t.Fatalf("SetNetworkPolicy = %v, want the audit failure", err)
	}
	if p, err := s.NetworkPolicy(ctx, "notes"); err != nil || len(p.Origins) != 0 {
		t.Errorf("origins after the failed write = %v, %v; want none", p.Origins, err)
	}
	if err := s.SetProviderPermission(ctx, "notes", store.ProviderPortal, false, ViaConsole); err == nil || !strings.Contains(err.Error(), "audit write failed") {
		t.Fatalf("SetProviderPermission = %v, want the audit failure", err)
	}
	if ok, err := s.st.ProviderPermitted(ctx, "notes", store.ProviderPortal); err != nil || !ok {
		t.Errorf("the permission after the failed revocation = %t, %v; want it kept", ok, err)
	}
	a, _ := s.Decide(ctx, req.Approval.ID, true)
	if a.Status != "failed" || !strings.Contains(a.Result, "audit write failed") {
		t.Errorf("the visibility approval = %s (%s), want failed with the audit failure", a.Status, a.Result)
	}
	if f, err := s.st.GetFlat(ctx, "notes"); err != nil || f.Visibility.Canonical() != store.Private {
		t.Errorf("visibility after the failed commit = %s, %v; want private", f.Visibility, err)
	}
	if got := shorts(rec.take()); !slices.Equal(got, []string{"approval.decided set_visibility failed by operator"}) {
		t.Errorf("events = %v, want only the failed decision", got)
	}
}
