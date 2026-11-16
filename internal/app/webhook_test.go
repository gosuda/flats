package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
	"github.com/gosuda/flats/internal/webhook"
)

// docs/spec/event-webhook.md: the two webhook keys (rules 6 to 9) and the host that sends an event (rules 11, 12 and 17).

const (
	hookToken  = "CANARY-url-token-5c1e"
	hookSecret = "whsec_CANARY-secret-77aa0123456789"
)

// delivery is one request that a test receiver got.
type delivery struct {
	header http.Header
	body   []byte
}

// receiver is an httptest server that hands every request to a channel.
func receiver(t *testing.T) (*httptest.Server, chan delivery) {
	t.Helper()
	got := make(chan delivery, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- delivery{header: r.Header.Clone(), body: b}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// keyCmd runs `flats key` with value on stdin and returns what it printed.
func keyCmd(t *testing.T, config, value string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := KeyCommand(context.Background(), append(args, "--config", config), strings.NewReader(value), &out)
	return out.String(), err
}

// A host with both keys sends a signed event to the stored URL. The key commands print the texts of rule 7, the audit log
// names the keys without their values, and nothing that an agent or the console page can read holds the URL or the secret.
func TestTheHostSendsASignedEventToTheStoredURL(t *testing.T) {
	clearKeyEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	config := initConfig(t, dir)
	srv, got := receiver(t)
	url := srv.URL + "/hook?token=" + hookToken
	db := filepath.Join(dir, "flats.db")

	out, err := keyCmd(t, config, url, "set", "webhook-url")
	if want := "stored the webhook URL in " + db + "; events are sent when both webhook keys are stored\n"; err != nil || out != want {
		t.Fatalf("key set webhook-url = %q, %v; want %q", out, err, want)
	}
	out, err = keyCmd(t, config, hookSecret, "set", "webhook-secret")
	if want := "stored the webhook secret in " + db + "; events are sent when both webhook keys are stored\n"; err != nil || out != want {
		t.Fatalf("key set webhook-secret = %q, %v; want %q", out, err, want)
	}
	out, err = keyCmd(t, config, "", "ls")
	if err != nil || !strings.Contains(out, "webhook-url    set (from cli, ") || !strings.Contains(out, "webhook-secret set (from cli, ") ||
		strings.Contains(out, hookToken) || strings.Contains(out, hookSecret) || strings.Contains(out, "waiting for the next start\nwebhook") {
		t.Fatalf("key ls = %q, %v", out, err)
	}
	var kinds []string
	for _, e := range auditIn(t, dir) {
		kinds = append(kinds, e.Kind+" "+e.Name+" "+e.Via)
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), hookToken) || strings.Contains(string(raw), hookSecret) {
			t.Errorf("an audit row holds a key value: %s", raw)
		}
	}
	if want := "key.set webhook-url command,key.set webhook-secret command"; strings.Join(kinds, ",") != want {
		t.Errorf("audit rows = %v, want %s", kinds, want)
	}

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
	var d delivery
	select {
	case d = <-got:
	case <-time.After(30 * time.Second):
		t.Fatal("no event arrived at the receiver")
	}
	ts, err := strconv.ParseInt(d.header.Get("X-Flats-Timestamp"), 10, 64)
	if err != nil {
		t.Fatalf("X-Flats-Timestamp = %q", d.header.Get("X-Flats-Timestamp"))
	}
	if sig := d.header.Get("X-Flats-Signature"); sig != webhook.Sign(hookSecret, ts, d.body) || !strings.HasPrefix(sig, "v1=") {
		t.Errorf("signature %q does not verify with the stored secret", sig)
	}
	var body struct {
		ID, Type, Operation, State string
		Flat                       struct{ Slug, ID string }
		ConsoleURL                 string `json:"console_url"`
		Change                     struct {
			Grant   string
			Origins []string
		}
	}
	if err := json.Unmarshal(d.body, &body); err != nil {
		t.Fatalf("body %s: %v", d.body, err)
	}
	if body.Type != "network.changed" || body.Operation != "set_network_origins" || body.State != "applied" || body.Flat.Slug != "app" || body.Flat.ID == "" ||
		body.Change.Grant != "origins" || len(body.Change.Origins) != 1 || body.ConsoleURL != strings.TrimSuffix(h.ConsoleURL(), "/")+"/flats/app" ||
		d.header.Get("X-Flats-Event") != "network.changed" || d.header.Get("X-Flats-Event-Id") != body.ID || !strings.HasPrefix(body.ID, "evt_") ||
		d.header.Get("Content-Type") != "application/json" || !strings.HasPrefix(d.header.Get("User-Agent"), "flats/") {
		t.Errorf("event = %s with headers %v", d.body, d.header)
	}

	// The surfaces that an agent or the console page reads hold neither value, and no route is about the webhook.
	client := &http.Client{}
	t.Cleanup(client.CloseIdleConnections)
	base := "http://" + h.Addr()
	for _, path := range []string{"/api/status", "/api/flats", "/api/flats/app", "/api/flats/app/logs", "/api/flats/app/network", "/api/approvals"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("GET %s = %d", path, resp.StatusCode)
		}
		if bytes.Contains(b, []byte(hookToken)) || bytes.Contains(b, []byte(hookSecret)) || bytes.Contains(bytes.ToLower(b), []byte("webhook")) {
			t.Errorf("GET %s mentions the webhook: %s", path, b)
		}
	}
	for _, path := range []string{"/console/api/keys", "/console/api/settings", "/console/api/providers", "/console/api/flats/app/logs"} {
		var v any
		if code := consoleCall(t, h, client, "GET", path, nil, &v); code != 200 {
			t.Errorf("GET %s = %d", path, code)
		}
		b, _ := json.Marshal(v)
		if bytes.Contains(b, []byte(hookToken)) || bytes.Contains(b, []byte(hookSecret)) || bytes.Contains(bytes.ToLower(b), []byte("webhook")) {
			t.Errorf("GET %s mentions the webhook: %s", path, b)
		}
	}
	var keys struct {
		Keys []struct{ ID string }
	}
	if code := consoleCall(t, h, client, "GET", "/console/api/keys", nil, &keys); code != 200 || len(keys.Keys) != 3 {
		t.Errorf("the console lists %d keys (status %d), want the three provider keys: %+v", len(keys.Keys), code, keys)
	}
	for _, path := range []string{"/api/webhook", "/console/api/webhook", "/console/api/keys/webhook-url", "/console/api/keys/webhook-secret", "/api/keys"} {
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			if code := consoleCall(t, h, client, method, path, strings.NewReader(`{"key":"x"}`), nil); code != 404 && code != 405 {
				t.Errorf("%s %s = %d, want no such route", method, path, code)
			}
		}
	}
}

// A host with only one of the two keys logs one line at its start that names the missing key, and sends nothing (rule 6).
func TestOneWebhookKeyLogsOneLineAndSendsNothing(t *testing.T) {
	clearKeyEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	config := initConfig(t, dir)
	srv, got := receiver(t)
	if _, err := keyCmd(t, config, srv.URL+"/hook", "set", "webhook-url"); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	h, err := Start(ctx, configModeOptions(config))
	if err != nil {
		t.Fatal(err)
	}
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
	log.SetOutput(os.Stderr)
	if n := strings.Count(logs.String(), "webhook: the webhook-secret key is not stored"); n != 1 {
		t.Errorf("%d start lines about the missing key, want 1:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), srv.URL) {
		t.Errorf("the log holds the URL:\n%s", logs.String())
	}
	select {
	case d := <-got:
		t.Errorf("an event was sent with one key: %s", d.body)
	default:
	}
}

// A value that rule 8 refuses is not stored, and removing a key prints the text of rule 7.
func TestWebhookKeysAreCheckedAndRemoved(t *testing.T) {
	clearKeyEnv(t)
	dir := t.TempDir()
	config := initConfig(t, dir)
	for name, url := range map[string]string{
		"another scheme":      "ftp://example.com/hook",
		"http to a name":      "http://example.com/hook",
		"http to a public IP": "http://8.8.8.8/hook",
		"a fragment":          "https://example.com/hook#part",
		"too long":            "https://example.com/" + strings.Repeat("a", 2049),
		"no host":             "https:///hook",
	} {
		if out, err := keyCmd(t, config, url, "set", "webhook-url"); err == nil {
			t.Errorf("%s: stored %q: %s", name, url, out)
		}
	}
	if out, err := keyCmd(t, config, "short-secret", "set", "webhook-secret"); err == nil {
		t.Errorf("a secret of 12 bytes was stored: %s", out)
	}
	if out, err := keyCmd(t, config, "", "ls"); err != nil || !strings.Contains(out, "webhook-url    none") || !strings.Contains(out, "webhook-secret none") {
		t.Fatalf("key ls after the refusals = %q, %v", out, err)
	}
	if evs := auditIn(t, dir); len(evs) != 0 {
		t.Errorf("a refused value wrote %d audit rows", len(evs))
	}
	for id, value := range map[string]string{"webhook-url": "http://127.0.0.1:9/hook", "webhook-secret": hookSecret} {
		if _, err := keyCmd(t, config, value, "set", id); err != nil {
			t.Fatalf("key set %s: %v", id, err)
		}
	}
	if out, err := keyCmd(t, config, "", "rm", "webhook-url"); err != nil || out != "removed the webhook URL; the webhook is off\n" {
		t.Errorf("key rm webhook-url = %q, %v", out, err)
	}
	if out, err := keyCmd(t, config, "", "rm", "webhook-secret"); err != nil || out != "removed the webhook secret; the webhook is off\n" {
		t.Errorf("key rm webhook-secret = %q, %v", out, err)
	}
	if _, err := keyCmd(t, config, "x", "set", "webhook"); err == nil || !strings.Contains(err.Error(), "webhook-url or webhook-secret") {
		t.Errorf("an unknown key: %v", err)
	}
	if !strings.Contains(KeyUsage, "webhook-url|webhook-secret") {
		t.Errorf("the usage does not name the webhook keys: %s", KeyUsage)
	}
}

// The sender reads both keys for each event: either key missing is "off", a reset of the sealed values turns the webhook
// off, and a value that does not open is an error that is not "off" (rules 6 and 11).
func TestWebhookCredentialsFollowTheStoredKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	keyFile := core.NewKeyFile(dir, "")
	put := func(id, value string) {
		t.Helper()
		if err := core.PutHostKeyValue(ctx, keyFile, st, id, value, core.KeySourceCLI, time.Now(), store.Audit{Via: store.AuditViaCommand}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := webhookCredentials(ctx, keyFile, st); !errors.Is(err, webhook.ErrOff) {
		t.Fatalf("no key: %v, want ErrOff", err)
	}
	put(core.HostKeyWebhookSecret, hookSecret)
	if _, err := webhookCredentials(ctx, keyFile, st); !errors.Is(err, webhook.ErrOff) {
		t.Fatalf("only the secret: %v, want ErrOff", err)
	}
	put(core.HostKeyWebhookURL, "https://example.com/hook")
	if c, err := webhookCredentials(ctx, keyFile, st); err != nil || c.URL != "https://example.com/hook" || c.Secret != hookSecret {
		t.Fatalf("both keys: %+v, %v", c, err)
	}
	// The key file is gone: the values do not open, and that is not the quiet "off".
	if err := os.Rename(filepath.Join(dir, core.SecretKeyFile), filepath.Join(dir, "moved.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := webhookCredentials(ctx, keyFile, st); err == nil || errors.Is(err, webhook.ErrOff) {
		t.Fatalf("values that do not open: %v, want an error that is not ErrOff", err)
	}
	if err := os.Rename(filepath.Join(dir, "moved.key"), filepath.Join(dir, core.SecretKeyFile)); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSealedValues(ctx, nil, store.Audit{Via: store.AuditViaCommand}); err != nil {
		t.Fatal(err)
	}
	if _, err := webhookCredentials(ctx, keyFile, st); !errors.Is(err, webhook.ErrOff) {
		t.Fatalf("after a reset: %v, want ErrOff", err)
	}
}
