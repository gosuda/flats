package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// docs/spec/secret-key-rotation.md, rules 15 and 30: the transaction that seals every value again.

// resealFixture holds rows of two flats and three operator records, one of them without a value. Every value
// carries labelA, and each row has its own time.
func resealFixture(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s := open(t)
	makeFlat(t, s, "alpha")
	makeFlat(t, s, "bravo")
	base := time.UnixMilli(1_700_000_000_000).UTC()
	for i, p := range []struct{ flat, name string }{{"bravo", "TWO"}, {"alpha", "ZED"}, {"alpha", "ONE"}} {
		sec := SealedSecret{Name: p.name, Nonce: fill(byte(10+i), 12), Ciphertext: fill(byte(20+i), 40), KeyID: labelA, UpdatedAt: base.Add(time.Duration(i) * time.Hour)}
		if err := s.PutSecret(ctx, p.flat, sec, Audit{}); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{"tailscale", "netbird"} {
		rec := HostKey{ID: id, Nonce: fill(byte(30+i), 12), Ciphertext: fill(byte(40+i), 50), KeyID: labelA, Source: "console", UpdatedAt: base.Add(time.Duration(10+i) * time.Hour)}
		if err := s.PutHostKey(ctx, rec, Audit{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutHostKey(ctx, HostKey{ID: "zrok", Source: "env", UpdatedAt: base.Add(20 * time.Hour)}, Audit{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"netbird", "zrok"} {
		if err := s.SetHostKeyError(ctx, id, "refused "+id); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// rowsOf lists every column of the two tables that hold sealed values, one line a row, so that a test can compare
// the state before and after.
func rowsOf(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	for _, q := range []string{
		`SELECT 'secret ' || flat || '/' || name || ' nonce=' || hex(nonce) || ' ct=' || hex(ciphertext) || ' label=' || key_id || ' at=' || updated_at FROM secrets ORDER BY flat, name`,
		`SELECT 'record ' || id || ' nonce=' || CASE WHEN nonce IS NULL THEN 'none' ELSE hex(nonce) END || ' ct=' || CASE WHEN ciphertext IS NULL THEN 'none' ELSE hex(ciphertext) END || ' label=' || key_id || ' source=' || source || ' at=' || updated_at || ' error=' || last_error FROM host_keys ORDER BY id`,
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			out = append(out, line)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// rotations returns the key.rotate events of s.
func rotations(t *testing.T, s *Store) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, e := range allAudit(t, s) {
		if e.Kind == AuditKeyRotate {
			out = append(out, e)
		}
	}
	return out
}

// newSeal returns the seal that the tests give a value: bytes that depend on the value, so that a row that got the
// seal of another value shows.
func newSeal(v SealedValue) (nonce, ciphertext []byte) {
	return []byte("nonce of " + v.String()), []byte("sealed again: " + v.String())
}

func TestResealSealsEveryValueAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := resealFixture(t)
	before := rowsOf(t, s)
	events := len(allAudit(t, s))
	at := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)

	var seen []string
	secrets, keys, err := s.Reseal(ctx, Reseal{From: labelA, To: labelB, Audit: Audit{Time: at, Via: AuditViaCommand, Actor: "operator"},
		Seal: func(v SealedValue) ([]byte, []byte, error) {
			// The function gets the stored seal of the value.
			seen = append(seen, fmt.Sprintf("%s %X %X", v, v.Nonce, v.Ciphertext))
			nonce, ciphertext := newSeal(v)
			return nonce, ciphertext, nil
		}})
	if err != nil || secrets != 3 || keys != 2 {
		t.Fatalf("Reseal = %d, %d, %v; want 3 secrets and 2 keys", secrets, keys, err)
	}
	// Flat secrets by flat and name, then the records with a value by id.
	wantSeen := []string{
		fmt.Sprintf("secret ONE of alpha %X %X", fill(12, 12), fill(22, 40)),
		fmt.Sprintf("secret ZED of alpha %X %X", fill(11, 12), fill(21, 40)),
		fmt.Sprintf("secret TWO of bravo %X %X", fill(10, 12), fill(20, 40)),
		fmt.Sprintf("operator key netbird %X %X", fill(31, 12), fill(41, 50)),
		fmt.Sprintf("operator key tailscale %X %X", fill(30, 12), fill(40, 50)),
	}
	if !slices.Equal(seen, wantSeen) {
		t.Errorf("the function saw\n %q\nwant\n %q", seen, wantSeen)
	}

	// Each row has the new nonce, the new ciphertext and the new label. Nothing else of it changed, and the record
	// without a value is as it was.
	after := rowsOf(t, s)
	if len(after) != len(before) {
		t.Fatalf("%d rows after, %d before", len(after), len(before))
	}
	values := []SealedValue{{Flat: "alpha", Name: "ONE"}, {Flat: "alpha", Name: "ZED"}, {Flat: "bravo", Name: "TWO"}, {Name: "netbird"}, {Name: "tailscale"}}
	for i, v := range values {
		nonce, ciphertext := newSeal(v)
		old := before[i]
		tail := old[strings.Index(old, " label="+labelA)+len(" label="+labelA):] // the time, and the source and the error of a record
		head := old[:strings.Index(old, " nonce=")]
		want := fmt.Sprintf("%s nonce=%X ct=%X label=%s%s", head, nonce, ciphertext, labelB, tail)
		if after[i] != want {
			t.Errorf("row %d after the re-seal:\n got %s\nwant %s", i, after[i], want)
		}
	}
	if after[5] != before[5] || !strings.HasPrefix(after[5], "record zrok nonce=none ct=none label= ") {
		t.Errorf("the record without a value changed:\n got %s\nwant %s", after[5], before[5])
	}

	// One key.rotate event, and no other event (rule 30).
	all := allAudit(t, s)
	if len(all) != events+1 {
		t.Fatalf("%d events after the re-seal, want one more than %d: %v", len(all), events, auditKinds(all))
	}
	e := all[len(all)-1]
	if e.Kind != AuditKeyRotate || e.Flat != "" || e.FlatID != "" || e.Name != "" || e.Via != AuditViaCommand || e.Actor != "operator" || !e.Time.Equal(at) {
		t.Errorf("event = %+v", e)
	}
	if want := map[string]any{"from": labelA, "to": labelB, "secrets": 3, "keys": 2, "out": false}; !jsonEqual(dataOf(t, e), want) {
		t.Errorf("data = %s, want %v", e.Data, want)
	}
	if ids, err := s.SealedKeyIDs(ctx); err != nil || ids[labelB] != 5 || ids.Total() != 5 {
		t.Errorf("SealedKeyIDs = %v, %v; want five values under the new label", ids, err)
	}
}

// With no sealed value the event is still written, with zero counts (rule 30).
func TestResealWithNoSealedValueWritesTheEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	if err := s.PutHostKey(ctx, HostKey{ID: "zrok", Source: "env", UpdatedAt: time.Now()}, Audit{}); err != nil {
		t.Fatal(err)
	}
	before := rowsOf(t, s)
	calls := 0
	secrets, keys, err := s.Reseal(ctx, Reseal{From: labelA, To: labelB, Out: true, Audit: Audit{Via: AuditViaCommand},
		Seal: func(v SealedValue) ([]byte, []byte, error) { calls++; return []byte("n"), []byte("c"), nil }})
	if err != nil || secrets != 0 || keys != 0 || calls != 0 {
		t.Fatalf("Reseal = %d, %d, %v after %d calls; want nothing sealed", secrets, keys, err, calls)
	}
	if after := rowsOf(t, s); !slices.Equal(after, before) {
		t.Errorf("rows changed: %q -> %q", before, after)
	}
	evs := rotations(t, s)
	if len(evs) != 1 {
		t.Fatalf("%d key.rotate events, want 1", len(evs))
	}
	if want := map[string]any{"from": labelA, "to": labelB, "secrets": 0, "keys": 0, "out": true}; !jsonEqual(dataOf(t, evs[0]), want) {
		t.Errorf("data = %s, want %v", evs[0].Data, want)
	}
}

// A re-seal that fails at any point changes no row and writes no event (rule 15). The cases cover the function of
// the caller, the write of the event, the COMMIT statement itself and a cancelled context.
func TestAFailedResealChangesNothing(t *testing.T) {
	t.Parallel()
	boom := errors.New("the value does not open")
	good := func(v SealedValue) ([]byte, []byte, error) {
		nonce, ciphertext := newSeal(v)
		return nonce, ciphertext, nil
	}
	for name, tc := range map[string]struct {
		prepare func(t *testing.T, s *Store)
		reseal  func(cancel context.CancelFunc, calls *int) Reseal
		check   func(t *testing.T, err error, calls int)
	}{
		"the function fails on the last value": {
			reseal: func(_ context.CancelFunc, calls *int) Reseal {
				return Reseal{To: labelB, Seal: func(v SealedValue) ([]byte, []byte, error) {
					*calls++
					if v.Flat == "" && v.Name == "tailscale" {
						return nil, nil, boom
					}
					return good(v)
				}}
			},
			check: func(t *testing.T, err error, calls int) {
				if !errors.Is(err, boom) || calls != 5 {
					t.Errorf("err = %v after %d calls; want the error of the function at the fifth value", err, calls)
				}
			},
		},
		"the function returns an empty seal": {
			reseal: func(_ context.CancelFunc, calls *int) Reseal {
				return Reseal{To: labelB, Seal: func(v SealedValue) ([]byte, []byte, error) {
					*calls++
					if v.Flat == "" {
						return nil, nil, nil // written as it is, this would empty the record
					}
					return good(v)
				}}
			},
			check: func(t *testing.T, err error, calls int) {
				if err == nil || !strings.Contains(err.Error(), "operator key netbird") {
					t.Errorf("err = %v; want a refusal that names the value", err)
				}
			},
		},
		"the write of the event fails": {
			prepare: failAudit,
			reseal:  func(_ context.CancelFunc, calls *int) Reseal { return Reseal{To: labelB, Seal: good} },
			check: func(t *testing.T, err error, _ int) {
				if err == nil || !strings.Contains(err.Error(), "audit write failed") {
					t.Errorf("err = %v; want the failure of the audit write", err)
				}
			},
		},
		"the COMMIT fails": {
			// A deferred foreign key is checked by the COMMIT statement, so every statement before it succeeds.
			prepare: func(t *testing.T, s *Store) {
				if _, err := s.db.Exec(`
CREATE TABLE reseal_parent (id INTEGER PRIMARY KEY);
CREATE TABLE reseal_child (id INTEGER REFERENCES reseal_parent(id) DEFERRABLE INITIALLY DEFERRED);
CREATE TRIGGER reseal_commit_fails AFTER INSERT ON audit_events WHEN NEW.kind='key.rotate'
BEGIN INSERT INTO reseal_child VALUES (1); END;`); err != nil {
					t.Fatal(err)
				}
			},
			reseal: func(_ context.CancelFunc, calls *int) Reseal {
				return Reseal{To: labelB, Seal: func(v SealedValue) ([]byte, []byte, error) { *calls++; return good(v) }}
			},
			check: func(t *testing.T, err error, calls int) {
				if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") || calls != 5 {
					t.Errorf("err = %v after %d calls; want the failure of the commit after every value was sealed", err, calls)
				}
			},
		},
		"the context is cancelled inside the last call": {
			reseal: func(cancel context.CancelFunc, calls *int) Reseal {
				return Reseal{To: labelB, Seal: func(v SealedValue) ([]byte, []byte, error) {
					*calls++
					if v.Flat == "" && v.Name == "tailscale" {
						cancel()
					}
					return good(v)
				}}
			},
			check: func(t *testing.T, err error, calls int) {
				if !errors.Is(err, context.Canceled) || calls != 5 {
					t.Errorf("err = %v after %d calls; want context.Canceled", err, calls)
				}
			},
		},
		"no function": {
			reseal: func(context.CancelFunc, *int) Reseal { return Reseal{To: labelB} },
			check: func(t *testing.T, err error, _ int) {
				if err == nil {
					t.Error("a re-seal without a function succeeded")
				}
			},
		},
		"no label of the new key": {
			reseal: func(context.CancelFunc, *int) Reseal { return Reseal{From: labelA, Seal: good} },
			check: func(t *testing.T, err error, _ int) {
				if err == nil {
					t.Error("a re-seal without the label of the new key succeeded")
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := resealFixture(t)
			if tc.prepare != nil {
				tc.prepare(t, s)
			}
			before := rowsOf(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			secrets, keys, err := s.Reseal(ctx, tc.reseal(cancel, &calls))
			tc.check(t, err, calls)
			if secrets != 0 || keys != 0 {
				t.Errorf("a failed re-seal reported %d secrets and %d keys", secrets, keys)
			}
			if after := rowsOf(t, s); !slices.Equal(after, before) {
				t.Errorf("rows after the failed re-seal:\n got %q\nwant %q", after, before)
			}
			if evs := rotations(t, s); len(evs) != 0 {
				t.Errorf("the failed re-seal wrote %d key.rotate events", len(evs))
			}
			// The store still works: the transaction was rolled back and its connection is usable.
			if _, err := s.SealedKeyIDs(context.Background()); err != nil {
				t.Errorf("a read after the failed re-seal: %v", err)
			}
		})
	}
}
