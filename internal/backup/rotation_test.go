package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// docs/spec/secret-key-rotation.md, rule 26, and docs/spec/host-backup.md, rules 3 and 12: what `flats backup` does
// around a rotation of the host key.

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func putBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sealedUnder makes key the key at keyPath and stores an operator key that is sealed under it, so that flats.db
// holds one value with the key ID of key.
func (f *fixture) sealedUnder(keyPath string, key []byte) {
	f.t.Helper()
	putBytes(f.t, keyPath, key)
	configured := keyPath
	if keyPath == filepath.Join(f.data, nameSecretKey) {
		configured = ""
	}
	f.seal(f.data, configured)
}

// The pending key of a rotation and a temporary key file are classes of their own only at the top of the data
// directory: below a flat the same names are files of the flat.
func TestClassifyLeavesOutTheKeyFilesOfARotation(t *testing.T) {
	t.Parallel()
	for rel, want := range map[string]class{
		"secret.key.next":                  classExcluded,
		".secret-key-123456":               classExcluded,
		".secret-key-":                     classExcluded,
		"secret.key":                       classHost,
		"secret.key.recovery":              classHost,
		"secret.key.recovery.id":           classHost,
		"secret.key.nextdoor":              classHost,
		".secret.key.recovery-123":         classHost,
		"tsnet/secret.key.next":            classHost,
		"flats/a/secret.key.next":          classHost,
		"flats/a/data/files/.secret-key-1": classGuest,
	} {
		if got := classify(rel); got.class != want {
			t.Errorf("classify(%q) = %v, want %v", rel, got.class, want)
		}
	}
}

// The command never copies secret.key.next or a temporary key file .secret-key-*, and the manifest lists them in
// excluded, with and without --exclude-secret-key (rule 3). A pending key that is left over does not stop a copy
// (state B).
func TestRunLeavesOutThePendingKeyAndTemporaryKeyFiles(t *testing.T) {
	t.Parallel()
	for _, exclude := range []bool{false, true} {
		f := populated(t)
		key := randomKey(t)
		f.sealedUnder(filepath.Join(f.data, nameSecretKey), key)
		left, temp := randomKey(t), randomKey(t)
		putBytes(t, filepath.Join(f.data, namePendingKey), left) // a valid key that sealed nothing: left over
		putBytes(t, filepath.Join(f.data, ".secret-key-4242"), temp)
		o := f.options("copy")
		o.ExcludeSecretKey = exclude
		res, err := f.run(o)
		if err != nil {
			t.Fatalf("exclude %v: %v", exclude, err)
		}
		m := readCopyManifest(t, res.Out)
		for _, name := range []string{namePendingKey, ".secret-key-4242"} {
			mustNotExist(t, filepath.Join(res.Out, "data", name))
			if m.has("data/" + name) {
				t.Errorf("exclude %v: the manifest lists %s as a file", exclude, name)
			}
			if !slices.Contains(m.Excluded, "data/"+name) {
				t.Errorf("exclude %v: excluded = %v, want it to list %s", exclude, m.Excluded, name)
			}
		}
		for name, bytes := range map[string][]byte{"the pending key": left, "the temporary key file": temp} {
			if hits := holdsBytes(t, res.Out, bytes); len(hits) != 0 {
				t.Errorf("exclude %v: %v hold the bytes of %s", exclude, hits, name)
			}
		}
		if m.has("data/"+nameSecretKey) == exclude {
			t.Errorf("exclude %v: the manifest lists secret.key: %v", exclude, !exclude)
		}
		if got, err := Verify(context.Background(), res.Out); err != nil || len(got.Failures) > 0 {
			t.Errorf("exclude %v: Verify = %+v, %v", exclude, got, err)
		}
	}
}

// refusedBeforeAnyWrite runs the backup and fails the test unless it refuses with a message that holds each of want
// and leaves the folder of the copies as it was.
func refusedBeforeAnyWrite(t *testing.T, f *fixture, o Options, want ...string) {
	t.Helper()
	before := dirNames(t, f.out)
	_, err := f.run(o)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal lacks %q: %v", w, err)
		}
	}
	if after := dirNames(t, f.out); !slices.Equal(before, after) {
		t.Errorf("the folder of the copies changed: %v -> %v", before, after)
	}
}

// `flats backup --out` refuses in its preflight in the two states of rule 22, with the message of that rule, and
// writes nothing. With --exclude-secret-key it reads no key for the second state and makes the copy there, and it
// still refuses while a pending key waits (rule 26). In the states B and D a copy is made.
func TestRunAroundARotationOfADefaultKey(t *testing.T) {
	t.Parallel()
	type state struct {
		put func(t *testing.T, f *fixture) (old, fresh []byte)
		// refuses says whether the copy is refused, without and with --exclude-secret-key.
		refuses, refusesExcluded bool
		want                     func(old, fresh []byte) []string
	}
	keyPath := func(f *fixture) string { return filepath.Join(f.data, nameSecretKey) }
	pending := func(f *fixture) string { return filepath.Join(f.data, namePendingKey) }
	names := func(old, fresh []byte) []string { return []string{"flats secret rotate-key"} }
	states := map[string]state{
		"C with the old key at the key path": {put: func(t *testing.T, f *fixture) ([]byte, []byte) {
			old, fresh := randomKey(t), randomKey(t)
			f.sealedUnder(keyPath(f), fresh)
			putBytes(t, keyPath(f), old)
			putBytes(t, pending(f), fresh)
			return old, fresh
		}, refuses: true, refusesExcluded: true, want: names},
		"C with nothing at the key path": {put: func(t *testing.T, f *fixture) ([]byte, []byte) {
			fresh := randomKey(t)
			f.sealedUnder(keyPath(f), fresh)
			if err := os.Remove(keyPath(f)); err != nil {
				t.Fatal(err)
			}
			putBytes(t, pending(f), fresh)
			return nil, fresh
		}, refuses: true, refusesExcluded: true, want: names},
		"C with a damaged file at the key path": {put: func(t *testing.T, f *fixture) ([]byte, []byte) {
			fresh := randomKey(t)
			f.sealedUnder(keyPath(f), fresh)
			putBytes(t, keyPath(f), []byte("damaged"))
			putBytes(t, pending(f), fresh)
			return nil, fresh
		}, refuses: true, refusesExcluded: true, want: names},
		"D": {put: func(t *testing.T, f *fixture) ([]byte, []byte) {
			fresh := randomKey(t)
			f.sealedUnder(keyPath(f), fresh)
			putBytes(t, pending(f), fresh)
			return nil, fresh
		}},
		"G": {put: func(t *testing.T, f *fixture) ([]byte, []byte) {
			old, fresh := randomKey(t), randomKey(t)
			f.sealedUnder(keyPath(f), fresh)
			putBytes(t, keyPath(f), old)
			return old, fresh
		}, refuses: true, want: func(old, fresh []byte) []string {
			return []string{core.KeyID(old), core.KeyID(fresh), "flats secret rotate-key --out FILE", "flats secret restore-key"}
		}},
		"a folder at the pending name": {put: func(t *testing.T, f *fixture) ([]byte, []byte) {
			key := randomKey(t)
			f.sealedUnder(keyPath(f), key)
			if err := os.Mkdir(pending(f), 0o700); err != nil {
				t.Fatal(err)
			}
			return nil, key
		}, refuses: true, refusesExcluded: true, want: func(old, fresh []byte) []string { return []string{namePendingKey, "move it away"} }},
	}
	for name, st := range states {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, exclude := range []bool{false, true} {
				f := newFixture(t)
				old, fresh := st.put(t, f)
				o := f.options("copy")
				o.ExcludeSecretKey = exclude
				refuses := st.refuses
				if exclude {
					refuses = st.refusesExcluded
				}
				if refuses {
					refusedBeforeAnyWrite(t, f, o, st.want(old, fresh)...)
					continue
				}
				res, err := f.run(o)
				if err != nil {
					t.Fatalf("exclude %v: %v", exclude, err)
				}
				mustNotExist(t, filepath.Join(res.Out, "data", namePendingKey))
				if !exclude {
					if hits := holdsBytes(t, res.Out, fresh); len(hits) != 1 {
						t.Errorf("the key is in %v, want it in secret.key alone", hits)
					}
				} else if hits := holdsBytes(t, res.Out, fresh); len(hits) != 0 {
					t.Errorf("exclude: %v hold the bytes of the key", hits)
				}
			}
		})
	}
}

// With host.secret_key_path the pending key lies beside the configured key, outside the data directory. The
// command reads it for the first state of rule 22, also with --exclude-secret-key, and a key file that is missing
// counts as another key than the pending one.
func TestRunAroundARotationOfAnExternalKey(t *testing.T) {
	t.Parallel()
	for name, put := range map[string]func(t *testing.T, path string, old []byte){
		"the old key at the key path": func(t *testing.T, path string, old []byte) { putBytes(t, path, old) },
		"nothing at the key path": func(t *testing.T, path string, old []byte) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run("C with "+name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			path, fresh := f.externalKey()
			f.seal(f.data, path)
			put(t, path, randomKey(t))
			putBytes(t, path+".next", fresh)
			o := f.options("copy")
			o.SecretKeyPath, o.ExcludeSecretKey = path, true
			refusedBeforeAnyWrite(t, f, o, "flats secret rotate-key", path+".next")
			if name == "the old key at the key path" {
				o.ExcludeSecretKey = false
				refusedBeforeAnyWrite(t, f, o, "flats secret rotate-key", path+".next")
			}
		})
	}
	t.Run("D", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		path, key := f.externalKey()
		f.seal(f.data, path)
		putBytes(t, path+".next", key)
		for _, exclude := range []bool{false, true} {
			o := f.options("copy")
			if exclude {
				o = f.options("excluded")
			}
			o.SecretKeyPath, o.ExcludeSecretKey = path, exclude
			res, err := f.run(o)
			if err != nil {
				t.Fatalf("exclude %v: %v", exclude, err)
			}
			if hits := holdsBytes(t, res.Out, key); len(hits) != 0 {
				t.Errorf("exclude %v: %v hold the bytes of the key", exclude, hits)
			}
		}
	})
}
