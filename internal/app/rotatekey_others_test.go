package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/secret-key-rotation.md: the recovery copy (rule 20), and what the start and the other commands do
// around a rotation (rules 2, 4, 22 to 24 and 27).

// The recovery copy is made current: the recovery password opens the new key afterwards, through
// `flats secret restore-key`, the start does not warn, and the command appends key.backup after key.rotate (rules 20
// and 31). On a terminal the command asks for the password once.
func TestRotateKeyMakesTheRecoveryCopyCurrent(t *testing.T) {
	h := newRotHost(t)
	password := strings.TrimSpace(recoveryPassword)
	pw := passwordFile(t, recoveryPassword)
	mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
	// A copy that the operator keeps apart from the host is not changed.
	apart := filepath.Join(t.TempDir(), "kept.recovery")
	mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw, "--out", apart)
	apartBefore := mustRead(t, apart)
	events := len(auditIn(t, h.dir))

	p := &rotatePrompter{password: password, answer: "rotate"}
	useTerminal(t, p)
	out, err := h.rotate(t)
	if err != nil {
		t.Fatalf("rotate-key: %v\n%s", err, out)
	}
	if p.hiddenAsked != 1 || len(p.lines) != 1 {
		t.Errorf("the command asked %d times for the password and %d times on a line; want once each", p.hiddenAsked, len(p.lines))
	}
	newKey := h.keyAt(t)
	copyPath := filepath.Join(h.dir, core.RecoveryKeyFile)
	if got, err := core.OpenKeyCopy(mustRead(t, copyPath), password); err != nil || !bytes.Equal(got, newKey) {
		t.Fatalf("the recovery password does not open the new key: %v", err)
	}
	if id := strings.TrimSpace(string(mustRead(t, filepath.Join(h.dir, core.RecoveryKeyIDFile)))); id != core.KeyID(newKey) {
		t.Errorf("the ID file holds %s, want %s", id, core.KeyID(newKey))
	}
	for _, f := range []string{copyPath, filepath.Join(h.dir, core.RecoveryKeyIDFile)} {
		if fi, err := os.Stat(f); err != nil || ostest.WrongMode(fi, 0o600) {
			t.Errorf("the mode of %s: %v, %v", f, fi, err)
		}
	}
	if !strings.Contains(out, "wrote "+copyPath+" and its key ID in "+core.RecoveryKeyIDFile+" again") || !strings.Contains(out, "the recovery password opens the new key") {
		t.Errorf("the output does not say what happened to the recovery copy:\n%s", out)
	}
	if what := leaks(out, h.key, newKey); what != "" || strings.Contains(out, password) {
		t.Errorf("the output shows %s or the password:\n%s", what, out)
	}
	if got := mustRead(t, apart); !bytes.Equal(got, apartBefore) {
		t.Error("the rotation changed the copy that is kept apart from the host")
	}
	// key.rotate in the transaction of the re-seal, then key.backup with the ID of the new key and out false.
	got := newAudit(t, h.dir, events)
	if len(got) != 2 || got[0].Kind != store.AuditKeyRotate || got[1].Kind != store.AuditKeyBackup {
		t.Fatalf("the events of the rotation: %q", auditSummary(got))
	}
	if m := eventData(t, got[1]); m["key_id"] != core.KeyID(newKey) || m["out"] != false || len(m) != 2 || got[1].Via != store.AuditViaCommand || got[1].Actor != operatorUser() {
		t.Errorf("key.backup = %+v data %s", got[1], got[1].Data)
	}

	// The start does not warn.
	if logs := mustStart(t, h, rotSealed); strings.Contains(logs, "run `flats secret backup-key`") {
		t.Errorf("the start warns after the rotation:\n%s", logs)
	}
	// The password restores the new key.
	if err := os.Remove(h.keyPath); err != nil {
		t.Fatal(err)
	}
	mustRefuseStart(t, h.config, core.ErrSecretKeyMissing.Error())
	mustRunSecretKey(t, h.config, "restore-key", "--password-file", pw)
	if got := h.keyAt(t); !bytes.Equal(got, newKey) {
		t.Fatal("restore-key did not restore the new key")
	}
	mustStart(t, h, rotSealed)
	mustHoldValues(t, h.dir, newKey)
}

// The command takes the password of the copy as it is: a password shorter than 12 characters opens a copy that the
// operator made by hand (rule 5). A copy that was out of date is made current too (rule 20).
func TestRotateKeyWithACopyMadeByHand(t *testing.T) {
	for name, tc := range map[string]struct {
		password string
		holds    func(h *rotHost) []byte
	}{
		"a short password":             {"short", func(h *rotHost) []byte { return h.key }},
		"a copy of another key":        {"a long recovery password", func(*rotHost) []byte { return bytes.Repeat([]byte{4}, 32) }},
		"a password of 1024 bytes":     {strings.Repeat("p", maxPasswordBytes), func(h *rotHost) []byte { return h.key }},
		"a password with a line end":   {"with a space \r\n", func(h *rotHost) []byte { return h.key }},
		"11 characters in two scripts": {"ééééé한글한글한글", func(h *rotHost) []byte { return h.key }},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRotHost(t)
			password := strings.TrimRight(tc.password, "\r\n")
			data, err := core.SealKeyCopy(tc.holds(h), password, recoveryWorkFactor)
			if err != nil {
				t.Fatal(err)
			}
			copyPath := filepath.Join(h.dir, core.RecoveryKeyFile)
			mustWrite(t, copyPath, data) // made by hand: no ID file
			h.mustRotate(t, "--yes", "--password-file", passwordFile(t, tc.password))
			newKey := h.keyAt(t)
			if got, err := core.OpenKeyCopy(mustRead(t, copyPath), password); err != nil || !bytes.Equal(got, newKey) {
				t.Fatalf("the password does not open the new key: %v", err)
			}
			if w := core.RecoveryCopyWarning(h.dir, newKey); w != "" {
				t.Errorf("the copy after the rotation: %s", w)
			}
		})
	}
}

// secretsOnlyHost is a stopped host with two flat secrets and no operator key, so that a start opens no stored key.
func secretsOnlyHost(t *testing.T) (dir, config string, key []byte) {
	t.Helper()
	clearKeyEnv(t)
	ctx := context.Background()
	dir = t.TempDir()
	config = initConfig(t, dir)
	h, err := Start(ctx, configModeOptions(config))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.SaveVersion(ctx, "app", []bundle.File{{Path: "index.html", Data: []byte("x")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"FIRST", "SECOND"} {
		if err := h.Svc.SetSecret(ctx, "app", name, "value-of-"+name, core.ViaCLI); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, config, mustRead(t, filepath.Join(dir, core.SecretKeyFile))
}

// Rule 22 refuses in two states and in no other. A database with a value that has no key ID starts with a wrong key
// as before this change, and so does one with values of two keys of which one is the key at the key path. Only when
// every value carries a key ID and none carries the ID of the key does the start refuse.
func TestStartChecksTheKeyIDsOnlyWhenEveryValueHasOne(t *testing.T) {
	wrong := bytes.Repeat([]byte{3}, 32)
	t.Run("a value without a key ID", func(t *testing.T) {
		dir, config, _ := secretsOnlyHost(t)
		sqlExec(t, dir, `UPDATE secrets SET key_id='' WHERE name='FIRST'`)
		mustWrite(t, filepath.Join(dir, core.SecretKeyFile), wrong)
		if _, err := startLogs(t, config); err != nil {
			t.Fatalf("the start with a wrong key and a value without a key ID: %v; want a start, as before this change", err)
		}
	})
	t.Run("values of two keys, one of them the key at the key path", func(t *testing.T) {
		dir, config, key := secretsOnlyHost(t)
		sqlExec(t, dir, `UPDATE secrets SET key_id='`+core.KeyID(wrong)+`' WHERE name='FIRST'`)
		if _, err := startLogs(t, config); err != nil {
			t.Fatalf("the start with values of two keys: %v; want a start", err)
		}
		if got := mustRead(t, filepath.Join(dir, core.SecretKeyFile)); !bytes.Equal(got, key) {
			t.Error("the start changed the key")
		}
	})
	t.Run("every value carries the ID of another key", func(t *testing.T) {
		dir, config, key := secretsOnlyHost(t)
		mustWrite(t, filepath.Join(dir, core.SecretKeyFile), wrong)
		err := mustRefuseStart(t, config, core.KeyID(wrong), core.KeyID(key), "flats secret restore-key", "flats secret reset")
		if !errors.Is(err, core.ErrKeySealedNothing) {
			t.Errorf("the refusal is %v", err)
		}
		// `flats key set` refuses with the same message, and stores nothing (rule 22).
		kerr := keySetError(config, core.HostKeyNetbird, rotNetbirdKey)
		refusedWith(t, "flats key set", kerr, core.KeyID(wrong), core.KeyID(key))
		if kerr.Error() != err.Error() {
			t.Errorf("flats key set gives another message than the start:\n %v\n %v", kerr, err)
		}
		st := openStore(t, dir)
		defer st.Close()
		if keys, err := st.ListHostKeys(context.Background()); err != nil || len(keys) != 0 {
			t.Errorf("the refused key set stored %v, %v", keys, err)
		}
	})
}

// Rule 23: the refusal of a missing key names the key ID that every sealed value carries, at the default path and at
// a configured path. A pending key that is left over or a duplicate does not change a start.
func TestStartNamesTheKeyIDOfAMissingKey(t *testing.T) {
	for name, place := range map[string]func(t *testing.T, h *rotHost) *rotHost{
		"the default path":  func(t *testing.T, h *rotHost) *rotHost { return h },
		"a configured path": func(t *testing.T, h *rotHost) *rotHost { return h.outside(t) },
	} {
		t.Run(name, func(t *testing.T) {
			h := place(t, newRotHost(t))
			if err := os.Remove(h.keyPath); err != nil {
				t.Fatal(err)
			}
			mustRefuseStart(t, h.config, "The stored values carry the key ID "+core.KeyID(h.key)+": a key file with that ID opens them")
			// A pending key that is left over does not change the refusal, and the start does not touch it.
			left := bytes.Repeat([]byte{8}, 32)
			mustWrite(t, h.pending(), left)
			mustRefuseStart(t, h.config, "The stored values carry the key ID "+core.KeyID(h.key))
			mustWrite(t, h.keyPath, h.key)
			mustStart(t, h, rotSealed)
			if got := mustRead(t, h.pending()); !bytes.Equal(got, left) {
				t.Error("the start changed a pending key that is left over")
			}
		})
	}
}

// Rule 24: when every sealed value carries one key ID, and the key path holds nothing or a key with another ID,
// `flats secret reset` names that ID before it asks for the confirmation, and says that a key file with that ID
// opens the values.
func TestResetNamesTheKeyIDOfTheValues(t *testing.T) {
	hint := func(h *rotHost) string {
		return "The stored values carry the key ID " + core.KeyID(h.key) + ": a key file with that ID opens them, for example the file that `flats secret rotate-key --out` wrote"
	}
	for name, put := range map[string]func(t *testing.T, h *rotHost){
		"nothing at the key path": func(t *testing.T, h *rotHost) {
			if err := os.Remove(h.keyPath); err != nil {
				t.Fatal(err)
			}
		},
		"a key with another ID": func(t *testing.T, h *rotHost) { mustWrite(t, h.keyPath, bytes.Repeat([]byte{7}, 32)) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newRotHost(t)
			put(t, h)
			rows := rowsText(sealedRows(t, h.dir))
			p := &rotatePrompter{answer: "no"}
			useTerminal(t, p)
			out, err := runSecretKey(t, h.config, "reset")
			if err != nil || !strings.Contains(out, hint(h)) || !strings.Contains(out, "Nothing was deleted") {
				t.Fatalf("reset: %v\n%s", err, out)
			}
			// Before the confirmation: the question came after the sentence was printed.
			if len(p.lines) != 1 || strings.Index(out, hint(h)) > strings.Index(out, "This deletes:") {
				t.Errorf("the ID does not come before the confirmation:\n%s", out)
			}
			if got := rowsText(sealedRows(t, h.dir)); len(got) != len(rows) {
				t.Error("a declined reset deleted values")
			}
		})
	}
	t.Run("values without a key ID", func(t *testing.T) {
		h := newRotHost(t)
		sqlExec(t, h.dir, `UPDATE secrets SET key_id=''`)
		if err := os.Remove(h.keyPath); err != nil {
			t.Fatal(err)
		}
		out := mustRunSecretKey(t, h.config, "reset", "--yes")
		if strings.Contains(out, "carry the key ID") {
			t.Errorf("reset names a key ID although not every value carries one:\n%s", out)
		}
	})
}

// The pending key lies beside the key path that the configuration names. A change of host.secret_key_path while a
// pending key waits hides it, and the start then gives the second refusal of rule 22 (rule 17).
func TestChangingTheKeyPathHidesAPendingKey(t *testing.T) {
	h := newRotHost(t).outside(t)
	pending := stateC(t, h, rotateCheckpointed, "the old key")
	err := mustRefuseStart(t, h.config, "flats secret rotate-key")
	if !errors.Is(err, core.ErrRotationStopped) {
		t.Fatalf("the refusal in state C is %v", err)
	}
	// The setting now names another file with the old key.
	moved := filepath.Join(t.TempDir(), "moved.key")
	mustWrite(t, moved, h.key)
	setPath := func(path string) {
		t.Helper()
		if err := ConfigCommand(context.Background(), []string{"set", "host.secret_key_path", path, "--config", h.config}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	setPath(moved)
	err = mustRefuseStart(t, h.config, core.KeyID(h.key), core.KeyID(pending))
	if !errors.Is(err, core.ErrKeySealedNothing) {
		t.Errorf("the refusal after the change of the setting is %v, want the one of a key that sealed none of the values", err)
	}
	// With the setting back, the command finishes the rotation.
	setPath(h.keyPath)
	h.mustRotate(t)
	mustStart(t, h, rotSealed)
	mustHoldValues(t, h.dir, pending)
}

// When the key path is a link, the file that the link names gets the new key, and the link stays (rule 12).
func TestRotateKeyThroughALink(t *testing.T) {
	for name, place := range map[string]func(t *testing.T, h *rotHost) *rotHost{
		"the default path":  func(t *testing.T, h *rotHost) *rotHost { return h },
		"a configured path": func(t *testing.T, h *rotHost) *rotHost { return h.outside(t) },
	} {
		t.Run(name, func(t *testing.T) {
			h := place(t, newRotHost(t))
			target := filepath.Join(t.TempDir(), "target.key")
			if err := os.Rename(h.keyPath, target); err != nil {
				t.Fatal(err)
			}
			if err := ostest.Symlink(t, target, h.keyPath); err != nil {
				t.Fatal(err)
			}
			h.mustRotate(t, "--yes")
			fi, err := os.Lstat(h.keyPath)
			if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
				t.Fatalf("the key path is no link any more: %v, %v", fi, err)
			}
			newKey := mustRead(t, target)
			if bytes.Equal(newKey, h.key) {
				t.Fatal("the file that the link names holds the old key")
			}
			mustHoldValues(t, h.dir, newKey)
			mustNotBeThere(t, h.pending())
			mustNotBeThere(t, target+".next")
			mustStart(t, h, rotSealed)
		})
	}
}

// The finish of a duplicate does not remove the pending key when its checkpoint fails: an operator can have made
// the duplicate by hand before the re-seal was durable (rule 19).
func TestRotateKeyFinishOfADuplicateNeedsItsCheckpoint(t *testing.T) {
	h := newRotHost(t)
	stopRotation(t, h, rotateKeyWritten, "--yes")
	newKey := h.keyAt(t)
	busy := errors.New("checkpoint database: another connection kept it busy")
	_, err := rotateWith(t, h.config, failAt(rotateCheckpoint, busy))
	failedWith(t, "the finish of a duplicate", err, "the pending key "+h.pending()+" was not removed", busy.Error(), "nothing was changed")
	if got := mustRead(t, h.pending()); !bytes.Equal(got, newKey) {
		t.Fatal("the pending key was removed although the checkpoint failed")
	}
	if got := h.keyAt(t); !bytes.Equal(got, newKey) {
		t.Fatal("the key file changed")
	}
	h.mustRotate(t)
	mustNotBeThere(t, h.pending())
}

// Each writer of a sealed value writes the key ID of the host key in the same row (rule 2): a set of a flat secret
// from the console and from the CLI, `flats key set`, the console, an input of the start, and the import of a
// legacy host.
func TestEveryWriterLabelsItsValueWithTheHostKey(t *testing.T) {
	clearKeyEnv(t)
	ctx := context.Background()
	labels := func(t *testing.T, dir string) map[string]string {
		t.Helper()
		got := map[string]string{}
		for _, r := range sealedRows(t, dir) {
			got[r.name] = r.keyID
		}
		return got
	}
	t.Run("a flat secret, the console and flats key set", func(t *testing.T) {
		dir := t.TempDir()
		config := initConfig(t, dir)
		// `flats key set` stores the first value, and creates the key.
		if err := keySetError(config, core.HostKeyNetbird, rotNetbirdKey); err != nil {
			t.Fatal(err)
		}
		id := core.KeyID(mustRead(t, filepath.Join(dir, core.SecretKeyFile)))
		h, err := Start(ctx, configModeOptions(config))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Svc.SaveVersion(ctx, "app", []bundle.File{{Path: "index.html", Data: []byte("x")}}, core.SaveMeta{}, core.ViaAPI); err != nil {
			t.Fatal(err)
		}
		if err := h.Svc.SetSecret(ctx, "app", "FROM_CLI", "v", core.ViaCLI); err != nil {
			t.Fatal(err)
		}
		if err := h.Svc.SetSecret(ctx, "app", "FROM_CONSOLE", "v", core.ViaConsole); err != nil {
			t.Fatal(err)
		}
		if err := h.SetTailscaleKey(ctx, keyA); err != nil { // the console's route
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"app/FROM_CLI": id, "app/FROM_CONSOLE": id, "key:netbird": id, "key:tailscale": id}
		if got := labels(t, dir); !mapsEqual(got, want) {
			t.Errorf("the key IDs are %v, want %v", got, want)
		}
	})
	t.Run("an input of the start", func(t *testing.T) {
		dir := t.TempDir()
		st := openStore(t, dir)
		in := keyInput{value: keyA, source: core.KeySourceEnv, from: "FLATS_TAILSCALE_AUTHKEY"}
		_, _, err := resolveTailscaleKey(ctx, core.NewKeyFile(dir, ""), st, in, time.Now(), func(string, ...any) {})
		st.Close()
		if err != nil {
			t.Fatal(err)
		}
		id := core.KeyID(mustRead(t, filepath.Join(dir, core.SecretKeyFile)))
		if got := labels(t, dir); !mapsEqual(got, map[string]string{"key:tailscale": id}) {
			t.Errorf("the key IDs are %v, want %s", got, id)
		}
	})
	t.Run("the import of a legacy host", func(t *testing.T) {
		dir := t.TempDir()
		st := openStore(t, dir)
		err := importLegacyKey(ctx, dir, st, &legacyPlan{keyFile: writeKey(t, keyA), keyFlag: "authkey-file"})
		st.Close()
		if err != nil {
			t.Fatal(err)
		}
		id := core.KeyID(mustRead(t, filepath.Join(dir, core.SecretKeyFile)))
		if got := labels(t, dir); !mapsEqual(got, map[string]string{"key:tailscale": id}) {
			t.Errorf("the key IDs are %v, want %s", got, id)
		}
	})
}

// The operator sees key IDs in the output of the key commands, in the audit log and in secret.key.recovery.id. No
// route, no agent tool and not `flats key ls` gives one (rule 4). The name of the test holds no word that the test
// looks for, because the status route shows the data directory, whose temporary path holds that name.
func TestNoRouteNoAgentToolAndNotKeyLsShowWhichKeySealedAValue(t *testing.T) {
	ctx := context.Background()
	h := newRotHost(t)
	pw := passwordFile(t, recoveryPassword)
	mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
	h.mustRotate(t, "--yes", "--password-file", pw)
	ids := []string{core.KeyID(h.key), core.KeyID(h.keyAt(t))}
	// The log holds both IDs, so a surface that showed the log would show them.
	if evs := eventsOf(t, h.dir, store.AuditKeyRotate); len(evs) != 1 || !strings.Contains(string(evs[0].Data), ids[0]) || !strings.Contains(string(evs[0].Data), ids[1]) {
		t.Fatalf("key.rotate does not hold the key IDs: %v", auditSummary(evs))
	}
	shows := func(what, text string) {
		t.Helper()
		for _, bad := range append(ids, "key_id") { // the two IDs, and the name of the field that holds one
			if strings.Contains(text, bad) {
				t.Errorf("%s shows %q:\n%s", what, bad, text)
			}
		}
	}

	var out bytes.Buffer
	if err := KeyCommand(ctx, []string{"ls", "--config", h.config}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "tailscale") {
		t.Fatalf("flats key ls:\n%s", out.String())
	}
	shows("flats key ls", out.String())

	host, err := Start(ctx, configModeOptions(h.config))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	base := "http://" + host.Addr()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for _, path := range []string{
		"/api/status", "/api/flats", "/api/flats/app", "/api/flats/app/secrets", "/api/flats/site/secrets", "/api/flats/app/env",
		"/api/flats/app/logs", "/api/flats/app/versions", "/console/api/keys", "/console/api/settings", "/console/api/providers",
	} {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("X-Flats-Console", "1")
		req.Header.Set("Origin", base)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d: %s", path, resp.StatusCode, body)
			continue
		}
		shows("GET "+path, string(body))
	}

	// The agent tools: none reaches the command, and none of those that read a flat shows a key ID.
	session, err := mcp.NewClient(&mcp.Implementation{Name: "rotation-test", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if name := strings.ToLower(tool.Name); strings.Contains(name, "rotate") || strings.Contains(name, "key") {
			t.Errorf("the tool %q reaches the host key", tool.Name)
		}
		desc, _ := json.Marshal(tool)
		shows("the definition of the tool "+tool.Name, string(desc))
	}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"list_flats", map[string]any{}},
		{"get_flat", map[string]any{"slug": "app"}},
		{"list_secrets", map[string]any{"slug": "app"}},
		{"list_secrets", map[string]any{"slug": "site"}},
		{"list_env", map[string]any{"slug": "app"}},
		{"get_logs", map[string]any{"slug": "app", "limit": 500}},
	} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v", call.name, err, res)
		}
		raw, _ := json.Marshal(res)
		shows("the tool "+call.name, string(raw))
	}
}

// Bootstrapping a new host treats secret.key.next as Flats data, like secret.key: it refuses a directory that holds
// one and no database, and writes nothing (rule 27).
func TestBootstrapTreatsAPendingKeyAsFlatsData(t *testing.T) {
	dir := t.TempDir()
	markDataDir(t, dir)
	mustWrite(t, filepath.Join(dir, core.PendingKeyFile), bytes.Repeat([]byte{1}, 32))
	h, err := Start(context.Background(), configModeOptions(filepath.Join(dir, "config.json")))
	if err == nil {
		h.Close()
		t.Fatal("a new host started beside a pending key")
	}
	if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "already holds Flats data") || !strings.Contains(err.Error(), core.PendingKeyFile) {
		t.Fatalf("the refusal: %v (status %d)", err, exitCode(err))
	}
	for _, f := range []string{"config.json", "flats.db", core.SecretKeyFile} {
		mustNotBeThere(t, filepath.Join(dir, f))
	}
}

// Every refusal of a rotation that stopped exits with status 78 at the start, like the other refusals of the key.
func TestRotationRefusalsAreActionErrors(t *testing.T) {
	for _, refusal := range []error{core.ErrRotationStopped, core.ErrKeySealedNothing, core.ErrPendingKey} {
		wrapped := errors.Join(errors.New("context"), refusal)
		if got := keyRefusal(wrapped); exitCode(got) != ExitConfig || !errors.Is(got, refusal) {
			t.Errorf("%v: exit status %d, want %d", refusal, exitCode(got), ExitConfig)
		}
		if got := rotationError(wrapped); exitCode(got) != ExitConfig || !errors.Is(got, refusal) {
			t.Errorf("%v from a command: exit status %d, want %d", refusal, exitCode(got), ExitConfig)
		}
	}
	other := errors.New("disk full")
	if got := rotationError(other); got != other {
		t.Errorf("another error changed: %v", got)
	}
}

// `flats secret backup-key` reads the key IDs without the host lock and without a migration. When flats.db is not of
// the schema of the binary, it makes no check of the labels, and it still stops on a pending key file that cannot be
// classified (rule 25).
func TestBackupKeyChecksNoLabelsOfAnotherSchema(t *testing.T) {
	h := newRotHost(t)
	pw := passwordFile(t, recoveryPassword)
	// State G: the key at the key path sealed none of the values, so backup-key refuses.
	file := filepath.Join(t.TempDir(), "new.key")
	h.mustRotate(t, "--yes", "--out", file)
	_, err := runSecretKey(t, h.config, "backup-key", "--password-file", pw)
	refusedWith(t, "backup-key in state G", err, core.KeyID(h.key))
	mustNotBeThere(t, filepath.Join(h.dir, core.RecoveryKeyFile))

	// The same host with a database of another schema version: no check of the labels. The copy is written, and the
	// command fails only at its audit event, as before this change.
	sqlExec(t, h.dir, `PRAGMA user_version = 26`)
	_, err = runSecretKey(t, h.config, "backup-key", "--password-file", pw)
	failedWith(t, "backup-key beside a database of another schema", err, "was written", "was not recorded")
	if got, err := core.OpenKeyCopy(mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile)), strings.TrimSpace(recoveryPassword)); err != nil || !bytes.Equal(got, h.key) {
		t.Errorf("the copy does not hold the key at the key path: %v", err)
	}
	if info, err := store.Inspect(filepath.Join(h.dir, "flats.db")); err != nil || info.Version != 26 {
		t.Errorf("backup-key migrated the database: %+v, %v", info, err)
	}
	// A pending key file that cannot be classified still stops it.
	if err := os.Mkdir(h.pending(), 0o700); err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile))
	_, err = runSecretKey(t, h.config, "backup-key", "--password-file", pw)
	refusedWith(t, "backup-key with a folder at the pending name", err, h.pending(), "move it away")
	if got := mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile)); !bytes.Equal(got, before) {
		t.Error("the refused backup-key replaced the copy")
	}
}
