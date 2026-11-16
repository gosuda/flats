package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/ostest"
	"github.com/gosuda/flats/internal/store"
)

// docs/spec/secret-key-path.md, rules 16 to 20: a host whose key is outside the data directory.

// externalKey moves the fixture's key out of the data directory, to a file of its own, and returns the path and the
// bytes. The bytes are random, so that a scan of the copy for them cannot hit by accident.
func (f *fixture) externalKey() (path string, key []byte) {
	f.t.Helper()
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		f.t.Fatal(err)
	}
	path = filepath.Join(f.t.TempDir(), "flats.key")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.data, nameSecretKey)); err != nil {
		f.t.Fatal(err)
	}
	return path, key
}

// seal stores an operator key in flats.db, sealed under the key at keyPath (a default key in dataDir when keyPath is "").
func (f *fixture) seal(dataDir, keyPath string) {
	f.t.Helper()
	k, err := core.SealHostKey(core.NewKeyFile(dataDir, keyPath), core.HostKeyZrok, "zrok-token", core.KeySourceCLI, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(f.data, nameFlatsDB))
	if err != nil {
		f.t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutHostKey(context.Background(), k); err != nil {
		f.t.Fatal(err)
	}
	if err := st.Sync(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// holdsBytes lists the files of a copy that hold needle.
func holdsBytes(t *testing.T, root string, needle []byte) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, needle) {
			hits = append(hits, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestRunWithAnExternalKey(t *testing.T) {
	t.Parallel()
	f := populated(t)
	f.seal(f.data, "") // sealed under the key that populated made; the next line moves a key out and reseals
	keyPath, key := f.externalKey()
	// Seal again under the external key, and drop the value that the old key sealed.
	st, err := store.Open(filepath.Join(f.data, nameFlatsDB))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteHostKey(context.Background(), core.HostKeyZrok); err != nil {
		t.Fatal(err)
	}
	st.Close()
	f.seal(f.data, keyPath)
	o := f.options("c1")
	o.SecretKeyPath = keyPath
	res, err := f.run(o)
	if err != nil {
		t.Fatal(err)
	}
	if res.SecretKey != SecretKeyExternal || res.KeyPath != keyPath {
		t.Errorf("result key = %q at %q, want external at %q", res.SecretKey, res.KeyPath, keyPath)
	}
	m := readCopyManifest(t, res.Out)
	if m.SecretKey != SecretKeyExternal || m.has("data/secret.key") {
		t.Errorf("manifest: secret_key %q, files %v", m.SecretKey, m.Files)
	}
	mustNotExist(t, filepath.Join(res.Out, "data", nameSecretKey))
	if hits := holdsBytes(t, res.Out, key); len(hits) != 0 {
		t.Errorf("the copy holds the key in %v", hits)
	}
	got, err := Verify(context.Background(), res.Out)
	if err != nil || len(got.Failures) > 0 {
		t.Fatalf("Verify = %+v, %v", got, err)
	}
	if len(got.Notes) != 1 || !strings.Contains(got.Notes[0], "the key is outside the copy") {
		t.Errorf("notes = %v; want the note that the key is outside the copy", got.Notes)
	}
}

// A key that does not open the sealed values fails the backup, and a second run does not help (rule 17).
func TestRunFailsWhenTheExternalKeyDoesNotOpenTheSealedValues(t *testing.T) {
	t.Parallel()
	f := populated(t)
	f.seal(f.data, "") // sealed under the old key, which is about to be replaced by another
	keyPath, _ := f.externalKey()
	o := f.options("c1")
	o.SecretKeyPath = keyPath
	_, err := f.run(o)
	var rerun *RerunError
	if err == nil || errors.As(err, &rerun) || !strings.Contains(err.Error(), "does not open with the key at host.secret_key_path") ||
		!strings.Contains(err.Error(), "a second run does not clear this failure") {
		t.Fatalf("err = %v, want a failure that says a second run does not clear it", err)
	}
	f.nothingLeft()
}

// A hard link to the key under another name in the data directory escapes the checks of rule 3. The scan of the
// copy finds it (rule 16).
func TestRunFailsWhenAHardLinkHoldsTheExternalKey(t *testing.T) {
	t.Parallel()
	f := populated(t)
	keyPath, _ := f.externalKey()
	if err := os.Link(keyPath, filepath.Join(f.data, "tsnet", "alias.bin")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	o := f.options("c1")
	o.SecretKeyPath = keyPath
	_, err := f.run(o)
	var rerun *RerunError
	if err == nil || errors.As(err, &rerun) || !strings.Contains(err.Error(), "tsnet/alias.bin") || !strings.Contains(err.Error(), "holds the bytes of the secret key") {
		t.Fatalf("err = %v, want a failure that names the file and that a second run does not clear", err)
	}
	f.nothingLeft()
}

// A file that appears at <data>/secret.key after the preflight is left out and listed (rule 16), whether it appears
// before the survey of step 13 (after step 12) or during the copy of step 14. The reads of the key make no checks of
// rule 3 again, so neither one fails the backup.
func TestRunLeavesOutAKeyThatAppearsInTheDataDirectoryAfterThePreflight(t *testing.T) {
	t.Parallel()
	for _, after := range []int{12, 14} {
		t.Run(fmt.Sprintf("after step %d", after), func(t *testing.T) {
			t.Parallel()
			f := populated(t)
			keyPath, key := f.externalKey()
			o := f.options("c1")
			o.SecretKeyPath = keyPath
			o.afterStep = func(step int) error {
				if step == after {
					return os.WriteFile(filepath.Join(f.data, nameSecretKey), key, 0o600)
				}
				return nil
			}
			res, err := f.run(o)
			if err != nil {
				t.Fatal(err)
			}
			m := readCopyManifest(t, res.Out)
			if !slices.Contains(m.Excluded, "data/secret.key") || m.has("data/secret.key") {
				t.Errorf("excluded %v, files has the key: %v", m.Excluded, m.has("data/secret.key"))
			}
			if hits := holdsBytes(t, res.Out, key); len(hits) != 0 {
				t.Errorf("the copy holds the key in %v", hits)
			}
		})
	}
}

// A failure that a second run cannot clear decides the kind of the error, also when a failure that it can clear comes
// first (rule 16): "run the backup again" would only repeat the failure.
func TestCheckErrorKinds(t *testing.T) {
	t.Parallel()
	transient := Failure{Path: "data/flats.db", Reason: "changed"}
	fixed := Failure{Path: "data/tsnet/alias.bin", Reason: "holds the bytes of the secret key", Fixed: true}
	var rerun *RerunError
	if err := checkError([]Failure{transient}); !errors.As(err, &rerun) || !strings.Contains(err.Error(), "run the backup again") {
		t.Errorf("a failure that a second run clears: err = %v, want a RerunError", err)
	}
	for name, fails := range map[string][]Failure{"only fixed": {fixed}, "transient first": {transient, fixed}} {
		err := checkError(fails)
		if err == nil || errors.As(err, &rerun) || !strings.Contains(err.Error(), "tsnet/alias.bin") || !strings.Contains(err.Error(), "a second run does not clear this failure") {
			t.Errorf("%s: err = %v, want the fixed failure, and no RerunError", name, err)
		}
	}
}

func TestRunRefusesInTheCasesOfRule3(t *testing.T) {
	t.Parallel()
	t.Run("a second key in the data directory", func(t *testing.T) {
		t.Parallel()
		f := populated(t)
		keyPath, key := f.externalKey()
		if err := os.WriteFile(filepath.Join(f.data, nameSecretKey), key, 0o600); err != nil {
			t.Fatal(err)
		}
		o := f.options("c1")
		o.SecretKeyPath = keyPath
		_, err := f.run(o)
		var refused *RefusedError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), "move that file away") {
			t.Fatalf("err = %v, want a refusal", err)
		}
		f.nothingLeft()
	})
	t.Run("a key that resolves into the data directory", func(t *testing.T) {
		t.Parallel()
		f := populated(t)
		inner := filepath.Join(f.data, "tsnet", "inner.key")
		if err := os.WriteFile(inner, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(f.data, nameSecretKey))
		link := filepath.Join(t.TempDir(), "flats.key")
		if err := ostest.Symlink(t, inner, link); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
		o := f.options("c1")
		o.SecretKeyPath = link
		_, err := f.run(o)
		var refused *RefusedError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), "inside the data directory") {
			t.Fatalf("err = %v, want a refusal", err)
		}
		f.nothingLeft()
	})
}

// A key that cannot be read refuses the backup in the preflight and names --exclude-secret-key. The flag makes the
// command skip the key (rules 17 and 18).
func TestRunWithAnExternalKeyThatIsMissing(t *testing.T) {
	t.Parallel()
	f := populated(t)
	keyPath, _ := f.externalKey()
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	o := f.options("c1")
	o.SecretKeyPath = keyPath
	var steps []int
	o.afterStep = func(step int) error { steps = append(steps, step); return nil }
	_, err := f.run(o)
	var refused *RefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "--exclude-secret-key") || !strings.Contains(err.Error(), keyPath) {
		t.Fatalf("err = %v, want a refusal that names the key and the flag", err)
	}
	if len(steps) != 0 {
		t.Errorf("the backup ran steps %v before it refused; the refusal is in the preflight", steps)
	}
	f.nothingLeft()

	o = f.options("c2")
	o.SecretKeyPath, o.ExcludeSecretKey = keyPath, true
	res, err := f.run(o)
	if err != nil {
		t.Fatalf("with --exclude-secret-key: %v", err)
	}
	if res.SecretKey != SecretKeyExternal {
		t.Errorf("secret_key = %q, want external", res.SecretKey)
	}
}

// Rule 22 of host backup applies to the external key: a key that changes or is gone between step 13 and step 18 fails the
// copy.
func TestRunFailsWhenTheExternalKeyChangesOrGoes(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(t *testing.T, path string){
		"changes": func(t *testing.T, path string) {
			if err := os.WriteFile(path, bytes.Repeat([]byte{9}, 32), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"goes": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := populated(t)
			keyPath, _ := f.externalKey()
			o := f.options("c1")
			o.SecretKeyPath = keyPath
			o.afterStep = func(step int) error {
				if step == 14 {
					change(t, keyPath)
				}
				return nil
			}
			_, err := f.run(o)
			var rerun *RerunError
			if !errors.As(err, &rerun) || !strings.Contains(err.Error(), "run the backup again") || !strings.Contains(err.Error(), keyPath) {
				t.Fatalf("err = %v, want a failure to run again that names the key", err)
			}
			f.nothingLeft()
		})
	}
}

// With --exclude-secret-key the command does not read the key, so a change is not noticed and no scan runs (rule 18).
func TestRunWithAnExcludedExternalKeyReadsNothing(t *testing.T) {
	t.Parallel()
	f := populated(t)
	keyPath, key := f.externalKey()
	if err := os.Link(keyPath, filepath.Join(f.data, "tsnet", "alias.bin")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	o := f.options("c1")
	o.SecretKeyPath, o.ExcludeSecretKey = keyPath, true
	res, err := f.run(o)
	if err != nil {
		t.Fatal(err)
	}
	// The copy holds the key under the name of the hard link: the scan did not run, as the flag says.
	if hits := holdsBytes(t, res.Out, key); len(hits) != 1 {
		t.Errorf("hits = %v; this test pins that the flag skips the scan", hits)
	}
}

func TestHashScanFindsAKeyAcrossTheBoundaryOfAReadAndHashesTheFile(t *testing.T) {
	t.Parallel()
	needle := bytes.Repeat([]byte{0xCD}, 32)
	const size = 3*256<<10 + 100
	buf := *bufPool.Get().(*[]byte)
	chunk := len(buf)
	bufPool.Put(&buf)
	for name, at := range map[string]int{
		"at the start":        0,
		"across the boundary": chunk - 16,
		"just before it":      chunk - 32,
		"just after it":       chunk,
		"in the second read":  chunk + 5000,
		"at the very end":     size - 32,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := make([]byte, size)
			if _, err := rand.Read(data); err != nil {
				t.Fatal(err)
			}
			copy(data[at:], needle)
			p := filepath.Join(t.TempDir(), "f")
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			got, found, err := hashScan(p, needle)
			if err != nil || !found {
				t.Fatalf("hashScan = %v, found %v; want a hit at %d", err, found, at)
			}
			want, err := hashFile(p)
			if err != nil || got != want {
				t.Errorf("hashScan hash/size = %+v, hashFile = %+v, %v", got, want, err)
			}
			// Without the needle nothing is found, and the hash is the same.
			plain := bytes.ReplaceAll(data, needle, bytes.Repeat([]byte{1}, 32))
			if err := os.WriteFile(p, plain, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, found, err := hashScan(p, needle); err != nil || found {
				t.Errorf("hashScan of a file without the needle: found %v, %v", found, err)
			}
		})
	}
}
