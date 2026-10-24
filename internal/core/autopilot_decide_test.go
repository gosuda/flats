package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

var bot = AgentDecision{Agent: "bot", Arrival: Arrival{Network: "local"}}

const botText = `Autopilot (agent "bot" via Local)`

// allowAutopilot turns Autopilot on with exactly the operations o.
func allowAutopilot(t *testing.T, s *Service, o ...AutopilotOp) {
	t.Helper()
	for _, op := range AutopilotOps {
		if _, err := s.SetAutopilotOp(t.Context(), op, slices.Contains(o, op), ""); err != nil {
			t.Fatal(err)
		}
	}
	set := ops(o...)
	if _, err := s.SetAutopilotEnabled(t.Context(), true, &set, ""); err != nil {
		t.Fatal(err)
	}
}

func approvalEvents(t *testing.T, s *Service, flat string) []string {
	t.Helper()
	evs, err := s.st.ListEvents(t.Context(), flat, "approval", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if !strings.Contains(e.Message, " requested via ") {
			out = append(out, e.Message)
		}
	}
	return out
}

func pendingID(t *testing.T, err error) string {
	t.Helper()
	var p *PendingApproval
	if !errors.As(err, &p) {
		t.Fatalf("want a pending approval: %v", err)
	}
	return p.Approval.ID
}

// resultID returns the approval of a request's result: resultID(t)(call).
func resultID(t *testing.T) func(ActionResult, error) string {
	return func(r ActionResult, err error) string {
		t.Helper()
		if err != nil || r.Approval == nil {
			t.Fatalf("want a pending approval: %+v %v", r, err)
		}
		return r.Approval.ID
	}
}

// insertApproval stores a pending approval with arbitrary frozen params.
func insertApproval(t *testing.T, s *Service, id, action, params string) store.Approval {
	t.Helper()
	a := store.Approval{ID: id, Flat: "frozen", Action: action, Params: []byte(params), Status: "pending", Via: "mcp", RequestedAt: time.Now().UTC().Truncate(time.Millisecond)}
	if err := s.st.InsertApproval(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	a, err := s.st.GetApproval(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDecideAsAgentRefusals(t *testing.T) {
	ctx := t.Context()
	s, _ := newAutopilotService(t, time.Time{})
	lifecycleSave(t, s, "refuse", "one")
	del := resultID(t)(s.Delete(ctx, "refuse", ViaMCP, ""))
	unknown := insertApproval(t, s, "apr-unknown", "rename", `{"to":"x"}`).ID
	check := func(name, id string, approve bool, d AgentDecision, want error, category string) {
		t.Helper()
		before, _ := s.st.GetApproval(ctx, id)
		a, err := s.DecideAsAgent(ctx, id, approve, d)
		if !errors.Is(err, want) || ErrorCategory(err) != category || a.ID != "" {
			t.Fatalf("%s: %+v %v (%s)", name, a, err, ErrorCategory(err))
		}
		if after, _ := s.st.GetApproval(ctx, id); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s changed the approval: %+v", name, after)
		}
	}
	named := func(n string) AgentDecision { return AgentDecision{Agent: n, Arrival: bot.Arrival} }
	// The agent name is checked first, then whether Autopilot is on.
	for _, n := range []string{"", " ", "\t\n", strings.Repeat("é", 101)} {
		check(fmt.Sprintf("name %q", n), del, true, named(n), ErrInvalid, "invalid")
	}
	check("off", del, true, named(strings.Repeat("é", 100)), ErrAutopilotOff, "autopilot_off")
	check("off, unknown id", "apr-missing", true, bot, ErrAutopilotOff, "autopilot_off")
	allowAutopilot(t, s)
	check("unknown id", "apr-missing", false, bot, store.ErrNotFound, "not_found")
	for _, approve := range []bool{true, false} {
		check("nothing allowed", del, approve, bot, ErrAutopilotNotAllowed, "autopilot_not_allowed")
		check("unknown action", unknown, approve, bot, ErrAutopilotNotAllowed, "autopilot_not_allowed")
	}
	// Every operation but the needed one.
	others := slices.DeleteFunc(slices.Clone(AutopilotOps), func(op AutopilotOp) bool { return op == OpDeleteFlat })
	allowAutopilot(t, s, others...)
	for _, approve := range []bool{true, false} {
		check("other operations allowed", del, approve, bot, ErrAutopilotNotAllowed, "autopilot_not_allowed")
	}
	_, err := s.DecideAsAgent(ctx, del, true, bot)
	if !strings.Contains(err.Error(), "Delete flats (delete_flat)") {
		t.Fatalf("refusal does not name the operation: %v", err)
	}
	_, err = s.DecideAsAgent(ctx, unknown, true, bot)
	if !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("refusal does not give the reason: %v", err)
	}
	if evs := approvalEvents(t, s, "refuse"); len(evs) != 0 {
		t.Fatalf("refusals wrote events %q", evs)
	}
}

// Unreadable frozen params and invalid visibility values are refused for
// approve and reject alike, with every operation allowed, and leave the
// approval as it was.
func TestDecideAsAgentRefusesUnclassified(t *testing.T) {
	s, _ := newAutopilotService(t, time.Time{})
	allowAutopilot(t, s, AutopilotOps...)
	cases := map[string][2]string{
		"null":            {"activate", `null`},
		"array":           {"activate", `[1]`},
		"string":          {"delete", `"blog"`},
		"number":          {"rollback", `7`},
		"invalid":         {"create_version", `{"revision":`},
		"null member":     {"rollback", `{"version":1,"restore_data":null}`},
		"wrong type":      {"activate", `{"version":"broken"}`},
		"wrong bool":      {"rollback", `{"restore_data":"yes"}`},
		"wrong seq":       {"set_visibility", `{"from":"private","visibility":"public","access_seq":"x","providers":"p"}`},
		"legacy":          {"set_visibility", `{"visibility":"public","providers":"p"}`},
		"bogus to":        {"set_visibility", `{"from":"public","visibility":"bogus","providers":"p"}`},
		"bogus from":      {"set_visibility", `{"from":"bogus","visibility":"private","providers":"p"}`},
		"case":            {"set_visibility", `{"from":"private","visibility":"Public","providers":"p"}`},
		"empty to":        {"set_visibility", `{"from":"public","visibility":"","providers":"p"}`},
		"spaced":          {"set_visibility", `{"from":"private ","visibility":"public","providers":"p"}`},
		"number to":       {"set_visibility", `{"from":"public","visibility":0,"providers":"p"}`},
		"unknown action":  {"deploy", `{"version":1}`},
		"no change":       {"set_visibility", `{"from":"private","visibility":"private","providers":"p"}`},
		"unknown level":   {"set_visibility", `{"from":"private","visibility":"public","access":"admin","access_seq":1,"providers":"p"}`},
		"same level":      {"set_visibility", `{"from":"public","visibility":"public","from_access":"view","access":"view","access_seq":1,"providers":"p"}`},
		"no start level":  {"set_visibility", `{"from":"public","visibility":"public","access":"edit","access_seq":1,"providers":"p"}`},
		"bad start level": {"set_visibility", `{"from":"public","visibility":"private","from_access":"x","providers":"p"}`},
	}
	i := 0
	for name, c := range cases {
		i++
		before := insertApproval(t, s, fmt.Sprint("apr-u", i), c[0], c[1])
		for _, approve := range []bool{true, false} {
			if _, err := s.DecideAsAgent(t.Context(), before.ID, approve, bot); !errors.Is(err, ErrAutopilotNotAllowed) || ErrorCategory(err) != "autopilot_not_allowed" {
				t.Errorf("%s approve=%t: %v", name, approve, err)
			}
			if after, _ := s.st.GetApproval(t.Context(), before.ID); !reflect.DeepEqual(before, after) {
				t.Errorf("%s approve=%t changed the approval:\n%+v\n%+v", name, approve, before, after)
			}
		}
	}
}

type decider struct {
	name   string
	decide func(s *Service, ctx context.Context, id string, approve bool) (store.Approval, error)
	actor  string // stored decided_by
	text   string // decider in event texts
}

var deciders = []decider{
	{"operator", func(s *Service, ctx context.Context, id string, approve bool) (store.Approval, error) {
		return s.Decide(ctx, id, approve)
	}, "console", "the operator"},
	{"agent", func(s *Service, ctx context.Context, id string, approve bool) (store.Approval, error) {
		return s.DecideAsAgent(ctx, id, approve, bot)
	}, EncodeAgentDecider(bot), botText},
}

// The status rules of rule 20 for both deciders and across them; the
// operator's records keep today's values.
func TestAgentDecisionStatusRules(t *testing.T) {
	for _, first := range deciders {
		for _, second := range deciders {
			t.Run(first.name+" then "+second.name, func(t *testing.T) {
				ctx := t.Context()
				s, _ := newAutopilotService(t, time.Time{})
				allowAutopilot(t, s, AutopilotOps...)
				lifecycleSave(t, s, "rules", "one")

				// Rejection.
				del := resultID(t)(s.Delete(ctx, "rules", ViaMCP, ""))
				a, err := first.decide(s, ctx, del, false)
				if err != nil || a.Status != "rejected" || a.DecidedBy != first.actor || a.Result != "rejected by "+first.text || a.AuthorizedAt == nil {
					t.Fatalf("reject %+v %v", a, err)
				}
				events := approvalEvents(t, s, "rules")
				if want := []string{"delete rejected by " + first.text}; !slices.Equal(events, want) {
					t.Fatalf("events %q, want %q", events, want)
				}
				again, err := second.decide(s, ctx, del, false)
				if err != nil || !reflect.DeepEqual(again, a) {
					t.Fatalf("same direction %+v %v", again, err)
				}
				if _, err := second.decide(s, ctx, del, true); !errors.Is(err, ErrConflict) {
					t.Fatalf("opposite direction %v", err)
				}

				// Approval.
				pub := resultID(t)(s.Publish(ctx, "rules", 0, "", ViaMCP))
				a, err = first.decide(s, ctx, pub, true)
				if err != nil || a.Status != "approved" || a.DecidedBy != first.actor || a.Result != "saved version 1" {
					t.Fatalf("approve %+v %v", a, err)
				}
				events = approvalEvents(t, s, "rules")
				if want := "create_version approved by " + first.text + ": saved version 1"; events[len(events)-1] != want {
					t.Fatalf("event %q, want %q", events[len(events)-1], want)
				}
				again, err = second.decide(s, ctx, pub, true)
				if err != nil || !reflect.DeepEqual(again, a) {
					t.Fatalf("same direction %+v %v", again, err)
				}
				if _, err := second.decide(s, ctx, pub, false); !errors.Is(err, ErrConflict) {
					t.Fatalf("opposite direction %v", err)
				}
				if got := approvalEvents(t, s, "rules"); len(got) != len(events) {
					t.Fatalf("repeated decisions wrote events %q", got[len(events):])
				}

				// An approval being applied.
				lifecycleSave(t, s, "rules", "two")
				applying := resultID(t)(s.Publish(ctx, "rules", 0, "", ViaMCP))
				if err := s.st.ClaimApprovalAuthorized(ctx, applying, "console", time.Now()); err != nil {
					t.Fatal(err)
				}
				for _, approve := range []bool{true, false} {
					if _, err := second.decide(s, ctx, applying, approve); !errors.Is(err, ErrConflict) {
						t.Fatalf("applying, approve=%t: %v", approve, err)
					}
				}
			})
		}
	}
	t.Run("operator identity", func(t *testing.T) {
		ctx := WithActor(t.Context(), "alice@example.com")
		s, _ := newAutopilotService(t, time.Time{})
		lifecycleSave(t, s, "who", "one")
		a, err := s.Decide(ctx, resultID(t)(s.Delete(ctx, "who", ViaMCP, "")), false)
		if err != nil || a.DecidedBy != "alice@example.com" || a.Result != "rejected by the operator" {
			t.Fatalf("%+v %v", a, err)
		}
		if got := approvalEvents(t, s, "who"); !slices.Equal(got, []string{"delete rejected by the operator"}) {
			t.Fatalf("events %q", got)
		}
	})
}

// Operator and agent decisions of one approval race: exactly one applies.
func TestAgentAndOperatorDecideConcurrently(t *testing.T) {
	ctx := t.Context()
	s, _ := newAutopilotService(t, time.Time{})
	allowAutopilot(t, s, AutopilotOps...)
	for round := range 5 {
		lifecycleSave(t, s, "race", fmt.Sprint("content ", round))
		id := resultID(t)(s.Publish(ctx, "race", 0, "", ViaMCP))
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d := deciders[i%2]
				if _, err := d.decide(s, ctx, id, i%4 < 2); err != nil && !errors.Is(err, ErrConflict) {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		a, _ := s.GetApproval(ctx, id)
		if a.Status != "approved" && a.Status != "rejected" {
			t.Fatalf("round %d: %+v", round, a)
		}
		evs := approvalEvents(t, s, "race")
		decided := 0
		for _, e := range evs {
			if strings.Contains(e, " by ") {
				decided++
			}
		}
		if decided != round+1 {
			t.Fatalf("round %d: %d decision events", round, decided)
		}
	}
}

// An agent decision fails stale frozen state like the operator's.
func TestAgentDecisionStaleApprovals(t *testing.T) {
	ctx := t.Context()
	n := &lifecycleNetwork{policy: "grant", state: "ready"}
	s, _ := newAutopilotServiceWith(t, time.Time{}, n)
	allowAutopilot(t, s, AutopilotOps...)
	lifecycleSave(t, s, "stale", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "stale"))
	if err := s.SetProviderPermission(ctx, "stale", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	vis := resultID(t)(s.SetVisibility(ctx, "stale", store.Public, ViaMCP, ""))
	lifecycleSave(t, s, "stale", "two")
	if _, err := s.Decide(ctx, lifecycleRequest(t, s, "stale").Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	_, err := s.Deploy(ctx, "stale", 2, ViaMCP)
	activate := pendingID(t, err)
	lifecycleSave(t, s, "stale", "three")
	if _, err := s.Decide(ctx, lifecycleRequest(t, s, "stale").Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	_, err = s.Deploy(ctx, "stale", 3, ViaMCP)
	if _, err := s.Decide(ctx, pendingID(t, err), true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{vis, activate} {
		a, err := s.DecideAsAgent(ctx, id, true, bot)
		if !errors.Is(err, ErrStaleApproval) || a.Status != "failed" || execution(t, a).FailureCode != "stale_approval" || a.DecidedBy != EncodeAgentDecider(bot) {
			t.Fatalf("%s: %+v %v", id, a, err)
		}
	}
	if f, _ := s.GetFlat(ctx, "stale"); f.Visibility != store.Private || f.LiveVersion != 3 {
		t.Fatalf("stale approvals changed the flat: %+v", f)
	}
}

// An agent decision grants no provider: making a flat Public through it
// still needs the operator's provider permission.
func TestAgentDecisionGrantsNoProvider(t *testing.T) {
	ctx := t.Context()
	n := &lifecycleNetwork{policy: "configured", state: "ready"}
	s, _ := newAutopilotServiceWith(t, time.Time{}, n)
	allowAutopilot(t, s, AutopilotOps...)
	lifecycleSave(t, s, "reach", "one")
	lifecycleApprove(t, s, lifecycleRequest(t, s, "reach"))
	id := resultID(t)(s.SetVisibility(ctx, "reach", store.Public, ViaMCP, ""))
	a, err := s.DecideAsAgent(ctx, id, true, bot)
	if !errors.Is(err, ErrProviderNotPermitted) || a.Status != "failed" || execution(t, a).FailureCode != "provider_not_permitted" {
		t.Fatalf("%+v %v", a, err)
	}
	f, _ := s.GetFlat(ctx, "reach")
	permitted, _ := s.st.ProviderPermitted(ctx, "reach", store.ProviderPortal)
	if f.Visibility != store.Private || permitted || n.public != nil {
		t.Fatalf("flat %+v, portal permitted %t", f, permitted)
	}
}

// prepareApproval leaves a pending approval of action on flat "crash".
func prepareApproval(t *testing.T, s *Service, action string) string {
	t.Helper()
	ctx := t.Context()
	lifecycleSave(t, s, "crash", "one")
	switch action {
	case "create_version":
		return resultID(t)(s.Publish(ctx, "crash", 0, "", ViaMCP))
	}
	lifecycleApprove(t, s, lifecycleRequest(t, s, "crash"))
	switch action {
	case "set_visibility":
		if err := s.SetProviderPermission(ctx, "crash", store.ProviderPortal, true, ViaConsole); err != nil {
			t.Fatal(err)
		}
		return resultID(t)(s.SetVisibility(ctx, "crash", store.Public, ViaMCP, ""))
	case "delete":
		return resultID(t)(s.Delete(ctx, "crash", ViaMCP, ""))
	}
	lifecycleSave(t, s, "crash", "two")
	if _, err := s.Decide(ctx, lifecycleRequest(t, s, "crash").Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	switch action {
	case "delete_version":
		return resultID(t)(s.DeleteVersion(ctx, "crash", 2, ViaMCP))
	case "activate":
		_, err := s.Deploy(ctx, "crash", 2, ViaMCP)
		return pendingID(t, err)
	case "rollback":
		_, err := s.Deploy(ctx, "crash", 2, ViaMCP)
		if _, err := s.Decide(ctx, pendingID(t, err), true); err != nil {
			t.Fatal(err)
		}
		_, err = s.Rollback(ctx, "crash", 1, ViaMCP)
		return pendingID(t, err)
	case "rollback_restore", "restore_data":
		// Deploying version 2 snapshots "before"; a restore puts it back.
		writeNote(t, s, "crash", "before")
		_, err := s.Deploy(ctx, "crash", 2, ViaMCP)
		if _, err := s.Decide(ctx, pendingID(t, err), true); err != nil {
			t.Fatal(err)
		}
		writeNote(t, s, "crash", "after")
		if action == "rollback_restore" {
			_, err = s.RollbackWithData(ctx, "crash", 1, true, ViaMCP)
		} else {
			_, err = s.RestoreSnapshot(ctx, "crash", snapshotBefore(t, s, "crash", 2), ViaMCP)
		}
		return pendingID(t, err)
	}
	t.Fatalf("no setup for %s", action)
	return ""
}

// writeNote writes a flat's FILES note, the data a restore puts back.
func writeNote(t *testing.T, s *Service, slug, text string) {
	t.Helper()
	if err := writeFile(filepath.Join(s.dataDirOf(slug), "files", "note"), text); err != nil {
		t.Fatal(err)
	}
}

func readNote(t *testing.T, s *Service, slug string) string {
	t.Helper()
	b, err := readFile(filepath.Join(s.dataDirOf(slug), "files", "note"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// snapshotBefore names the snapshot taken before version n was deployed.
func snapshotBefore(t *testing.T, s *Service, slug string, n int) string {
	t.Helper()
	name, err := s.liveSnapshot(slug, n)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func claim(t *testing.T, s *Service, d decider, id string) store.Approval {
	t.Helper()
	ctx := t.Context()
	a, _ := s.st.GetApproval(ctx, id)
	if d.name == "agent" {
		need, err := AutopilotNeeds(a.Action, a.Params)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.st.ClaimApprovalAutopilot(ctx, id, d.actor, uint16(need), time.Now()); err != nil {
			t.Fatal(err)
		}
	} else if err := s.st.ClaimApprovalAuthorized(ctx, id, d.actor, time.Now()); err != nil {
		t.Fatal(err)
	}
	a, _ = s.st.GetApproval(ctx, id)
	return a
}

// A restart finishes a claimed approval, before or after its operation
// applied, with one decision event for an agent decision and today's
// records for an operator decision.
func TestAgentDecisionResumesAfterRestart(t *testing.T) {
	viaWords := map[string]string{"create_version": "saved version 1 from draft revision", "activate": "deploy: version 2 is live (was 1) via", "rollback": "rollback: version 1 is live (was 2) via"}
	// rollback_restore is a rollback with restore_data; restore_data is the
	// action that restores the live version's data.
	for _, setup := range []string{"create_version", "delete_version", "activate", "rollback", "rollback_restore", "restore_data", "set_visibility", "delete"} {
		action := setup
		if setup == "rollback_restore" {
			action = "rollback"
		}
		for _, applied := range []bool{false, true} {
			if action == "delete" && applied {
				continue // a deleted flat keeps no events
			}
			for _, d := range deciders {
				name := fmt.Sprintf("%s/applied=%t/%s", setup, applied, d.name)
				t.Run(name, func(t *testing.T) {
					ctx := t.Context()
					s, logs := newAutopilotServiceWith(t, time.Time{}, &lifecycleNetwork{policy: "grant", state: "ready"})
					allowAutopilot(t, s, AutopilotOps...)
					id := prepareApproval(t, s, setup)
					before := approvalEvents(t, s, "crash")
					a := claim(t, s, d, id)
					if applied {
						if _, err := s.applyApproval(ctx, a); err != nil {
							t.Fatal(err)
						}
					}
					s.resumeApplying(ctx)
					a, _ = s.GetApproval(ctx, id)
					if a.Status != "approved" || a.DecidedBy != d.actor {
						t.Fatalf("resumed %+v", a)
					}
					if a.Action != action {
						t.Fatalf("action %s, want %s", a.Action, action)
					}
					if (setup == "rollback_restore" || setup == "restore_data") && readNote(t, s, "crash") != "before" {
						t.Fatal("data not restored")
					}
					if action == "delete" {
						if _, err := s.st.GetFlat(ctx, "crash"); !errors.Is(err, store.ErrNotFound) {
							t.Fatal("flat not deleted")
						}
						want := "flat crash deleted via " + string(applyVia(a))
						if len(logs.matching(want)) != 1 {
							t.Fatalf("host log lacks %q", want)
						}
						return
					}
					added := approvalEvents(t, s, "crash")[len(before):]
					var want []string
					if d.name == "agent" || !applied {
						want = []string{fmt.Sprintf("%s approved by %s: %s", action, d.text, a.Result)}
					}
					if !slices.Equal(added, want) {
						t.Fatalf("decision events %q, want %q", added, want)
					}
					if w, ok := viaWords[setup]; ok && !applied {
						via := "via " + string(applyVia(a))
						evs, _ := s.st.ListEvents(ctx, "crash", "", 0, 1000)
						found := false
						for _, e := range evs {
							found = found || (strings.Contains(e.Message, w) && strings.HasSuffix(e.Message, via))
						}
						if !found {
							t.Fatalf("no %q event %s", w, via)
						}
					}
					// Later resumes and decisions add nothing.
					s.resumeApplying(ctx)
					if _, err := s.Decide(ctx, id, true); err != nil {
						t.Fatal(err)
					}
					if _, err := s.DecideAsAgent(ctx, id, true, bot); err != nil {
						t.Fatal(err)
					}
					if got := approvalEvents(t, s, "crash")[len(before):]; !slices.Equal(got, want) {
						t.Fatalf("events after repeats %q", got)
					}
				})
			}
		}
	}
}

// approvalIDs and durations differ between runs.
var (
	approvalIDs   = regexp.MustCompile(`apr-[0-9a-z]+`)
	durations     = regexp.MustCompile(`in [0-9.]+[µm]?s\b`)
	snapshotTimes = regexp.MustCompile(`-\d+\.sqlite\b`)
)

// runScenario requests and decides one approval of each action on a new
// service and returns what it recorded, with decider and surface words
// replaced by placeholders.
func runScenario(t *testing.T, d decider) (approvals []string, events []string, logLines []string, flat store.Flat) {
	t.Helper()
	ctx := t.Context()
	n := &lifecycleNetwork{policy: "grant", state: "ready"}
	s, logs := newAutopilotServiceWith(t, time.Time{}, n)
	allowAutopilot(t, s, AutopilotOps...)
	// Requests keep saying they wait for the operator.
	norm := strings.NewReplacer("waiting for the operator", "waiting for the operator",
		d.text, "<decider>", "via "+string(ViaAutopilot), "via <surface>", "via "+string(ViaConsole), "via <surface>")
	decide := func(id string) {
		t.Helper()
		a, err := d.decide(s, ctx, id, true)
		if err != nil || a.Status != "approved" {
			t.Fatalf("%s: %+v %v", id, a, err)
		}
		if a.DecidedBy != d.actor {
			t.Fatalf("decided_by %q", a.DecidedBy)
		}
		approvals = append(approvals, snapshotTimes.ReplaceAllString(fmt.Sprintf("%s|%s|%s|%s", a.Action, a.Status, norm.Replace(a.Result), a.ResultData), "-<t>.sqlite"))
	}
	lifecycleSave(t, s, "equal", "one")
	decide(resultID(t)(s.Publish(ctx, "equal", 0, "", ViaMCP)))
	_, err := s.Deploy(ctx, "equal", 1, ViaMCP)
	decide(pendingID(t, err))
	lifecycleSave(t, s, "equal", "two")
	decide(resultID(t)(s.Publish(ctx, "equal", 0, "", ViaMCP)))
	writeNote(t, s, "equal", "before")
	_, err = s.Deploy(ctx, "equal", 2, ViaMCP)
	decide(pendingID(t, err))
	_, err = s.Rollback(ctx, "equal", 1, ViaMCP)
	decide(pendingID(t, err))
	_, err = s.Deploy(ctx, "equal", 2, ViaMCP)
	decide(pendingID(t, err))
	writeNote(t, s, "equal", "after")
	_, err = s.RollbackWithData(ctx, "equal", 1, true, ViaMCP)
	decide(pendingID(t, err))
	if readNote(t, s, "equal") != "before" {
		t.Fatal("rollback with restore_data did not restore the data")
	}
	writeNote(t, s, "equal", "after")
	_, err = s.RestoreSnapshot(ctx, "equal", snapshotBefore(t, s, "equal", 2), ViaMCP)
	decide(pendingID(t, err))
	if readNote(t, s, "equal") != "before" {
		t.Fatal("restore_data did not restore the data")
	}
	decide(resultID(t)(s.DeleteVersion(ctx, "equal", 2, ViaMCP)))
	if err := s.SetProviderPermission(ctx, "equal", store.ProviderPortal, true, ViaConsole); err != nil {
		t.Fatal(err)
	}
	decide(resultID(t)(s.SetVisibility(ctx, "equal", store.Public, ViaMCP, "")))
	decide(resultID(t)(s.SetVisibility(ctx, "equal", store.Private, ViaMCP, "")))
	flat, _ = s.st.GetFlat(ctx, "equal")
	flat.CreatedAt, flat.UpdatedAt, flat.ID = time.Time{}, time.Time{}, ""
	evs, _ := s.st.ListEvents(ctx, "equal", "", 0, 1000)
	for _, e := range evs {
		msg := snapshotTimes.ReplaceAllString(approvalIDs.ReplaceAllString(norm.Replace(e.Message), "<id>"), "-<t>.sqlite")
		events = append(events, e.Kind+"|"+durations.ReplaceAllString(msg, "in <t>"))
	}
	id := resultID(t)(s.Delete(ctx, "equal", ViaMCP, ""))
	decide(id)
	if a, _ := s.GetApproval(ctx, id); a.DecidedBy != d.actor {
		t.Fatalf("deleted flat's approval lost its decider: %+v", a)
	}
	for _, l := range logs.matching("via ") {
		logLines = append(logLines, snapshotTimes.ReplaceAllString(approvalIDs.ReplaceAllString(norm.Replace(l), "<id>"), "-<t>.sqlite"))
	}
	return approvals, events, logLines, flat
}

// Agent and operator decisions of every action give the same state,
// results and events, apart from the decider and the surface words.
func TestAgentDecisionMatchesOperator(t *testing.T) {
	opA, opE, opL, opF := runScenario(t, deciders[0])
	agA, agE, agL, agF := runScenario(t, deciders[1])
	if !slices.Equal(opA, agA) {
		t.Fatalf("approvals differ:\n%s\n%s", strings.Join(opA, "\n"), strings.Join(agA, "\n"))
	}
	if !slices.Equal(opE, agE) {
		t.Fatalf("events differ:\n%s\n%s", strings.Join(opE, "\n"), strings.Join(agE, "\n"))
	}
	if !slices.Equal(opL, agL) {
		t.Fatalf("host log differs:\n%s\n%s", strings.Join(opL, "\n"), strings.Join(agL, "\n"))
	}
	if !reflect.DeepEqual(opF, agF) {
		t.Fatalf("flat differs:\n%+v\n%+v", opF, agF)
	}
	if len(opA) != 12 || !slices.ContainsFunc(opA, func(a string) bool { return strings.HasPrefix(a, "restore_data|approved|") }) ||
		!slices.ContainsFunc(opA, func(a string) bool {
			return strings.HasPrefix(a, "rollback|") && strings.Contains(a, `"data_impact":"restore_data"`)
		}) || !slices.ContainsFunc(opE, func(e string) bool { return strings.Contains(e, "<decider>") }) ||
		!slices.ContainsFunc(opE, func(e string) bool { return strings.Contains(e, "via <surface>") }) ||
		!slices.ContainsFunc(opL, func(l string) bool { return strings.Contains(l, "deleted via <surface>") }) {
		t.Fatalf("scenario did not record what it compares:\n%s\n%s", strings.Join(opE, "\n"), strings.Join(opL, "\n"))
	}
}
