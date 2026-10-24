package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/store"
)

func TestOpSet(t *testing.T) {
	if len(AutopilotOps) != 9 || len(AutopilotOpInfos) != len(AutopilotOps) {
		t.Fatalf("%d ops, %d infos", len(AutopilotOps), len(AutopilotOpInfos))
	}
	for i, info := range AutopilotOpInfos {
		if info.ID != AutopilotOps[i] || info.Label == "" || info.Requests == "" {
			t.Fatalf("info %d: %+v", i, info)
		}
	}
	s, err := ParseOpSet([]string{"delete_flat", "deploy", "create_version", "deploy"})
	if err != nil || s != 1|1<<2|1<<8 {
		t.Fatalf("parse %b %v", s, err)
	}
	if !slices.Equal(s.List(), []AutopilotOp{OpCreateVersion, OpDeploy, OpDeleteFlat}) || !s.Has(OpDeploy) || s.Has(OpRollback) || s.Has("bogus") {
		t.Fatalf("list %v", s.List())
	}
	if s.Labels() != "Create versions, Deploy, Delete flats" {
		t.Fatalf("labels %q", s.Labels())
	}
	if got := OpSet(0).List(); got == nil || len(got) != 0 {
		t.Fatalf("empty list %#v", got)
	}
	if _, err := ParseOpSet([]string{"deploy", "publish"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestGuideState(t *testing.T) {
	if (Autopilot{Allowed: 3, Guide: "g"}).GuideState() != nil {
		t.Fatal("off state is not nil")
	}
	st := Autopilot{Enabled: true, Allowed: opBit(OpDeploy) | opBit(OpDeleteFlat), Guide: "g"}.GuideState()
	want := &runtimeref.AutopilotState{Guide: "g", Allowed: []runtimeref.AutopilotOperation{
		{ID: "deploy", Label: "Deploy", Requests: "deploy of a saved version (activation or redeploy)"},
		{ID: "delete_flat", Label: "Delete flats", Requests: "delete_flat"},
	}}
	if fmt.Sprint(st) != fmt.Sprint(want) {
		t.Fatalf("state %+v", st)
	}
	if st := (Autopilot{Enabled: true}).GuideState(); st == nil || len(st.Allowed) != 0 {
		t.Fatalf("none allowed %+v", st)
	}
}

func ops(o ...AutopilotOp) OpSet {
	var s OpSet
	for _, op := range o {
		s |= opBit(op)
	}
	return s
}

// unclassifiedOps marks a table row AutopilotNeeds must refuse.
const unclassifiedOps = OpSet(1 << 15)

func TestAutopilotNeeds(t *testing.T) {
	vis := func(fields string) string { return `{"providers":"p",` + fields + `}` }
	cases := []struct {
		action, params string
		want           OpSet
	}{
		{"create_version", `{"revision":2,"hash":"h","base_version":1,"expected_live":1,"visibility":"private","providers":"p"}`, ops(OpCreateVersion)},
		{"create_version", `{}`, ops(OpCreateVersion)},
		{"publish", `{"revision":2}`, ops(OpCreateVersion)},
		{"delete_version", `{"version":2,"hash":"h"}`, ops(OpDeleteVersion)},
		{"activate", `{"version":2,"hash":"h"}`, ops(OpDeploy)},
		{"activate", `{"version":2,"extra":{"nested":null},"more":[1]}`, ops(OpDeploy)},
		{"rollback", `{"version":1}`, ops(OpRollback)},
		{"rollback", `{"version":1,"restore_data":false}`, ops(OpRollback)},
		{"rollback", `{"version":1,"restore_data":true}`, ops(OpRollbackRestoreData)},
		{"restore_data", `{"version":1,"restore_data":true,"snapshot":"s"}`, ops(OpRollbackRestoreData)},
		{"restore_data", `{}`, ops(OpRollbackRestoreData)},
		{"delete", `{"slug":"blog"}`, ops(OpDeleteFlat)},
		{"delete", `{"slug":"another-slug"}`, ops(OpDeleteFlat)},
		{"delete", `{}`, ops(OpDeleteFlat)},
		{"deploy", `{"version":1}`, unclassifiedOps},
		{"rename", `{}`, unclassifiedOps},
		{"", `{}`, unclassifiedOps},

		// Wrong field types.
		{"create_version", `{"revision":"x"}`, unclassifiedOps},
		{"publish", `{"hash":1}`, unclassifiedOps},
		{"delete_version", `{"version":"broken"}`, unclassifiedOps},
		{"activate", `{"version":"broken"}`, unclassifiedOps},
		{"rollback", `{"restore_data":"yes"}`, unclassifiedOps},
		{"restore_data", `{"version":"x"}`, unclassifiedOps},
		{"delete", `{"slug":5}`, unclassifiedOps},
		{"set_visibility", vis(`"from":"private","visibility":"public","access_seq":"x"`), unclassifiedOps},
		{"set_visibility", vis(`"from":"private","visibility":1`), unclassifiedOps},
		// Top-level null members.
		{"activate", `{"version":null}`, unclassifiedOps},
		{"rollback", `{"version":1,"restore_data":null}`, unclassifiedOps},
		{"delete", `{"slug":null}`, unclassifiedOps},
		{"set_visibility", vis(`"from":"private","visibility":"public","access":null`), unclassifiedOps},
		{"set_visibility", vis(`"from":"private","visibility":"public","access_seq":null`), unclassifiedOps},

		// set_visibility.
		{"set_visibility", vis(`"from":"private","visibility":"public"`), ops(OpMakePublic)},
		{"set_visibility", vis(`"from":"private","visibility":"public","access":"edit"`), ops(OpMakePublic)},
		{"set_visibility", vis(`"from":"private","visibility":"public","access":"bogus"`), ops(OpMakePublic)},
		{"set_visibility", vis(`"from":"private","visibility":"public","access":"view","access_seq":3`), ops(OpMakePublic)},
		{"set_visibility", vis(`"from":"private","visibility":"public","access":"","access_seq":3`), ops(OpMakePublic)},
		{"set_visibility", vis(`"from":"private","visibility":"public","access":"edit","access_seq":3`), ops(OpMakePublic, OpPublicEditing)},
		{"set_visibility", vis(`"from":"private","visibility":"public","access":"edit","access_seq":0`), ops(OpMakePublic, OpPublicEditing)},
		{"set_visibility", vis(`"from":"private","visibility":"public","access":"bogus","access_seq":3`), unclassifiedOps},
		{"set_visibility", vis(`"from":"private","visibility":"public","from_access":"bogus"`), ops(OpMakePublic)},
		{"set_visibility", vis(`"from":"public","visibility":"private"`), ops(OpMakePrivate)},
		{"set_visibility", vis(`"from":"public","visibility":"private","from_access":"edit","access_seq":3`), ops(OpMakePrivate)},
		{"set_visibility", vis(`"from":"public","visibility":"private","from_access":"view"`), ops(OpMakePrivate)},
		{"set_visibility", vis(`"from":"public","visibility":"private","from_access":"bogus"`), unclassifiedOps},
		{"set_visibility", vis(`"from":"public","visibility":"public","from_access":"view","access":"edit","access_seq":3`), ops(OpPublicEditing)},
		{"set_visibility", vis(`"from":"public","visibility":"public","from_access":"edit","access":"view","access_seq":3`), ops(OpPublicEditing)},
		{"set_visibility", vis(`"from":"public","visibility":"public","from_access":"edit"`), ops(OpPublicEditing)},
		{"set_visibility", vis(`"from":"public","visibility":"public","from_access":"edit","access":"edit"`), ops(OpPublicEditing)},
		{"set_visibility", vis(`"from":"public","visibility":"public","from_access":"view","access":"view","access_seq":3`), unclassifiedOps},
		{"set_visibility", vis(`"from":"public","visibility":"public","from_access":"edit","access":"edit","access_seq":3`), unclassifiedOps},
		{"set_visibility", vis(`"from":"public","visibility":"public","access":"edit","access_seq":3`), unclassifiedOps},
		{"set_visibility", vis(`"from":"private","visibility":"private"`), unclassifiedOps},
		{"set_visibility", vis(`"visibility":"public"`), unclassifiedOps},
		{"set_visibility", vis(`"from":"","visibility":"public"`), unclassifiedOps},
		// Approvals from before frozen starting visibilities.
		{"set_visibility", `{"visibility":"public","expected_live":1}`, unclassifiedOps},
		{"set_visibility", `{"visibility":"private"}`, unclassifiedOps},
	}
	// Every action with params that are not a readable object.
	for _, action := range []string{"create_version", "publish", "delete_version", "activate", "rollback", "restore_data", "delete", "set_visibility"} {
		for _, p := range []string{``, `null`, `[]`, `[{"version":1}]`, `"s"`, `1`, `true`, `{`, `{"version":1`} {
			cases = append(cases, struct {
				action, params string
				want           OpSet
			}{action, p, unclassifiedOps})
		}
	}
	for _, tc := range cases {
		got, err := AutopilotNeeds(tc.action, json.RawMessage(tc.params))
		var ue *UnclassifiedError
		if tc.want == unclassifiedOps {
			if !errors.As(err, &ue) || ue.Reason == "" || got != 0 {
				t.Errorf("%s %s: got %v %v, want unclassified", tc.action, tc.params, got.List(), err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s %s: got %v %v, want %v", tc.action, tc.params, got.List(), err, tc.want.List())
		}
	}
}

// Only the four visibility values any Flats version defined classify, in
// either field, checked before they are made canonical.
func TestAutopilotNeedsRawVisibility(t *testing.T) {
	need := func(from, to string) (OpSet, error) {
		raw, _ := json.Marshal(map[string]any{"from": from, "visibility": to, "providers": "p", "from_access": "view"})
		return AutopilotNeeds("set_visibility", raw)
	}
	for _, v := range []string{"public", "public-listed", "public-unlisted"} {
		if got, err := need("private", v); err != nil || got != ops(OpMakePublic) {
			t.Errorf("private to %s: %v %v", v, got.List(), err)
		}
		if got, err := need(v, "private"); err != nil || got != ops(OpMakePrivate) {
			t.Errorf("%s to private: %v %v", v, got.List(), err)
		}
	}
	for _, bad := range []string{"bogus", "Public", "PRIVATE", "", "private ", " public"} {
		for _, pair := range [][2]string{{bad, "private"}, {bad, "public"}, {"private", bad}, {"public", bad}} {
			var ue *UnclassifiedError
			if got, err := need(pair[0], pair[1]); !errors.As(err, &ue) {
				t.Errorf("%q to %q: %v %v", pair[0], pair[1], got.List(), err)
			}
		}
	}
}

func TestAgentDeciderRecords(t *testing.T) {
	cases := []struct {
		arrival Arrival
		stored  string
		text    string
	}{
		{Arrival{Network: "local"}, `autopilot:{"agent":"pr-bot","network":"local"}`, `Autopilot (agent "pr-bot" via Local)`},
		{Arrival{Network: "listener", IP: "192.168.1.20"}, `autopilot:{"agent":"pr-bot","network":"listener","ip":"192.168.1.20"}`, `Autopilot (agent "pr-bot" via the console listener from 192.168.1.20)`},
		{Arrival{Network: "listener", IP: "fe80::1%en0"}, `autopilot:{"agent":"pr-bot","network":"listener","ip":"fe80::1%en0"}`, `Autopilot (agent "pr-bot" via the console listener from fe80::1%en0)`},
		{Arrival{Network: "listener", IP: "2001:db8::7"}, `autopilot:{"agent":"pr-bot","network":"listener","ip":"2001:db8::7"}`, `Autopilot (agent "pr-bot" via the console listener from 2001:db8::7)`},
		{Arrival{Network: "tailscale", Login: "bot@example.com"}, `autopilot:{"agent":"pr-bot","network":"tailscale","login":"bot@example.com"}`, `Autopilot (agent "pr-bot" via Tailscale, bot@example.com)`},
		{Arrival{Network: "tailscale", Node: "ci-runner"}, `autopilot:{"agent":"pr-bot","network":"tailscale","node":"ci-runner"}`, `Autopilot (agent "pr-bot" via Tailscale node ci-runner)`},
		{Arrival{Network: "netbird", Peer: "ci.netbird.cloud"}, `autopilot:{"agent":"pr-bot","network":"netbird","peer":"ci.netbird.cloud"}`, `Autopilot (agent "pr-bot" via NetBird peer ci.netbird.cloud)`},
		{Arrival{Network: "netbird"}, `autopilot:{"agent":"pr-bot","network":"netbird"}`, `Autopilot (agent "pr-bot" via NetBird)`},
		{Arrival{Network: "external", Origin: "https://flats.example.com"}, `autopilot:{"agent":"pr-bot","network":"external","origin":"https://flats.example.com"}`, `Autopilot (agent "pr-bot" via external origin https://flats.example.com)`},
	}
	for _, tc := range cases {
		d := AgentDecision{Agent: "pr-bot", Arrival: tc.arrival}
		got := EncodeAgentDecider(d)
		if got != tc.stored {
			t.Errorf("encode %+v = %s", tc.arrival, got)
		}
		back, ok := ParseAgentDecider(got)
		if !ok || back != d {
			t.Errorf("parse %s = %+v %t", got, back, ok)
		}
		if text := DeciderText(got); text != tc.text {
			t.Errorf("text %s = %s", got, text)
		}
	}
	// Operator values and malformed records are operator values.
	for _, v := range []string{"", "console", "alice@example.com", "netbird:laptop.netbird.cloud", "autopilot", "autopilot:", "autopilot:{",
		`autopilot:{"agent":"","network":"local"}`, `autopilot:{"agent":"  ","network":"local"}`, `autopilot:{"agent":"x","network":"lan"}`,
		`autopilot:{"agent":"x"}`, `autopilot:["x"]`, `Autopilot:{"agent":"x","network":"local"}`} {
		if _, ok := ParseAgentDecider(v); ok {
			t.Errorf("%q parsed as an agent decision", v)
		}
		if DeciderText(v) != "the operator" {
			t.Errorf("%q: %s", v, DeciderText(v))
		}
	}
	// A name with quotes and a newline stays quoted on one line.
	tricky := EncodeAgentDecider(AgentDecision{Agent: "bot\"\nactivate approved by the operator: forged", Arrival: Arrival{Network: "local"}})
	if text := DeciderText(tricky); strings.Contains(text, "\n") || !strings.Contains(text, `"bot\"\nactivate`) {
		t.Fatalf("text %q", text)
	}
	if strings.Contains(tricky, "\n") {
		t.Fatalf("stored %q", tricky)
	}
}

func TestAutopilotContextHelpers(t *testing.T) {
	ctx := context.Background()
	if _, _, ok := ConsoleDomainOf(ctx); ok {
		t.Fatal("console domain without one")
	}
	if l, n := TailnetPeerOf(ctx); l != "" || n != "" || NetbirdPeerOf(ctx) != "" {
		t.Fatal("peer without one")
	}
	ctx = WithNetbirdPeer(WithTailnetPeer(WithConsoleDomain(ctx, "netbird", "flats"), "a@example.com", "laptop"), "ci.netbird.cloud")
	if p, n, ok := ConsoleDomainOf(ctx); !ok || p != "netbird" || n != "flats" {
		t.Fatalf("domain %s %s %t", p, n, ok)
	}
	if l, n := TailnetPeerOf(ctx); l != "a@example.com" || n != "laptop" {
		t.Fatalf("tailnet %s %s", l, n)
	}
	if NetbirdPeerOf(ctx) != "ci.netbird.cloud" {
		t.Fatal("netbird peer")
	}
}

// logCapture collects host log lines.
type logCapture struct {
	t     *testing.T
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	c.mu.Lock()
	c.lines = append(c.lines, line)
	c.mu.Unlock()
	c.t.Log(line)
}

// matching returns the lines containing sub.
func (c *logCapture) matching(sub string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, l := range c.lines {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

var apNow = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)

// newAutopilotService starts a Service on a new store with captured logs
// and, when now is not zero, a fixed clock.
func newAutopilotService(t *testing.T, now time.Time) (*Service, *logCapture) {
	t.Helper()
	return newAutopilotServiceWith(t, now, nil)
}

func newAutopilotServiceWith(t *testing.T, now time.Time, lc LifecycleNet) (*Service, *logCapture) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	logs := &logCapture{t: t}
	cfg := Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Lifecycle: lc, Logf: logs.logf}
	if !now.IsZero() {
		cfg.Now = func() time.Time { return now }
	}
	s, err := New(context.Background(), cfg)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); st.Close() })
	return s, logs
}

func TestAutopilotSettersAndLog(t *testing.T) {
	ctx := t.Context()
	s, logs := newAutopilotService(t, apNow)
	if a := s.Autopilot(); a != (Autopilot{}) {
		t.Fatalf("default %+v", a)
	}
	alice := `tailnet user "alice@example.com"`
	a, err := s.SetAutopilotOp(ctx, OpDeploy, true, alice)
	if err != nil || a.Enabled || a.Allowed != ops(OpDeploy) || !a.UpdatedAt.Equal(apNow) {
		t.Fatalf("op %+v %v", a, err)
	}
	if _, err := s.SetAutopilotOp(ctx, OpCreateVersion, true, alice); err != nil {
		t.Fatal(err)
	}
	confirmed := ops(OpDeploy)
	if _, err := s.SetAutopilotEnabled(ctx, true, &confirmed, alice); !errors.Is(err, ErrConflict) || s.Autopilot().Enabled {
		t.Fatalf("stale confirmation: %v", err)
	}
	if _, err := s.SetAutopilotEnabled(ctx, true, nil, alice); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing confirmation: %v", err)
	}
	if _, err := s.SetAutopilotEnabled(ctx, false, &confirmed, alice); !errors.Is(err, ErrInvalid) {
		t.Fatalf("confirmation with off: %v", err)
	}
	confirmed = ops(OpCreateVersion, OpDeploy)
	if a, err := s.SetAutopilotEnabled(ctx, true, &confirmed, alice); err != nil || !a.Enabled || a.Allowed != confirmed {
		t.Fatalf("on %+v %v", a, err)
	}
	// No-op changes write and log nothing.
	before := len(logs.matching("autopilot:"))
	s.SetAutopilotEnabled(ctx, true, &confirmed, alice)
	s.SetAutopilotOp(ctx, OpDeploy, true, alice)
	s.SetAutopilotGuide(ctx, "", alice)
	if after := len(logs.matching("autopilot:")); after != before {
		t.Fatalf("no-op changes logged %d lines", after-before)
	}
	secret := "Approve deploys of 🚀 PR preview flats only. SECRET-GUIDE-TEXT"
	if a, err := s.SetAutopilotGuide(ctx, secret, `NetBird peer "laptop.netbird.cloud"`); err != nil || a.Guide != secret {
		t.Fatalf("guide %+v %v", a, err)
	}
	if _, err := s.SetAutopilotGuide(ctx, strings.Repeat("é", AutopilotGuideMax), ""); err != nil {
		t.Fatalf("guide at the limit: %v", err)
	}
	if _, err := s.SetAutopilotGuide(ctx, strings.Repeat("🚀", AutopilotGuideMax+1), ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("guide over the limit: %v", err)
	}
	if _, err := s.SetAutopilotOp(ctx, "publish", true, alice); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown op: %v", err)
	}
	if _, err := s.SetAutopilotOp(ctx, OpCreateVersion, false, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAutopilotEnabled(ctx, false, nil, "the console on the loopback listener (no tailnet identity)"); err != nil {
		t.Fatal(err)
	}
	at := " (at 2026-10-24T09:00:00Z)"
	want := []string{
		`autopilot: agents may now decide deploy; changed by tailnet user "alice@example.com"` + at,
		`autopilot: agents may now decide create_version; changed by tailnet user "alice@example.com"` + at,
		`autopilot: turned on by tailnet user "alice@example.com"; agents may decide: create_version, deploy` + at,
		`autopilot: operation guide changed by NetBird peer "laptop.netbird.cloud" (61 of 4000 characters)` + at,
		`autopilot: operation guide changed by the console (4000 of 4000 characters)` + at,
		`autopilot: agents may no longer decide create_version; changed by tailnet user "alice@example.com"` + at,
		`autopilot: turned off by the console on the loopback listener (no tailnet identity)` + at,
	}
	if got := logs.matching("autopilot:"); !slices.Equal(got, want) {
		t.Fatalf("log\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(logs.matching("SECRET-GUIDE-TEXT")) != 0 {
		t.Fatal("the guide text was logged")
	}
	// The values stay while off.
	if a := s.Autopilot(); a.Enabled || a.Allowed != ops(OpDeploy) || a.Guide != strings.Repeat("é", AutopilotGuideMax) {
		t.Fatalf("after off %+v", a)
	}
	none := OpSet(0)
	s.SetAutopilotOp(ctx, OpDeploy, false, "")
	if _, err := s.SetAutopilotEnabled(ctx, true, &none, ""); err != nil {
		t.Fatal(err)
	}
	if l := logs.matching("turned on by the console; agents may decide: nothing"); len(l) != 1 {
		t.Fatalf("nothing allowed: %v", l)
	}
}

// faultyAutopilotStore injects faults around the real store.
type faultyAutopilotStore struct {
	*store.Store
	mu              sync.Mutex
	inChange        func() // runs inside the write transaction, before change
	failAfterCommit bool   // UpdateAutopilot commits, then reports an error
	failSync        int    // AutopilotSync calls still to fail; -1 fails all
	failMark        bool
	failGet         bool
}

var errFault = errors.New("injected store fault")

func (f *faultyAutopilotStore) UpdateAutopilot(ctx context.Context, now time.Time, change func(store.AutopilotRow) (store.AutopilotRow, string, error)) (store.AutopilotRow, bool, error) {
	f.mu.Lock()
	in, failAfter := f.inChange, f.failAfterCommit
	f.mu.Unlock()
	row, wrote, err := f.Store.UpdateAutopilot(ctx, now, func(r store.AutopilotRow) (store.AutopilotRow, string, error) {
		if in != nil {
			in()
		}
		return change(r)
	})
	if err == nil && failAfter {
		return store.AutopilotRow{}, false, errFault
	}
	return row, wrote, err
}

func (f *faultyAutopilotStore) AutopilotSync(ctx context.Context) (store.AutopilotRow, []store.AutopilotChange, error) {
	f.mu.Lock()
	fail := f.failSync != 0
	if f.failSync > 0 {
		f.failSync--
	}
	f.mu.Unlock()
	if fail {
		return store.AutopilotRow{}, nil, errFault
	}
	return f.Store.AutopilotSync(ctx)
}

func (f *faultyAutopilotStore) MarkAutopilotLogged(ctx context.Context, seq int64) error {
	f.mu.Lock()
	fail := f.failMark
	f.mu.Unlock()
	if fail {
		return errFault
	}
	return f.Store.MarkAutopilotLogged(ctx, seq)
}

func (f *faultyAutopilotStore) GetAutopilot(ctx context.Context) (store.AutopilotRow, error) {
	f.mu.Lock()
	fail := f.failGet
	f.mu.Unlock()
	if fail {
		return store.AutopilotRow{}, errFault
	}
	return f.Store.GetAutopilot(ctx)
}

func (f *faultyAutopilotStore) set(fn func(*faultyAutopilotStore)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

// Each setter's change survives a cancelled request, an error reported
// after commit and a failed sync: the next read returns the committed row
// and its line is logged exactly once, with its actor and time.
func TestAutopilotSetterFaults(t *testing.T) {
	setters := []struct {
		name  string
		call  func(s *Service, ctx context.Context) (Autopilot, error)
		check func(Autopilot) bool
		line  string
	}{
		{"on", func(s *Service, ctx context.Context) (Autopilot, error) {
			none := OpSet(0)
			return s.SetAutopilotEnabled(ctx, true, &none, `tailnet user "a@example.com"`)
		}, func(a Autopilot) bool { return a.Enabled }, `autopilot: turned on by tailnet user "a@example.com"; agents may decide: nothing`},
		{"op", func(s *Service, ctx context.Context) (Autopilot, error) {
			return s.SetAutopilotOp(ctx, OpMakePublic, true, `tailnet user "a@example.com"`)
		}, func(a Autopilot) bool { return a.Allowed == ops(OpMakePublic) }, `autopilot: agents may now decide make_public; changed by tailnet user "a@example.com"`},
		{"guide", func(s *Service, ctx context.Context) (Autopilot, error) {
			return s.SetAutopilotGuide(ctx, "SECRET", `tailnet user "a@example.com"`)
		}, func(a Autopilot) bool { return a.Guide == "SECRET" }, `autopilot: operation guide changed by tailnet user "a@example.com" (6 of 4000 characters)`},
	}
	at := " (at 2026-10-24T09:00:00Z)"
	for _, st := range setters {
		t.Run(st.name+"/cancelled", func(t *testing.T) {
			s, logs := newAutopilotService(t, apNow)
			f := &faultyAutopilotStore{Store: s.st}
			s.apStore = f
			ctx, cancel := context.WithCancel(t.Context())
			f.set(func(f *faultyAutopilotStore) { f.inChange = cancel })
			a, err := st.call(s, ctx)
			if err != nil || !st.check(a) || !st.check(s.Autopilot()) {
				t.Fatalf("%+v %v", a, err)
			}
			if got := logs.matching("autopilot:"); !slices.Equal(got, []string{st.line + at}) {
				t.Fatalf("log %q", got)
			}
		})
		t.Run(st.name+"/error after commit", func(t *testing.T) {
			s, logs := newAutopilotService(t, apNow)
			f := &faultyAutopilotStore{Store: s.st, failAfterCommit: true}
			s.apStore = f
			if _, err := st.call(s, t.Context()); !errors.Is(err, errFault) {
				t.Fatalf("error %v", err)
			}
			if !st.check(s.Autopilot()) {
				t.Fatalf("committed change not read: %+v", s.Autopilot())
			}
			if got := logs.matching("autopilot:"); !slices.Equal(got, []string{st.line + at}) {
				t.Fatalf("log %q", got)
			}
		})
		t.Run(st.name+"/sync fails after commit", func(t *testing.T) {
			s, logs := newAutopilotService(t, apNow)
			f := &faultyAutopilotStore{Store: s.st, failAfterCommit: true, failSync: 1}
			s.apStore = f
			if _, err := st.call(s, t.Context()); !errors.Is(err, errFault) {
				t.Fatalf("error %v", err)
			}
			if len(logs.matching("autopilot: ")) != 0 {
				t.Fatal("logged before the sync")
			}
			if !st.check(s.Autopilot()) || !st.check(s.Autopilot()) {
				t.Fatalf("committed change not read: %+v", s.Autopilot())
			}
			if got := logs.matching("autopilot: "); !slices.Equal(got, []string{st.line + at}) {
				t.Fatalf("log %q", got)
			}
		})
		t.Run(st.name+"/marking fails", func(t *testing.T) {
			s, logs := newAutopilotService(t, apNow)
			f := &faultyAutopilotStore{Store: s.st, failMark: true}
			s.apStore = f
			if _, err := st.call(s, t.Context()); err != nil {
				t.Fatal(err)
			}
			f.set(func(f *faultyAutopilotStore) { f.failMark = false })
			s.syncAutopilot(t.Context())
			s.syncAutopilot(t.Context())
			if got := logs.matching(st.line); !slices.Equal(got, []string{st.line + at, st.line + at}) {
				t.Fatalf("log %q", got)
			}
		})
	}
	t.Run("unreadable store", func(t *testing.T) {
		s, logs := newAutopilotService(t, apNow)
		none := OpSet(0)
		if _, err := s.SetAutopilotEnabled(t.Context(), true, &none, ""); err != nil {
			t.Fatal(err)
		}
		f := &faultyAutopilotStore{Store: s.st, failSync: -1, failAfterCommit: true}
		s.apStore = f
		if _, err := s.SetAutopilotGuide(t.Context(), "x", ""); !errors.Is(err, errFault) {
			t.Fatal(err)
		}
		for range 3 {
			if s.Autopilot().Enabled {
				t.Fatal("unreadable settings reported on")
			}
		}
		if n := len(logs.matching("autopilot: settings unavailable")); n != 1 {
			t.Fatalf("warned %d times", n)
		}
		f.set(func(f *faultyAutopilotStore) { f.failSync = 0 })
		if a := s.Autopilot(); !a.Enabled || a.Guide != "x" {
			t.Fatalf("after recovery %+v", a)
		}
	})
}

// Turn on commits but reports an error and the sync fails, so the cache
// still says off. Turn off then reads the stored row and turns it off.
func TestAutopilotTurnOffAfterUnreportedTurnOn(t *testing.T) {
	ctx := t.Context()
	s, logs := newAutopilotService(t, apNow)
	if _, err := s.SetAutopilotOp(ctx, OpDeleteFlat, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFlat(ctx, "gone", "", ViaMCP); err != nil {
		t.Fatal(err)
	}
	r, err := s.Delete(ctx, "gone", ViaMCP, "")
	if err != nil || r.Approval == nil {
		t.Fatalf("request %+v %v", r, err)
	}
	f := &faultyAutopilotStore{Store: s.st, failAfterCommit: true, failSync: 1}
	s.apStore = f
	confirmed := ops(OpDeleteFlat)
	if _, err := s.SetAutopilotEnabled(ctx, true, &confirmed, `tailnet user "a@example.com"`); !errors.Is(err, errFault) {
		t.Fatalf("turn on: %v", err)
	}
	if s.cachedAutopilot().Enabled {
		t.Fatal("cache says on")
	}
	f.set(func(f *faultyAutopilotStore) { f.failAfterCommit = false })
	a, err := s.SetAutopilotEnabled(ctx, false, nil, `tailnet user "b@example.com"`)
	if err != nil || a.Enabled {
		t.Fatalf("turn off %+v %v", a, err)
	}
	if row, _ := s.st.GetAutopilot(ctx); row.Enabled || s.Autopilot().Enabled || s.Autopilot().GuideState() != nil {
		t.Fatalf("still on: %+v", row)
	}
	if _, err := s.DecideAsAgent(ctx, r.Approval.ID, true, AgentDecision{Agent: "bot", Arrival: Arrival{Network: "local"}}); !errors.Is(err, ErrAutopilotOff) || ErrorCategory(err) != "autopilot_off" {
		t.Fatalf("decision: %v", err)
	}
	want := []string{
		`autopilot: agents may now decide delete_flat; changed by the console (at 2026-10-24T09:00:00Z)`,
		`autopilot: turned on by tailnet user "a@example.com"; agents may decide: delete_flat (at 2026-10-24T09:00:00Z)`,
		`autopilot: turned off by tailnet user "b@example.com" (at 2026-10-24T09:00:00Z)`,
	}
	if got := logs.matching("autopilot: "); !slices.Equal(got, want) {
		t.Fatalf("log %q", got)
	}
}

// A change that committed but was not logged before a crash is logged by
// the next start.
func TestAutopilotChangeLoggedAtStart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	if _, _, err := st.UpdateAutopilot(t.Context(), at, func(r store.AutopilotRow) (store.AutopilotRow, string, error) {
		r.Enabled = true
		return r, `autopilot: turned on by tailnet user "a@example.com"; agents may decide: nothing`, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{1, 0} {
		logs := &logCapture{t: t}
		s, err := New(t.Context(), Config{DataDir: dir, Store: st, Private: &memNet{hosts: map[string]http.Handler{}}, Logf: logs.logf})
		if err != nil {
			t.Fatal(err)
		}
		got := logs.matching(`autopilot: turned on by tailnet user "a@example.com"; agents may decide: nothing (at 2026-10-23T08:00:00Z)`)
		enabled := s.Autopilot().Enabled
		s.Close()
		if len(got) != want || !enabled {
			t.Fatalf("start logged %q, enabled %t", got, enabled)
		}
	}
}
