package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/secret-key-rotation.md, rule 18: the states that a stop of the command leaves, by their letter. Each
// test stops the command at a point through rotateHook and shows which file holds the key that opens every value,
// what the start does, and what the next `rotate-key` does.

// stopRotation runs the command and stops it at point.
func stopRotation(t *testing.T, h *rotHost, point string, args ...string) string {
	t.Helper()
	out, err := rotateWith(t, h.config, stopAt(point), args...)
	if !errors.Is(err, errStopped) {
		t.Fatalf("the command did not stop at %s: %v\n%s", point, err, out)
	}
	return out
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustNotBeThere(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is there: %v", path, err)
	}
}

// keySetError runs `flats key set` for id with value on the standard input.
func keySetError(config, id, value string) error {
	return KeyCommand(context.Background(), []string{"set", id, "--config", config}, strings.NewReader(value), &bytes.Buffer{})
}

// refusedWith fails the test unless err is a refusal (status 78) whose message holds each of want.
func refusedWith(t *testing.T, what string, err error, want ...string) {
	t.Helper()
	if err == nil || exitCode(err) != ExitConfig {
		t.Fatalf("%s: %v (status %d), want a refusal", what, err, exitCode(err))
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("%s: the refusal lacks %q: %v", what, w, err)
		}
	}
}

// A stop after step 1 and before the commit, and a stop inside step 1, leave state B: the key path holds the old
// key, which opens every value, and a pending key is left over. The start starts. The next run starts a new
// rotation and removes the pending key first, and its confirmation and its output say so (rules 10 and 21).
func TestRotateKeyStateB(t *testing.T) {
	stops := map[string]func(t *testing.T, h *rotHost){
		"after step 1": func(t *testing.T, h *rotHost) {
			stopRotation(t, h, rotateNewKeyWritten, "--yes")
			if got := mustRead(t, h.pending()); len(got) != 32 || bytes.Equal(got, h.key) {
				t.Fatalf("the pending key holds %d bytes", len(got))
			}
		},
		"inside step 1": func(t *testing.T, h *rotHost) {
			// The stop comes while the file is written: it exists and is not complete.
			out, err := rotateWith(t, h.config, func(p string) error {
				if p != rotateWriteNewKey {
					return nil
				}
				mustWrite(t, h.pending(), []byte("half a key"))
				return errStopped
			}, "--yes")
			if !errors.Is(err, errStopped) {
				t.Fatalf("the command did not stop inside step 1: %v\n%s", err, out)
			}
		},
	}
	for name, stop := range stops {
		t.Run(name, func(t *testing.T) {
			h := newRotHost(t)
			before := rowsText(sealedRows(t, h.dir))
			stop(t, h)
			left := mustRead(t, h.pending())

			// The key path holds the key of every value, and no value changed.
			mustHoldValues(t, h.dir, h.key)
			if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, before) {
				t.Error("a stop before the commit changed a row")
			}
			if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 0 {
				t.Errorf("%d key.rotate events after a stop before the commit", n)
			}
			mustStart(t, h, rotSealed)
			// A copy is made as in state A, and it holds no pending key.
			copyDir := filepath.Join(t.TempDir(), "copy")
			if text, err := runBackup(t, "--out", copyDir, "--config", h.config); err != nil {
				t.Fatalf("flats backup in state B: %v\n%s", err, text)
			}
			mustNotBeThere(t, filepath.Join(copyDir, "data", core.PendingKeyFile))
			if m := mustRead(t, filepath.Join(copyDir, "manifest.json")); !strings.Contains(string(m), `"data/secret.key.next"`) {
				t.Errorf("the manifest does not list the pending key as excluded:\n%s", m)
			}

			// A refusal of the next run leaves the pending key where it is (rule 21).
			if _, err := h.rotate(t); exitCode(err) != 2 {
				t.Fatalf("without --yes: %v", err)
			}
			if got := mustRead(t, h.pending()); !bytes.Equal(got, left) {
				t.Fatal("a refused run changed the pending key that was left over")
			}
			// The confirmation names the file (rule 10), and a declined one leaves it too.
			p := &rotatePrompter{answer: "no"}
			useTerminal(t, p)
			out, err := h.rotate(t)
			if err != nil || !strings.Contains(out, h.pending()+" is left over from a rotation that stopped before it changed a value") ||
				!strings.Contains(out, "This rotation removes it") {
				t.Fatalf("the confirmation with a pending key that is left over: %v\n%s", err, out)
			}
			if got := mustRead(t, h.pending()); !bytes.Equal(got, left) {
				t.Fatal("a declined run changed the pending key that was left over")
			}

			// A new rotation: it removes the file first, and says so.
			p.answer = "rotate"
			out, err = h.rotate(t)
			if err != nil || !strings.Contains(out, "removed "+h.pending()+", which was left over") {
				t.Fatalf("the next run: %v\n%s", err, out)
			}
			newKey := h.keyAt(t)
			if bytes.Equal(newKey, h.key) || bytes.Equal(newKey, left) {
				t.Fatal("the next run made no new key")
			}
			mustNotBeThere(t, h.pending())
			mustHoldValues(t, h.dir, newKey)
			if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 1 {
				t.Errorf("%d key.rotate events after the new rotation", n)
			}
			mustStart(t, h, rotSealed)
		})
	}
}

// stateC puts the host in state C: the values are under the new key, which the pending key holds, and the key path
// does not hold it. keyPath says what the key path holds then. It returns the key of the pending file.
func stateC(t *testing.T, h *rotHost, point, keyPath string) []byte {
	t.Helper()
	stopRotation(t, h, point, "--yes")
	switch keyPath {
	case "the old key":
	case "nothing":
		if err := os.Remove(h.keyPath); err != nil {
			t.Fatal(err)
		}
	case "a damaged file":
		mustWrite(t, h.keyPath, []byte("a stop in step 3 damaged this file"))
	default:
		t.Fatalf("unknown content of the key path %q", keyPath)
	}
	pending := mustRead(t, h.pending())
	if len(pending) != 32 || bytes.Equal(pending, h.key) {
		t.Fatalf("the pending key holds %d bytes", len(pending))
	}
	return pending
}

// A stop after the commit and before the write of step 3 ends leaves state C, also when the checkpoint did not run
// yet: the pending key waits and holds the key that opens every value. The start, `flats key set`,
// `flats secret backup-key` and `flats backup --out` refuse and name the command, `flats secret reset` refuses, and
// the next run finishes the rotation: it makes no new key, asks no confirmation and writes no second event.
func TestRotateKeyStateC(t *testing.T) {
	for _, point := range []string{rotateCommitted, rotateCheckpointed} {
		for _, at := range []string{"the old key", "nothing", "a damaged file"} {
			t.Run(point+" with "+at+" at the key path", func(t *testing.T) {
				h := newRotHost(t)
				pending := stateC(t, h, point, at)
				mustHoldValues(t, h.dir, pending)
				if _, failed := openedWith(t, h.dir, h.key); len(failed) != rotSealed {
					t.Fatalf("the old key still opens a value: only %v failed", failed)
				}
				rows := rowsText(sealedRows(t, h.dir))
				events := len(auditIn(t, h.dir))
				keyBefore, _ := os.ReadFile(h.keyPath)

				// The start refuses and names the command, whatever the key path holds (rule 22).
				mustRefuseStart(t, h.config, "flats secret rotate-key", h.pending(), core.KeyID(pending))
				refusedWith(t, "flats key set", keySetError(h.config, core.HostKeyNetbird, "8f3c2a1e-4b5d-4c6e-9f70-0123456789ab"), "flats secret rotate-key")
				_, err := runSecretKey(t, h.config, "backup-key", "--password-file", passwordFile(t, recoveryPassword))
				refusedWith(t, "flats secret backup-key", err, "flats secret rotate-key")
				_, err = runSecretKey(t, h.config, "reset", "--yes")
				refusedWith(t, "flats secret reset", err, "flats secret rotate-key")
				copies := t.TempDir()
				for _, args := range [][]string{{"--out", filepath.Join(copies, "a")}, {"--out", filepath.Join(copies, "b"), "--exclude-secret-key"}} {
					_, err = runBackup(t, append(args, "--config", h.config)...)
					refusedWith(t, "flats backup "+strings.Join(args[2:], " "), err, "flats secret rotate-key")
				}
				if got := listed(t, copies); len(got) != 0 {
					t.Errorf("a refused backup wrote %v", got)
				}
				// None of them changed a row, a key file or the log, and none wrote a recovery copy.
				if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, rows) {
					t.Error("a refused command changed a row")
				}
				if n := len(auditIn(t, h.dir)); n != events {
					t.Errorf("the refused commands wrote %d events", n-events)
				}
				if got := mustRead(t, h.pending()); !bytes.Equal(got, pending) {
					t.Error("a refused command changed the pending key")
				}
				if got, _ := os.ReadFile(h.keyPath); !bytes.Equal(got, keyBefore) {
					t.Error("a refused command changed the key path")
				}
				mustNotBeThere(t, filepath.Join(h.dir, core.RecoveryKeyFile))

				// --out is refused while a pending key waits (rule 19), and writes nothing.
				outFile := filepath.Join(t.TempDir(), "new.key")
				_, err = h.rotate(t, "--yes", "--out", outFile)
				refusedWith(t, "rotate-key --out", err, "without --out")
				mustNotBeThere(t, outFile)

				// The next run finishes: no --yes, no terminal, no new key, no second event.
				out, err := h.rotate(t)
				if err != nil {
					t.Fatalf("the finish: %v\n%s", err, out)
				}
				if !strings.Contains(out, "finishing a rotation that had stopped") {
					t.Errorf("the output of the finish:\n%s", out)
				}
				if got := h.keyAt(t); !bytes.Equal(got, pending) {
					t.Fatal("the key file does not hold the pending key after the finish")
				}
				mustNotBeThere(t, h.pending())
				if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 1 {
					t.Errorf("%d key.rotate events after the finish, want the one of the first run", n)
				}
				if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, rows) {
					t.Error("the finish changed a row")
				}
				if at == "nothing" {
					for _, want := range []string{"created " + h.keyPath, "mode 0600", "belongs to the user that ran this command", "the owner and the access that the host needs"} {
						if !strings.Contains(out, want) {
							t.Errorf("the output of a finish that created the key file lacks %q:\n%s", want, out)
						}
					}
					if u := operatorUser(); u != "" && !strings.Contains(out, u) {
						t.Errorf("the output does not name the owner %s:\n%s", u, out)
					}
				}
				if what := leaks(out, h.key, pending); what != "" {
					t.Errorf("the output of the finish shows %s:\n%s", what, out)
				}
				mustStart(t, h, rotSealed)
				mustHoldValues(t, h.dir, pending)
			})
		}
	}
}

// In state C the start refuses before it reads an input for an operator key and before it opens or stores one, so
// host_keys does not change (rule 22).
func TestStartInStateCLeavesTheOperatorKeysAlone(t *testing.T) {
	t.Run("with an input for an operator key", func(t *testing.T) {
		h := newRotHost(t)
		// No Tailscale key is stored, so a start would store the input, sealed under the old key.
		if err := KeyCommand(context.Background(), []string{"rm", core.HostKeyTailscale, "--config", h.config}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		stopRotation(t, h, rotateCheckpointed, "--yes")
		rows := rowsText(sealedRows(t, h.dir))
		events := len(auditIn(t, h.dir))
		t.Setenv("FLATS_TAILSCALE_AUTHKEY", keyB)
		t.Setenv("FLATS_NETBIRD_SETUP_KEY", "8f3c2a1e-4b5d-4c6e-9f70-0123456789ab")
		mustRefuseStart(t, h.config, "flats secret rotate-key")
		if os.Getenv("FLATS_TAILSCALE_AUTHKEY") != keyB {
			t.Error("the start read the input for the Tailscale key before it refused")
		}
		// The same with a flag that names a file.
		o := configModeOptions(h.config)
		o.TailscaleKeyFile, o.Set = writeKey(t, keyB), map[string]bool{"tailscale-authkey-file": true}
		if host, err := Start(context.Background(), o); err == nil {
			host.Close()
			t.Fatal("the host started")
		} else if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "flats secret rotate-key") {
			t.Fatalf("the start with --tailscale-authkey-file: %v", err)
		}
		if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, rows) {
			t.Errorf("the refused start changed host_keys:\n%q\nwant\n%q", got, rows)
		}
		if n := len(auditIn(t, h.dir)); n != events {
			t.Errorf("the refused start wrote %d events", n-events)
		}
	})
	t.Run("with a stored Tailscale key", func(t *testing.T) {
		h := newRotHost(t) // holds a stored Tailscale key, which the start opens after the check of the key
		pending := stateC(t, h, rotateCheckpointed, "the old key")
		rows := rowsText(sealedRows(t, h.dir))
		events := len(auditIn(t, h.dir))
		err := mustRefuseStart(t, h.config, "flats secret rotate-key")
		if !errors.Is(err, core.ErrRotationStopped) {
			t.Errorf("the refusal is %v, want the one of a rotation that stopped and not the one of a key that does not open", err)
		}
		if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, rows) {
			t.Error("the refused start changed host_keys")
		}
		if n := len(auditIn(t, h.dir)); n != events {
			t.Errorf("the refused start wrote %d events", n-events)
		}
		h.mustRotate(t)
		mustStart(t, h, rotSealed)
		mustHoldValues(t, h.dir, pending)
	})
}

// A stop after the write of step 3 and before the removal of the pending key leaves state D: the key path holds the
// new key, and the pending key is a duplicate. The start starts, and warns while the recovery copy holds the old
// key. The next run removes the pending key, writes the recovery copy and makes no new key. --out is refused, and
// `flats secret reset` refuses as before, because the key opens every value.
func TestRotateKeyStateD(t *testing.T) {
	for name, hook := range map[string]func(string) error{
		"after the write of the key file":  stopAt(rotateKeyWritten),
		"inside the write of the key file": stopAt(rotateKeyWriting), // written and synced, and not read again
	} {
		t.Run(name, func(t *testing.T) {
			h := newRotHost(t)
			pw := passwordFile(t, recoveryPassword)
			mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
			out, err := rotateWith(t, h.config, hook, "--yes", "--password-file", pw)
			if !errors.Is(err, errStopped) {
				t.Fatalf("the command did not stop: %v\n%s", err, out)
			}
			newKey := h.keyAt(t)
			if bytes.Equal(newKey, h.key) || !bytes.Equal(mustRead(t, h.pending()), newKey) {
				t.Fatal("the key path and the pending key do not hold the same new key")
			}
			mustHoldValues(t, h.dir, newKey)

			// The start starts, and warns about the recovery copy, which still holds the old key.
			if logs := mustStart(t, h, rotSealed); !strings.Contains(logs, "run `flats secret backup-key`") {
				t.Errorf("the start does not warn about the recovery copy of the old key:\n%s", logs)
			}
			rows := rowsText(sealedRows(t, h.dir)) // after the start, so that only the command can change a row from here
			// A copy is made as in state E, and it holds no pending key.
			copyDir := filepath.Join(t.TempDir(), "copy")
			if text, err := runBackup(t, "--out", copyDir, "--config", h.config); err != nil {
				t.Fatalf("flats backup in state D: %v\n%s", err, text)
			}
			mustNotBeThere(t, filepath.Join(copyDir, "data", core.PendingKeyFile))
			if got := mustRead(t, filepath.Join(copyDir, "data", core.SecretKeyFile)); !bytes.Equal(got, newKey) {
				t.Error("the copy does not hold the new key")
			}
			excluded := filepath.Join(t.TempDir(), "copy")
			if text, err := runBackup(t, "--out", excluded, "--exclude-secret-key", "--config", h.config); err != nil {
				t.Fatalf("flats backup --exclude-secret-key in state D: %v\n%s", err, text)
			}
			mustNotBeThere(t, filepath.Join(excluded, "data", core.PendingKeyFile))

			// --out is refused while the pending key is a duplicate, and reset refuses as before.
			outFile := filepath.Join(t.TempDir(), "new.key")
			_, err = h.rotate(t, "--yes", "--out", outFile, "--password-file", pw)
			refusedWith(t, "rotate-key --out in state D", err, "without --out")
			mustNotBeThere(t, outFile)
			_, err = runSecretKey(t, h.config, "reset", "--yes")
			refusedWith(t, "flats secret reset in state D", err, "nothing is lost")
			if got := mustRead(t, h.pending()); !bytes.Equal(got, newKey) {
				t.Fatal("a refused command changed the pending key")
			}

			// The next run removes the pending key and makes no new key.
			out, err = h.rotate(t, "--password-file", pw)
			if err != nil || !strings.Contains(out, "finishing a rotation that had stopped") || !strings.Contains(out, "removed the pending key "+h.pending()) {
				t.Fatalf("the finish: %v\n%s", err, out)
			}
			mustNotBeThere(t, h.pending())
			if got := h.keyAt(t); !bytes.Equal(got, newKey) {
				t.Fatal("the finish changed the key file")
			}
			if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, rows) {
				t.Error("the finish changed a row")
			}
			if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 1 {
				t.Errorf("%d key.rotate events after the finish", n)
			}
			// Step 4 ran: the recovery copy holds the new key, and the start is silent.
			if w := core.RecoveryCopyWarning(h.dir, newKey); w != "" {
				t.Errorf("the recovery copy after the finish: %s", w)
			}
			if logs := mustStart(t, h, rotSealed); strings.Contains(logs, "run `flats secret backup-key`") {
				t.Errorf("the start still warns after the finish:\n%s", logs)
			}
		})
	}
}

// A stop after step 3 leaves state E: the new key is in place and no pending key is there. The start starts, and
// warns while the recovery copy holds the old key. The next run is a new rotation, which makes the copy current.
func TestRotateKeyStateE(t *testing.T) {
	for _, point := range []string{rotatePendingRemoved, rotateCopyWritten} {
		t.Run(point, func(t *testing.T) {
			h := newRotHost(t)
			pw := passwordFile(t, recoveryPassword)
			mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
			stopRotation(t, h, point, "--yes", "--password-file", pw)
			newKey := h.keyAt(t)
			if bytes.Equal(newKey, h.key) {
				t.Fatal("the key file holds the old key")
			}
			mustNotBeThere(t, h.pending())
			mustHoldValues(t, h.dir, newKey)
			// After step 3 the copy holds the old key. Between the two writes of step 4 it holds the new key beside
			// the ID of the old one. The start warns in both.
			if logs := mustStart(t, h, rotSealed); !strings.Contains(logs, "run `flats secret backup-key`") {
				t.Errorf("the start does not warn:\n%s", logs)
			}
			holds, err := core.OpenKeyCopy(mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile)), strings.TrimSpace(recoveryPassword))
			if err != nil {
				t.Fatal(err)
			}
			if want := map[string][]byte{rotatePendingRemoved: h.key, rotateCopyWritten: newKey}[point]; !bytes.Equal(holds, want) {
				t.Errorf("the recovery copy holds the key %s after a stop at %s", core.KeyID(holds), point)
			}
			// A new rotation.
			h.mustRotate(t, "--yes", "--password-file", pw)
			third := h.keyAt(t)
			if bytes.Equal(third, newKey) {
				t.Fatal("the next run made no new key")
			}
			mustHoldValues(t, h.dir, third)
			if w := core.RecoveryCopyWarning(h.dir, third); w != "" {
				t.Errorf("the recovery copy after the new rotation: %s", w)
			}
			if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 2 {
				t.Errorf("%d key.rotate events after two rotations", n)
			}
		})
	}
}

// With --out, a stop inside step 1 or after it leaves state F: FILE is incomplete, or holds a key that sealed
// nothing, and the old key opens every value. The start starts. The next run refuses FILE because it exists, and
// leaves it as it is (rule 14).
func TestRotateKeyStateF(t *testing.T) {
	for name, stop := range map[string]func(t *testing.T, h *rotHost, file string){
		"after step 1": func(t *testing.T, h *rotHost, file string) {
			stopRotation(t, h, rotateNewKeyWritten, "--yes", "--out", file)
		},
		"inside step 1": func(t *testing.T, h *rotHost, file string) {
			_, err := rotateWith(t, h.config, func(p string) error {
				if p != rotateWriteNewKey {
					return nil
				}
				mustWrite(t, file, []byte("half a key"))
				return errStopped
			}, "--yes", "--out", file)
			if !errors.Is(err, errStopped) {
				t.Fatalf("the command did not stop inside step 1: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRotHost(t)
			file := filepath.Join(t.TempDir(), "new.key")
			before := rowsText(sealedRows(t, h.dir))
			stop(t, h, file)
			left := mustRead(t, file)
			mustHoldValues(t, h.dir, h.key)
			if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, before) {
				t.Error("a stop before the commit changed a row")
			}
			mustNotBeThere(t, h.pending())
			mustStart(t, h, rotSealed)
			rows := rowsText(sealedRows(t, h.dir)) // after the start, so that only the command can change a row from here

			_, err := h.rotate(t, "--yes", "--out", file)
			refusedWith(t, "rotate-key with the FILE of a run that stopped", err, file+" exists", "Remove it, or name another file")
			if got := mustRead(t, file); !bytes.Equal(got, left) {
				t.Error("the refusal changed FILE")
			}
			if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, rows) {
				t.Error("the refusal changed a row")
			}
			if got := h.keyAt(t); !bytes.Equal(got, h.key) {
				t.Error("the refusal changed the key file")
			}
			// Another file works.
			other := filepath.Join(t.TempDir(), "other.key")
			h.mustRotate(t, "--yes", "--out", other)
			mustHoldValues(t, h.dir, mustRead(t, other))
		})
	}
}

// A rotation with --out: the key path is not written, FILE holds the key that opens every value (state G), the
// start refuses with the key IDs until that key is at the key path, and then starts (state H). In state G
// `flats secret backup-key` and `flats backup --out` refuse, a backup with --exclude-secret-key is made, and a run
// without the same FILE refuses by rule 9 and names the key ID of the values.
func TestRotateKeyOutStatesGAndH(t *testing.T) {
	h := newRotHost(t)
	pw := passwordFile(t, recoveryPassword)
	mustRunSecretKey(t, h.config, "backup-key", "--password-file", pw)
	fiBefore, err := os.Stat(h.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "new.key")
	out := h.mustRotate(t, "--yes", "--out", file, "--password-file", pw)

	newKey := mustRead(t, file)
	if len(newKey) != 32 || bytes.Equal(newKey, h.key) {
		t.Fatalf("FILE holds %d bytes", len(newKey))
	}
	if fi, err := os.Stat(file); err != nil || ostest.WrongMode(fi, 0o600) {
		t.Errorf("the mode of FILE: %v, %v", fi, err)
	}
	// The key path was not written: the same file, the same bytes, the same time of the last change.
	fiAfter, err := os.Stat(h.keyPath)
	if err != nil || !os.SameFile(fiBefore, fiAfter) || !fiAfter.ModTime().Equal(fiBefore.ModTime()) || !bytes.Equal(h.keyAt(t), h.key) {
		t.Errorf("the key path changed in a rotation with --out: %v", err)
	}
	mustNotBeThere(t, h.pending())
	mustHoldValues(t, h.dir, newKey)
	for _, want := range []string{
		"sealed 3 flat secrets and 2 operator keys again", core.KeyID(h.key), core.KeyID(newKey),
		"wrote the new key to " + file,
		"The new key must be put at " + h.keyPath, "then remove " + file,
		"While flats.db holds sealed values, the start refuses until the new key is at " + h.keyPath,
		"wrote " + filepath.Join(h.dir, core.RecoveryKeyFile), "opens only with the old key",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output with --out lacks %q:\n%s", want, out)
		}
	}
	if what := leaks(out, h.key, newKey); what != "" {
		t.Errorf("the output shows %s:\n%s", what, out)
	}
	evs := eventsOf(t, h.dir, store.AuditKeyRotate)
	if len(evs) != 1 || eventData(t, evs[0])["out"] != true || eventData(t, evs[0])["to"] != core.KeyID(newKey) {
		t.Errorf("key.rotate after --out: %v", auditSummary(evs))
	}
	// Step 4 came after step 2: the recovery copy holds the new key already.
	if w := core.RecoveryCopyWarning(h.dir, newKey); w != "" {
		t.Errorf("the recovery copy after --out: %s", w)
	}

	// State G. The start gives the second refusal of rule 22: the ID of the key, the IDs of the values, the ways out.
	err = mustRefuseStart(t, h.config, core.KeyID(h.key), core.KeyID(newKey), "flats secret rotate-key --out FILE", "flats secret restore-key", "flats secret reset")
	if !errors.Is(err, core.ErrKeySealedNothing) {
		t.Errorf("the refusal of the start is %v", err)
	}
	rows := rowsText(sealedRows(t, h.dir))
	copyBefore := mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile))
	_, err = runSecretKey(t, h.config, "backup-key", "--password-file", pw)
	refusedWith(t, "flats secret backup-key in state G", err, core.KeyID(h.key), core.KeyID(newKey))
	if got := mustRead(t, filepath.Join(h.dir, core.RecoveryKeyFile)); !bytes.Equal(got, copyBefore) {
		t.Error("the refused backup-key replaced the copy of the new key")
	}
	refusedWith(t, "flats key set in state G", keySetError(h.config, core.HostKeyNetbird, "8f3c2a1e-4b5d-4c6e-9f70-0123456789ab"), core.KeyID(newKey))
	copies := t.TempDir()
	_, err = runBackup(t, "--out", filepath.Join(copies, "a"), "--config", h.config)
	refusedWith(t, "flats backup --out in state G", err, core.KeyID(h.key), core.KeyID(newKey))
	if got := listed(t, copies); len(got) != 0 {
		t.Errorf("the refused backup wrote %v", got)
	}
	// With --exclude-secret-key the command reads no key, and makes the copy.
	if text, err := runBackup(t, "--out", filepath.Join(copies, "b"), "--exclude-secret-key", "--config", h.config); err != nil {
		t.Fatalf("flats backup --exclude-secret-key in state G: %v\n%s", err, text)
	}
	mustNotBeThere(t, filepath.Join(copies, "b", "data", core.SecretKeyFile))

	// A run without the same FILE refuses by rule 9 and names the key ID of the values.
	for _, args := range [][]string{{"--yes"}, {"--yes", "--out", filepath.Join(t.TempDir(), "another.key")}} {
		_, err = h.rotate(t, append(args, "--password-file", pw)...)
		refusedWith(t, "rotate-key in state G without the same FILE", err, "opens 0 of the 5 sealed values", "carry the key ID "+core.KeyID(newKey), "rotate-key --out", "flats secret reset")
	}
	if got := rowsText(sealedRows(t, h.dir)); !slices.Equal(got, rows) {
		t.Error("a refused command changed a row")
	}
	// A run with the same FILE finishes: it reports the re-seal durable, and makes no new key.
	out, err = h.rotate(t, "--out", file, "--password-file", pw)
	if err != nil || !strings.Contains(out, "finished a rotation that had stopped") || !strings.Contains(out, "the re-seal is durable") ||
		!strings.Contains(out, "The new key must be put at "+h.keyPath) || !strings.Contains(out, "then remove "+file) {
		t.Fatalf("the finish with the same FILE: %v\n%s", err, out)
	}
	if got := mustRead(t, file); !bytes.Equal(got, newKey) {
		t.Error("the finish changed FILE")
	}
	if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 1 {
		t.Errorf("%d key.rotate events after the finish", n)
	}

	// State H: the operator puts the key in place. The host starts, and the same FILE is refused as in place.
	mustWrite(t, h.keyPath, newKey)
	if logs := mustStart(t, h, rotSealed); strings.Contains(logs, "run `flats secret backup-key`") {
		t.Errorf("the start warns after the key is in place:\n%s", logs)
	}
	_, err = h.rotate(t, "--yes", "--out", file, "--password-file", pw)
	refusedWith(t, "rotate-key with a FILE whose key is in place", err, "holds the key that "+h.keyPath+" holds too", "remove "+file)
	if got := mustRead(t, file); !bytes.Equal(got, newKey) {
		t.Error("the refusal changed FILE")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	// A new rotation.
	h.mustRotate(t, "--yes", "--password-file", pw)
	mustHoldValues(t, h.dir, h.keyAt(t))
}

// With --out, a stop after the commit leaves state G too, also when the checkpoint did not run yet: the same FILE
// finishes the rotation.
func TestRotateKeyOutStoppedAfterTheCommit(t *testing.T) {
	for _, point := range []string{rotateCommitted, rotateCheckpointed} {
		t.Run(point, func(t *testing.T) {
			h := newRotHost(t)
			file := filepath.Join(t.TempDir(), "new.key")
			stopRotation(t, h, point, "--yes", "--out", file)
			newKey := mustRead(t, file)
			mustHoldValues(t, h.dir, newKey)
			if got := h.keyAt(t); !bytes.Equal(got, h.key) {
				t.Fatal("the key path changed")
			}
			mustRefuseStart(t, h.config, core.KeyID(h.key), core.KeyID(newKey))
			out, err := h.rotate(t, "--out", file)
			if err != nil || !strings.Contains(out, "the re-seal is durable") {
				t.Fatalf("the finish: %v\n%s", err, out)
			}
			if n := len(eventsOf(t, h.dir, store.AuditKeyRotate)); n != 1 {
				t.Errorf("%d key.rotate events", n)
			}
			mustWrite(t, h.keyPath, newKey)
			mustStart(t, h, rotSealed)
		})
	}
}

// A pending key that is left over is removed at the start of step 1 of a new rotation with --out too, and the
// output says so (rule 21). A refusal before that step leaves it where it is.
func TestRotateKeyOutRemovesAPendingKeyThatIsLeftOver(t *testing.T) {
	for name, left := range map[string][]byte{
		"a valid key that sealed nothing": bytes.Repeat([]byte{2}, 32),
		"a file that is no key":           []byte("half a key"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newRotHost(t)
			mustWrite(t, h.pending(), left)
			// A refusal of point 4: FILE lies inside the data directory.
			_, err := h.rotate(t, "--yes", "--out", filepath.Join(h.dir, "new.key"))
			refusedWith(t, "--out inside the data directory", err, "outside the data directory")
			if got := mustRead(t, h.pending()); !bytes.Equal(got, left) {
				t.Fatal("a refusal removed or changed the pending key that is left over")
			}
			file := filepath.Join(t.TempDir(), "new.key")
			out := h.mustRotate(t, "--yes", "--out", file)
			if !strings.Contains(out, h.pending()+" is left over from a rotation that stopped before it changed a value") ||
				!strings.Contains(out, "removed "+h.pending()+", which was left over") {
				t.Errorf("the output does not say that the pending key was removed:\n%s", out)
			}
			mustNotBeThere(t, h.pending())
			if got := h.keyAt(t); !bytes.Equal(got, h.key) {
				t.Error("the key path changed")
			}
			mustHoldValues(t, h.dir, mustRead(t, file))
		})
	}
}
