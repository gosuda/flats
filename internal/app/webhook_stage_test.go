package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/webhook"
)

// docs/spec/event-webhook.md: `flats secret reset` treats the two webhook keys as it treats every sealed value (rule 7),
// and the console link of an event that no request caused is the address that Host.ConsoleURL gives (rule 4).
//
// These tests are serial: they start a host, and the first one clears the key variables of the environment.

// hookRecorder is a core.Notifier that keeps the events it is given.
type hookRecorder struct {
	mu  sync.Mutex
	got []webhook.Payload
}

func (r *hookRecorder) Notify(p webhook.Payload) {
	r.mu.Lock()
	r.got = append(r.got, p)
	r.mu.Unlock()
}

// one returns the only event that step made, and forgets it.
func (r *hookRecorder) one(t *testing.T, step string) webhook.Payload {
	t.Helper()
	r.mu.Lock()
	got := r.got
	r.got = nil
	r.mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("%s made %d events, want one: %+v", step, len(got), got)
	}
	return got[0]
}

// `flats secret reset` deletes the two webhook keys with the other sealed values, so the webhook is off after it
// (rules 6 and 7): `flats key ls` shows both keys as none, the sender finds the webhook off, the audit log has a
// key.delete row for each key and one key.reset, and the next host sends nothing.
func TestSecretResetTurnsTheWebhookOff(t *testing.T) {
	clearKeyEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	config := initConfig(t, dir)
	srv, got := receiver(t)
	url := srv.URL + "/hook?token=" + hookToken
	for _, k := range []struct{ id, value string }{{core.HostKeyWebhookURL, url}, {core.HostKeyWebhookSecret, hookSecret}} {
		if _, err := keyCmd(t, config, k.value, "set", k.id); err != nil {
			t.Fatalf("key set %s: %v", k.id, err)
		}
	}
	keyFile := core.NewKeyFile(dir, "")
	// credentials reads the two keys as the sender does for an event, from the database of the stopped host.
	credentials := func() (webhook.Credentials, error) {
		t.Helper()
		st, err := store.Open(filepath.Join(dir, "flats.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		return webhookCredentials(ctx, keyFile, st)
	}
	if c, err := credentials(); err != nil || c.URL != url || c.Secret != hookSecret {
		t.Fatalf("before the reset the keys open to %+v, %v; the test needs a webhook that is on", c, err)
	}
	// While the key opens the values nothing is lost, and reset refuses: the webhook stays on.
	if out, err := runSecretKey(t, config, "reset", "--yes"); exitCode(err) != ExitConfig {
		t.Fatalf("reset with a working key = %q, %v; want a refusal", out, err)
	}
	if _, err := credentials(); err != nil {
		t.Fatalf("a refused reset turned the webhook off: %v", err)
	}

	// The key is lost: reset is allowed.
	if err := os.Remove(filepath.Join(dir, core.SecretKeyFile)); err != nil {
		t.Fatal(err)
	}
	n := len(auditIn(t, dir))
	out := mustRunSecretKey(t, config, "reset", "--yes")
	if !strings.Contains(out, "operator key "+core.HostKeyWebhookURL) || !strings.Contains(out, "operator key "+core.HostKeyWebhookSecret) || !strings.Contains(out, "deleted 2 values") ||
		strings.Contains(out, hookToken) || strings.Contains(out, hookSecret) {
		t.Errorf("reset printed %q; want the names of both webhook keys, the count 2 and no value", out)
	}
	ls, err := keyCmd(t, config, "", "ls")
	if err != nil || !strings.Contains(ls, "webhook-url    none") || !strings.Contains(ls, "webhook-secret none") {
		t.Errorf("key ls after the reset = %q, %v; want both webhook keys as none", ls, err)
	}
	if c, err := credentials(); !errors.Is(err, webhook.ErrOff) {
		t.Errorf("after the reset the sender reads %+v, %v; want webhook.ErrOff", c, err)
	}
	var rows []string
	for _, e := range newAudit(t, dir, n) {
		rows = append(rows, e.Kind+" "+e.Name)
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), hookToken) || strings.Contains(string(raw), hookSecret) {
			t.Errorf("an audit row of the reset holds a key value: %s", raw)
		}
		if e.Kind == store.AuditKeyReset {
			var data map[string]any
			if err := json.Unmarshal(e.Data, &data); err != nil || data["keys"] != float64(2) || data["secrets"] != float64(0) {
				t.Errorf("key.reset data = %s, %v; want 2 keys and 0 secrets", e.Data, err)
			}
		}
	}
	if want := []string{"key.delete webhook-secret", "key.delete webhook-url", "key.reset "}; !slices.Equal(rows, want) {
		t.Errorf("audit rows of the reset = %q, want %q", rows, want)
	}

	// The next host has no webhook: a change that makes an event sends nothing.
	h, err := Start(ctx, configModeOptions(config))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	if _, err := h.Svc.CreateFlat(ctx, "app", "", core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	if err := h.Svc.SetNetworkPolicy(ctx, "app", []string{"https://api.example.com"}, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	// Close waits for the sender, so after it no delivery can still be on its way.
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-got:
		t.Errorf("an event was sent after the reset: %s", d.body)
	default:
	}
}

// An event that no request caused carries the console address that Host.ConsoleURL gives (rule 4). On a host with a
// preferred console domain that is the preferred one and not the first served one. An event of a request through
// another console domain keeps the address of its request, and without a preference the first served domain is used.
func TestAnEventWithoutARequestHasThePreferredConsoleAddress(t *testing.T) {
	h, _, _ := preferredHost(t)
	setPreferred(t, h, "tailscale:two")
	const one, two = "https://one.tail1234.ts.net", "https://two.tail1234.ts.net"
	if got := h.ConsoleURL(); got != two {
		t.Fatalf("ConsoleURL = %q; the test needs a host whose preferred console domain %s is not its first one", got, two)
	}
	rec := &hookRecorder{}
	h.Svc.SetNotifier(rec)
	ctx := context.Background()
	if _, err := h.Svc.SaveVersion(ctx, "app", []bundle.File{{Path: "index.html", Data: []byte("x")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}

	if err := h.Svc.SetNetworkPolicy(ctx, "app", []string{"https://api.example.com"}, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if p := rec.one(t, "a grant without a request"); p.Type != "network.changed" || p.ConsoleURL != two+"/flats/app" {
		t.Errorf("a grant without a request has the link %q, want %s", p.ConsoleURL, two+"/flats/app")
	}
	req, err := h.Svc.Publish(ctx, "app", 0, "", core.ViaAPI)
	if err != nil || req.Approval == nil {
		t.Fatalf("Publish = %+v, %v", req, err)
	}
	if p := rec.one(t, "an approval without a request"); p.Type != "approval.created" || p.ConsoleURL != two+"/approvals/"+req.Approval.ID {
		t.Errorf("an approval without a request has the link %q, want %s", p.ConsoleURL, two+"/approvals/"+req.Approval.ID)
	}

	// A request through the console domain one keeps its own address.
	if err := h.Svc.SetNetworkPolicy(viaDomain("one"), "app", nil, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if p := rec.one(t, "a grant of a request through one"); p.ConsoleURL != one+"/flats/app" {
		t.Errorf("a grant of a request through one has the link %q, want %s", p.ConsoleURL, one+"/flats/app")
	}

	// The loopback address as the preference, and no preference: the link follows Host.ConsoleURL each time.
	setPreferred(t, h, "local")
	local := strings.TrimSuffix(h.ConsoleURL(), "/")
	if local == one || local == two || local == "" {
		t.Fatalf("ConsoleURL with the preference local = %q, want the loopback address", local)
	}
	if err := h.Svc.SetNetworkPolicy(ctx, "app", []string{"https://one.example.com"}, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if p := rec.one(t, "a grant with the preference local"); p.ConsoleURL != local+"/flats/app" {
		t.Errorf("a grant with the preference local has the link %q, want %s", p.ConsoleURL, local+"/flats/app")
	}
	setPreferred(t, h, "")
	if got := h.ConsoleURL(); got != one {
		t.Fatalf("ConsoleURL without a preference = %q, want the first served console domain %s", got, one)
	}
	if err := h.Svc.SetNetworkPolicy(ctx, "app", []string{"https://two.example.com"}, core.ViaConsole); err != nil {
		t.Fatal(err)
	}
	if p := rec.one(t, "a grant without a preference"); p.ConsoleURL != one+"/flats/app" {
		t.Errorf("a grant without a preference has the link %q, want %s", p.ConsoleURL, one+"/flats/app")
	}
}
