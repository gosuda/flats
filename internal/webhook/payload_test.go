package webhook

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func keysOf(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

func decodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("the body is not a JSON object: %v\n%s", err, raw)
	}
	return m
}

func TestPayloadFieldsPerType(t *testing.T) {
	visibility := &Change{
		From: &Access{Visibility: "private", Access: "view"},
		To:   &Access{Visibility: "public", Access: "edit"},
	}
	cases := []struct {
		name string
		p    Payload
		keys []string
	}{
		{
			name: "approval.created",
			p: Payload{
				ID: "evt_a", Type: "approval.created", At: "2026-11-16T12:34:56.000Z",
				Flat: FlatRef{Slug: "notes", ID: "flt_1"}, Operation: "publish", State: "pending",
				ApprovalID: "apr_1", Via: "mcp", ConsoleURL: "http://127.0.0.1/approvals/apr_1",
			},
			keys: []string{"approval_id", "at", "console_url", "flat", "id", "operation", "state", "type", "via"},
		},
		{
			name: "approval.decided",
			p: Payload{
				ID: "evt_b", Type: "approval.decided", At: "2026-11-16T12:34:56.000Z",
				Flat: FlatRef{Slug: "notes", ID: "flt_1"}, Operation: "delete", State: "approved",
				ApprovalID: "apr_1", DecidedBy: "operator", ConsoleURL: "http://127.0.0.1/approvals/apr_1",
			},
			keys: []string{"approval_id", "at", "console_url", "decided_by", "flat", "id", "operation", "state", "type"},
		},
		{
			name: "approval.decided without decided_by",
			p: Payload{
				ID: "evt_c", Type: "approval.decided", At: "2026-11-16T12:34:56.000Z",
				Flat: FlatRef{Slug: "notes", ID: "flt_1"}, Operation: "publish", State: "failed",
				ApprovalID: "apr_2", ConsoleURL: "http://127.0.0.1/approvals/apr_2",
			},
			keys: []string{"approval_id", "at", "console_url", "flat", "id", "operation", "state", "type"},
		},
		{
			name: "visibility.changed",
			p: Payload{
				ID: "evt_d", Type: "visibility.changed", At: "2026-11-16T12:34:56.000Z",
				Flat: FlatRef{Slug: "notes", ID: "flt_1"}, Operation: "set_visibility", State: "applied",
				ApprovalID: "apr_3", DecidedBy: "autopilot", Change: visibility,
				ConsoleURL: "http://127.0.0.1/flats/notes",
			},
			keys: []string{"approval_id", "at", "change", "console_url", "decided_by", "flat", "id", "operation", "state", "type"},
		},
		{
			name: "network.changed origins",
			p: Payload{
				ID: "evt_e", Type: "network.changed", At: "2026-11-16T12:34:56.000Z",
				Flat: FlatRef{Slug: "notes", ID: "flt_1"}, Operation: "set_network_origins", State: "applied",
				Change:     &Change{Grant: "origins", Origins: []string{"https://api.example.com"}},
				ConsoleURL: "http://127.0.0.1/flats/notes",
			},
			keys: []string{"at", "change", "console_url", "flat", "id", "operation", "state", "type"},
		},
		{
			name: "network.changed provider",
			p: Payload{
				ID: "evt_f", Type: "network.changed", At: "2026-11-16T12:34:56.000Z",
				Flat: FlatRef{Slug: "notes", ID: "flt_1"}, Operation: "set_provider_permission", State: "applied",
				Change:     &Change{Grant: "provider", Provider: "tailscale", Permitted: true},
				ConsoleURL: "http://127.0.0.1/flats/notes",
			},
			keys: []string{"at", "change", "console_url", "flat", "id", "operation", "state", "type"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(tc.p)
			if err != nil {
				t.Fatal(err)
			}
			m := decodeMap(t, raw)
			if got := keysOf(m); !slices.Equal(got, tc.keys) {
				t.Fatalf("fields = %v, want %v\n%s", got, tc.keys, raw)
			}
			flat, _ := m["flat"].(map[string]any)
			if got := keysOf(flat); !slices.Equal(got, []string{"id", "slug"}) {
				t.Errorf("flat fields = %v, want id and slug", got)
			}
		})
	}
}

func TestFlatIDIsPresentWhenEmpty(t *testing.T) {
	t.Parallel()
	p := sampleEvent("evt_x")
	p.Flat = FlatRef{Slug: "gone"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"flat":{"slug":"gone","id":""}`) {
		t.Fatalf("flat = %s, want the id present and empty", raw)
	}
}

func TestChangeShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		c    Change
		want string
	}{
		{
			"visibility",
			Change{From: &Access{"private", "view"}, To: &Access{"public", "edit"}},
			`{"from":{"visibility":"private","access":"view"},"to":{"visibility":"public","access":"edit"}}`,
		},
		{
			"origins",
			Change{Grant: "origins", Count: 2, Origins: []string{"https://a.example", "https://b.example"}},
			`{"grant":"origins","count":2,"origins":["https://a.example","https://b.example"]}`,
		},
		{
			// A list that the caller cut says so and keeps the count of the whole list.
			"origins truncated",
			Change{Grant: "origins", Count: 32, Origins: []string{"https://a.example"}, Truncated: true},
			`{"grant":"origins","count":32,"origins":["https://a.example"],"truncated":true}`,
		},
		{"origins empty", Change{Grant: "origins", Origins: []string{}}, `{"grant":"origins","count":0,"origins":[]}`},
		{"origins nil", Change{Grant: "origins"}, `{"grant":"origins","count":0,"origins":[]}`},
		{
			"provider permitted",
			Change{Grant: "provider", Provider: "tailscale", Permitted: true},
			`{"grant":"provider","provider":"tailscale","permitted":true}`,
		},
		{
			"provider revoked",
			Change{Grant: "provider", Provider: "zrok"},
			`{"grant":"provider","provider":"zrok","permitted":false}`,
		},
		{
			// Fields of the other kind must not leak into a change.
			"origins with stray fields",
			Change{
				Grant: "origins", Count: 1, Origins: []string{"https://a.example"}, Provider: "zrok", Permitted: true,
				From: &Access{"private", "view"}, To: &Access{"public", "view"},
			},
			`{"grant":"origins","count":1,"origins":["https://a.example"]}`,
		},
		{
			"provider with stray fields",
			Change{Grant: "provider", Provider: "zrok", Origins: []string{"x"}, From: &Access{"a", "b"}},
			`{"grant":"provider","provider":"zrok","permitted":false}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Both the value and the pointer marshal the same way.
			for _, v := range []any{tc.c, &tc.c} {
				raw, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				if string(raw) != tc.want {
					t.Errorf("%T marshals as\n%s\nwant\n%s", v, raw, tc.want)
				}
			}
		})
	}
}

// TestNotifyFillsIDAndTime reads the bodies that a receiver gets. It shows the
// text form of the generated ID and time, and that a caller's own values stay.
func TestNotifyFillsIDAndTime(t *testing.T) {
	t.Parallel()
	var capt capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capt.record(r)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	// A clock that is not in UTC and has nanoseconds: the time must come out in
	// UTC, cut to milliseconds, with three digits.
	tokyo := time.FixedZone("JST", 9*3600)
	moments := []time.Time{
		time.Date(2026, 11, 16, 21, 34, 56, 123456789, tokyo),
		time.Date(2026, 11, 16, 12, 34, 56, 0, time.UTC),
		time.Date(2026, 11, 16, 12, 34, 56, 7_000_000, time.UTC),
	}
	wantAt := []string{"2026-11-16T12:34:56.123Z", "2026-11-16T12:34:56.000Z", "2026-11-16T12:34:56.007Z"}

	client, _ := newCountingClient(t)
	var clock settableClock
	h := newHarness(t, Config{Keys: keysFor(srv.URL), Client: client, Waits: []time.Duration{}, Now: clock.now})
	h.start(t.Context())

	for _, m := range moments {
		clock.set(m)
		p := sampleEvent("")
		p.At = ""
		h.sender.Notify(p)
		h.wait(1)
	}
	own := sampleEvent("evt_mine")
	own.At = "2000-01-02T03:04:05.678Z"
	h.sender.Notify(own)
	h.wait(1)

	reqs := capt.all()
	if len(reqs) != 4 {
		t.Fatalf("%d requests, want 4", len(reqs))
	}
	idPattern := regexp.MustCompile(`^evt_[a-z2-7]{26}$`)
	atPattern := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
	seen := map[string]bool{}
	for k := range moments {
		m := decodeMap(t, reqs[k].body)
		id, _ := m["id"].(string)
		if !idPattern.MatchString(id) {
			t.Errorf("generated id %q is not evt_ and 26 lower-case base32 characters", id)
		}
		if seen[id] {
			t.Errorf("id %q was generated twice", id)
		}
		seen[id] = true
		at, _ := m["at"].(string)
		if !atPattern.MatchString(at) {
			t.Errorf("at %q does not have three fraction digits and Z", at)
		}
		if at != wantAt[k] {
			t.Errorf("at = %q, want %q", at, wantAt[k])
		}
	}
	m := decodeMap(t, reqs[3].body)
	if m["id"] != "evt_mine" || m["at"] != "2000-01-02T03:04:05.678Z" {
		t.Errorf("a given id and time were replaced: %v %v", m["id"], m["at"])
	}
}

func TestOversizeBodyIsDropped(t *testing.T) {
	t.Parallel()
	var capt capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capt.record(r)
	}))
	t.Cleanup(srv.Close)
	client, _ := newCountingClient(t)
	h := newHarness(t, Config{Keys: keysFor(srv.URL), Client: client, Waits: []time.Duration{}, Now: fixedClock})

	big := sampleEvent("evt_big")
	big.ConsoleURL = "http://127.0.0.1/" + strings.Repeat("x", maxBody)
	h.sender.Notify(big)
	// Notify writes no log line itself: a worker logs the drop.
	if lines := h.log.snapshot(); len(lines) != 0 {
		t.Fatalf("Notify wrote a log line: %q", lines)
	}

	// A body at the limit is accepted: build one that is a few bytes short.
	small := sampleEvent("evt_fit")
	small.ConsoleURL = "http://127.0.0.1/" + strings.Repeat("x", 3000)
	h.sender.Notify(small)
	h.start(t.Context())
	if got := h.wait(1); got[0] != "evt_fit" {
		t.Fatalf("finished %v, want only evt_fit", got)
	}
	if n := capt.count(); n != 1 {
		t.Errorf("%d attempts, want only the small event", n)
	}
	// The log is read after Run returned: a worker that took the note may not have written its line yet.
	h.stop()
	lines := h.log.snapshot()
	if len(lines) != 1 || !strings.Contains(lines[0], "evt_big") || !strings.Contains(lines[0], "approval.created") {
		t.Fatalf("log = %q, want one line with the id and the type", lines)
	}
	if strings.Contains(lines[0], "xxxx") || strings.Contains(lines[0], "127.0.0.1") {
		t.Errorf("the log line quotes the body: %q", lines[0])
	}
}
