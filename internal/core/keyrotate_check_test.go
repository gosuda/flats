package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

// docs/spec/secret-key-rotation.md, rules 22, 23 and 26: the check of a rotation that stopped.

// keyPlaces runs a test for the default key path and for a configured one. data is the data directory.
func keyPlaces(t *testing.T, run func(t *testing.T, data string, k KeyFile)) {
	t.Helper()
	t.Run("the default path", func(t *testing.T) {
		t.Parallel()
		data := t.TempDir()
		run(t, data, NewKeyFile(data, ""))
	})
	t.Run("a configured path", func(t *testing.T) {
		t.Parallel()
		data := t.TempDir()
		run(t, data, NewKeyFile(data, filepath.Join(t.TempDir(), "host.key")))
	})
}

// The first state of rule 22: a pending key waits. The check refuses and names the command, whatever the key path
// holds, before the refusals of a missing or a configured key, and it writes nothing.
func TestCheckSecretKeyRefusesWhileAPendingKeyWaits(t *testing.T) {
	t.Parallel()
	rotated := keyOf(8)
	for name, atKeyPath := range map[string][]byte{
		"the old key":     goodKey(),
		"nothing":         nil,
		"a damaged file":  []byte("half a key"),
		"an empty file":   {},
		"a key of no use": keyOf(9),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			keyPlaces(t, func(t *testing.T, data string, k KeyFile) {
				st := labelledStore(t, data, KeyID(rotated), KeyID(rotated))
				if atKeyPath != nil {
					putFile(t, k.Path(), atKeyPath)
				}
				putFile(t, k.PendingPath(), rotated)
				before := listDir(t, filepath.Dir(k.Path()))
				for what, err := range map[string]error{
					"CheckSecretKey":       CheckSecretKey(context.Background(), k, st),
					"CheckRotation":        CheckRotation(k, labelsOf(t, st)),
					"CheckStoppedRotation": CheckStoppedRotation(k, labelsOf(t, st)),
				} {
					if !errors.Is(err, ErrRotationStopped) || errors.Is(err, ErrSecretKeyMissing) || errors.Is(err, ErrKeyLocation) {
						t.Errorf("%s: err = %v, want ErrRotationStopped alone", what, err)
						continue
					}
					for _, part := range []string{"flats secret rotate-key", "finishes the rotation", k.PendingPath(), KeyID(rotated)} {
						if !strings.Contains(err.Error(), part) {
							t.Errorf("%s: the message lacks %q: %v", what, part, err)
						}
					}
				}
				if after := listDir(t, filepath.Dir(k.Path())); !slices.Equal(after, before) {
					t.Errorf("the check changed the folder of the key: %v -> %v", before, after)
				}
				if got, err := os.ReadFile(k.PendingPath()); err != nil || string(got) != string(rotated) {
					t.Errorf("the pending key changed: %v", err)
				}
			})
		})
	}
}

// The start of the service makes the same check, and creates no key in the state with nothing at the key path.
func TestNewRefusesWhileAPendingKeyWaits(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	rotated := keyOf(8)
	st := labelledStore(t, data, KeyID(rotated))
	k := NewKeyFile(data, "")
	putFile(t, k.PendingPath(), rotated)
	if _, err := New(context.Background(), Config{DataDir: data, Store: st, Logf: t.Logf}); !errors.Is(err, ErrRotationStopped) {
		t.Fatalf("New = %v, want ErrRotationStopped", err)
	}
	if _, err := os.Lstat(k.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused start left something at the key path: %v", err)
	}
}

// The second state of rule 22: a valid key that sealed none of the values, which all carry a key ID.
func TestCheckSecretKeyRefusesAKeyThatSealedNothing(t *testing.T) {
	t.Parallel()
	other, third := keyOf(8), keyOf(9)
	keyPlaces(t, func(t *testing.T, data string, k KeyFile) {
		st := labelledStore(t, data, KeyID(other), KeyID(other))
		putFile(t, k.Path(), goodKey())
		check := func(step string, ids ...string) {
			t.Helper()
			for what, err := range map[string]error{
				"CheckSecretKey": CheckSecretKey(context.Background(), k, st),
				"CheckRotation":  CheckRotation(k, labelsOf(t, st)),
			} {
				if !errors.Is(err, ErrKeySealedNothing) || errors.Is(err, ErrRotationStopped) {
					t.Errorf("%s, %s: err = %v, want ErrKeySealedNothing", step, what, err)
					continue
				}
				msg := err.Error()
				// The ID of the key, the IDs of the values, and the ways out in the order of the rule.
				order := append([]string{k.Path(), KeyID(goodKey())}, ids...)
				order = append(order, "flats secret rotate-key --out FILE", "the same file", "only after that run reported the re-seal durable",
					"flats secret restore-key", "flats secret reset")
				at := 0
				for _, part := range order {
					i := strings.Index(msg[at:], part)
					if i < 0 {
						t.Errorf("%s, %s: the message lacks %q after position %d: %v", step, what, part, at, err)
						break
					}
					at += i + len(part)
				}
			}
			// The variant for a caller that reads no key makes no check of this state (rule 26).
			if err := CheckStoppedRotation(k, labelsOf(t, st)); err != nil {
				t.Errorf("%s: CheckStoppedRotation = %v, want nil", step, err)
			}
		}
		check("no pending key", KeyID(other))

		// A pending key that is left over changes nothing of it.
		putFile(t, k.PendingPath(), third)
		check("a pending key that is left over", KeyID(other))
		if err := os.Remove(k.PendingPath()); err != nil {
			t.Fatal(err)
		}

		// Values of two keys, of which none is the key: the message names both.
		row := store.SealedSecret{Name: "LATER", Nonce: []byte("nonce-nonce-"), Ciphertext: []byte("sealed"), KeyID: KeyID(third), UpdatedAt: time.Now()}
		if err := st.PutSecret(context.Background(), "app", row, store.Audit{}); err != nil {
			t.Fatal(err)
		}
		both := []string{KeyID(other), KeyID(third)}
		slices.Sort(both)
		check("two key IDs", both...)
	})
}

// Every other state passes the check, as before the key IDs (rule 22, last paragraph).
func TestCheckSecretKeyLeavesTheOtherStatesAlone(t *testing.T) {
	t.Parallel()
	other := keyOf(8)
	here := KeyID(goodKey())
	for name, tc := range map[string]struct {
		labels  []string
		pending []byte
	}{
		"no sealed value":                              {nil, nil},
		"no sealed value and a pending key":            {nil, other},
		"every value carries the ID of the key":        {[]string{here, here}, nil},
		"a value without a key ID and a wrong key":     {[]string{KeyID(other), ""}, nil},
		"only values without a key ID":                 {[]string{"", ""}, nil},
		"values of two keys, one of them the key":      {[]string{KeyID(other), here}, nil},
		"a pending key that is a duplicate":            {[]string{here}, goodKey()},
		"a pending key that is left over":              {[]string{here}, other},
		"a pending file that is no key":                {[]string{here}, []byte("part")},
		"a duplicate while the values carry no key ID": {[]string{""}, goodKey()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			keyPlaces(t, func(t *testing.T, data string, k KeyFile) {
				st := labelledStore(t, data, tc.labels...)
				putFile(t, k.Path(), goodKey())
				if tc.pending != nil {
					putFile(t, k.PendingPath(), tc.pending)
				}
				before := listDir(t, filepath.Dir(k.Path()))
				if err := CheckSecretKey(context.Background(), k, st); err != nil {
					t.Errorf("CheckSecretKey = %v, want nil", err)
				}
				if err := CheckStoppedRotation(k, labelsOf(t, st)); err != nil {
					t.Errorf("CheckStoppedRotation = %v, want nil", err)
				}
				// The check never moves, writes or removes a pending key (rule 23).
				if after := listDir(t, filepath.Dir(k.Path())); !slices.Equal(after, before) {
					t.Errorf("the check changed the folder of the key: %v -> %v", before, after)
				}
			})
		})
	}
}

// Rule 23: the refusal of a missing key names the key ID of the values when they all carry one.
func TestMissingKeyRefusalNamesTheKeyIDOfTheValues(t *testing.T) {
	t.Parallel()
	other := keyOf(8)
	id := KeyID(other)
	t.Run("one key ID", func(t *testing.T) {
		t.Parallel()
		keyPlaces(t, func(t *testing.T, data string, k KeyFile) {
			st := labelledStore(t, data, id, id)
			// A pending key that is left over does not wait, so it changes nothing.
			putFile(t, k.PendingPath(), keyOf(9))
			err := CheckSecretKey(context.Background(), k, st)
			want, parts := ErrSecretKeyMissing, []string{"flats secret restore-key", "flats secret reset", "Flats will not create a new key"}
			if k.Configured() {
				want, parts = ErrKeyLocation, []string{"nothing is there", "never creates", "flats secret restore-key", KeySetting}
			}
			if !errors.Is(err, want) {
				t.Fatalf("err = %v, want %v", err, want)
			}
			for _, part := range append(parts, k.Path(), "key ID "+id) {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("the message lacks %q: %v", part, err)
				}
			}
		})
	})
	t.Run("operator keys alone", func(t *testing.T) {
		t.Parallel()
		data := t.TempDir()
		st := labelledStore(t, data)
		rec := store.HostKey{ID: HostKeyZrok, Nonce: []byte("nonce-nonce-"), Ciphertext: []byte("sealed"), KeyID: id, Source: KeySourceCLI, UpdatedAt: time.Now()}
		if err := st.PutHostKey(context.Background(), rec, store.Audit{}); err != nil {
			t.Fatal(err)
		}
		err := CheckSecretKey(context.Background(), NewKeyFile(data, ""), st)
		if !errors.Is(err, ErrSecretKeyMissing) || !strings.Contains(err.Error(), "flats key rm") || !strings.Contains(err.Error(), "key ID "+id) {
			t.Errorf("err = %v, want the refusal of operator keys alone with the key ID", err)
		}
	})
	for name, labels := range map[string][]string{
		"two key IDs":              {id, KeyID(keyOf(9))},
		"a value without a key ID": {id, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			keyPlaces(t, func(t *testing.T, data string, k KeyFile) {
				st := labelledStore(t, data, labels...)
				err := CheckSecretKey(context.Background(), k, st)
				if !errors.Is(err, ErrSecretKeyMissing) && !errors.Is(err, ErrKeyLocation) {
					t.Fatalf("err = %v, want the refusal of a missing key", err)
				}
				if strings.Contains(err.Error(), "key ID") {
					t.Errorf("the message names a key ID, and the values carry no single one: %v", err)
				}
			})
		})
	}
	// With something at the configured path that is no key, the refusal is the one of that file, without the ID.
	t.Run("a configured file that is no key", func(t *testing.T) {
		t.Parallel()
		data := t.TempDir()
		st := labelledStore(t, data, id)
		k := NewKeyFile(data, filepath.Join(t.TempDir(), "host.key"))
		putFile(t, k.Path(), []byte("short"))
		err := CheckSecretKey(context.Background(), k, st)
		if !errors.Is(err, ErrKeyLocation) || !errors.Is(err, ErrInvalidSecretKey) || strings.Contains(err.Error(), "key ID") {
			t.Errorf("err = %v, want the refusal of the file alone", err)
		}
	})
}

// The variant for `flats backup --exclude-secret-key` (rule 26): it refuses while a pending key waits, also with a
// key file that is missing, and it lets a duplicate pass.
func TestCheckStoppedRotation(t *testing.T) {
	t.Parallel()
	rotated := keyOf(8)
	labels := store.KeyIDCounts{KeyID(rotated): 3}
	keyPlaces(t, func(t *testing.T, data string, k KeyFile) {
		if err := CheckStoppedRotation(k, labels); err != nil {
			t.Errorf("no pending key: %v", err)
		}
		putFile(t, k.PendingPath(), rotated)
		if err := CheckStoppedRotation(k, labels); !errors.Is(err, ErrRotationStopped) {
			t.Errorf("a pending key and no key file: err = %v, want ErrRotationStopped", err)
		}
		putFile(t, k.Path(), goodKey())
		if err := CheckStoppedRotation(k, labels); !errors.Is(err, ErrRotationStopped) {
			t.Errorf("a pending key beside the old key: err = %v, want ErrRotationStopped", err)
		}
		putFile(t, k.Path(), rotated)
		if err := CheckStoppedRotation(k, labels); err != nil {
			t.Errorf("a duplicate: err = %v, want nil", err)
		}
		// A pending key whose ID no value carries does not wait, whatever the key file is.
		if err := os.Remove(k.Path()); err != nil {
			t.Fatal(err)
		}
		if err := CheckStoppedRotation(k, store.KeyIDCounts{KeyID(goodKey()): 3}); err != nil {
			t.Errorf("a pending key that labels nothing: err = %v, want nil", err)
		}
	})
}
