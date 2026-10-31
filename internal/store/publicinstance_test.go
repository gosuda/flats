package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMigrationPublicInstance(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"new", "schema 21"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.db")
			var before AutopilotRow
			if name == "schema 21" {
				db, err := sql.Open("sqlite", fileURI(path))
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range migrations {
					if m.version > 21 {
						break
					}
					if err := runMigration(ctx, db, m); err != nil {
						db.Close()
						t.Fatal(err)
					}
				}
				s := &Store{db: db}
				before, _, err = s.UpdateAutopilot(ctx, time.UnixMilli(1700000000123), func(r AutopilotRow) (AutopilotRow, string, error) {
					r.Enabled, r.Ops, r.Guide = true, 5, "keep me"
					return r, "autopilot: existing change", nil
				})
				if err != nil {
					db.Close()
					t.Fatal(err)
				}
				pendingApproval(t, s, "existing")
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				if info, err := Inspect(path); err != nil || info.Version != 21 {
					t.Fatalf("fixture %+v %v", info, err)
				}
			}
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if row, err := s.GetPublicInstance(ctx); err != nil || row != (PublicInstanceRow{}) {
				t.Fatalf("public instance %+v %v", row, err)
			}
			ap, changes, err := s.AutopilotSync(ctx)
			if err != nil || ap != before {
				t.Fatalf("autopilot %+v, want %+v: %v", ap, before, err)
			}
			if name == "schema 21" {
				if len(changes) != 1 || changes[0].Line != "autopilot: existing change" {
					t.Fatalf("changes %+v", changes)
				}
				if a, err := s.GetApproval(ctx, "existing"); err != nil || a.Status != "pending" {
					t.Fatalf("approval %+v %v", a, err)
				}
			} else if len(changes) != 0 {
				t.Fatalf("new changes %+v", changes)
			}
			if info, err := Inspect(path); err != nil || info.Version != 23 {
				t.Fatalf("migrated %+v %v", info, err)
			}
		})
	}
}

func TestPublicInstanceHasData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f.db")
	s := open2(t, path)
	if info, err := Inspect(path); err != nil || info.HasData {
		t.Fatalf("empty host %+v %v", info, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE public_instance SET enabled=1, updated_at=123`); err != nil {
		t.Fatal(err)
	}
	if info, err := Inspect(path); err != nil || info.HasData {
		t.Fatalf("settings alone %+v %v", info, err)
	}
	if _, _, err := s.UpdatePublicInstance(ctx, time.Now(), func(r PublicInstanceRow) (PublicInstanceRow, string, error) {
		r.Enabled = false
		return r, "public instance: turned off", nil
	}); err != nil {
		t.Fatal(err)
	}
	if info, err := Inspect(path); err != nil || !info.HasData {
		t.Fatalf("change log is data %+v %v", info, err)
	}
}

func TestPublicInstanceChecks(t *testing.T) {
	s := open(t)
	for _, tc := range []struct{ name, stmt string }{
		{"another row", `INSERT INTO public_instance (id) VALUES (2)`},
		{"invalid enabled", `UPDATE public_instance SET enabled=2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.db.ExecContext(context.Background(), tc.stmt); err == nil {
				t.Fatal("invalid settings accepted")
			}
		})
	}
}

func TestUpdatePublicInstance(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1700000000, 123456789).UTC()
	r, wrote, err := s.UpdatePublicInstance(ctx, now, func(r PublicInstanceRow) (PublicInstanceRow, string, error) {
		r.Enabled = true
		return r, "public instance: turned on by operator", nil
	})
	if err != nil || !wrote || !r.Enabled || !r.UpdatedAt.Equal(time.UnixMilli(now.UnixMilli())) {
		t.Fatalf("update %+v %t %v", r, wrote, err)
	}
	if got, err := s.GetPublicInstance(ctx); err != nil || got != r {
		t.Fatalf("stored %+v, returned %+v: %v", got, r, err)
	}
	r2, wrote, err := s.UpdatePublicInstance(ctx, now.Add(time.Hour), func(r PublicInstanceRow) (PublicInstanceRow, string, error) {
		r.UpdatedAt = now.Add(time.Hour)
		return r, "public instance: no change", nil
	})
	if err != nil || wrote || r2 != r {
		t.Fatalf("no-op %+v %t %v", r2, wrote, err)
	}
	_, changes, err := s.AutopilotSync(ctx)
	if err != nil || len(changes) != 1 || changes[0].Line != "public instance: turned on by operator" || !changes[0].At.Equal(r.UpdatedAt) {
		t.Fatalf("changes %+v %v", changes, err)
	}
	offAt := now.Add(2 * time.Hour)
	r, wrote, err = s.UpdatePublicInstance(ctx, offAt, func(r PublicInstanceRow) (PublicInstanceRow, string, error) {
		r.Enabled = false
		return r, "public instance: turned off by operator", nil
	})
	if err != nil || !wrote || r.Enabled || !r.UpdatedAt.Equal(time.UnixMilli(offAt.UnixMilli())) {
		t.Fatalf("turn off %+v %t %v", r, wrote, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.GetPublicInstance(ctx); err != nil || got != r {
		t.Fatalf("reopened %+v, want %+v: %v", got, r, err)
	}
}

func TestPublicInstanceChangeLog(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for i := range 130 {
		var err error
		if i%2 == 0 {
			_, _, err = s.UpdatePublicInstance(ctx, time.Now(), func(r PublicInstanceRow) (PublicInstanceRow, string, error) {
				r.Enabled = !r.Enabled
				return r, fmt.Sprintf("public instance: line %d", i), nil
			})
		} else {
			_, _, err = s.UpdateAutopilot(ctx, time.Now(), func(r AutopilotRow) (AutopilotRow, string, error) {
				r.Enabled = !r.Enabled
				return r, fmt.Sprintf("autopilot: line %d", i), nil
			})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	_, changes, err := s.AutopilotSync(ctx)
	if err != nil || len(changes) != 130 {
		t.Fatalf("changes %d %v", len(changes), err)
	}
	for i, c := range changes {
		prefix := "autopilot:"
		if i%2 == 0 {
			prefix = "public instance:"
		}
		if c.Line != fmt.Sprintf("%s line %d", prefix, i) || i > 0 && c.Seq <= changes[i-1].Seq {
			t.Fatalf("change %d: %+v", i, c)
		}
	}
	if err := s.MarkAutopilotLogged(ctx, changes[119].Seq); err != nil {
		t.Fatal(err)
	}
	_, rest, err := s.AutopilotSync(ctx)
	if err != nil || len(rest) != 10 || rest[0] != changes[120] {
		t.Fatalf("unlogged %+v %v", rest, err)
	}
	var kept int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM autopilot_changes WHERE logged=1`).Scan(&kept); err != nil || kept != autopilotChangesKept {
		t.Fatalf("retained %d %v", kept, err)
	}
}

func TestUpdatePublicInstanceRollback(t *testing.T) {
	for _, name := range []string{"change error", "cancelled", "log failure"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := open(t)
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if name == "log failure" {
				if _, err := s.db.ExecContext(ctx, `DROP TABLE autopilot_changes`); err != nil {
					t.Fatal(err)
				}
			}
			boom := errors.New("change rejected")
			r, wrote, err := s.UpdatePublicInstance(cctx, time.Now(), func(r PublicInstanceRow) (PublicInstanceRow, string, error) {
				r.Enabled = true
				if name == "change error" {
					return r, "public instance: rejected", boom
				}
				if name == "cancelled" {
					cancel()
				}
				return r, "public instance: should roll back", nil
			})
			if err == nil || wrote || r != (PublicInstanceRow{}) {
				t.Fatalf("failed update %+v %t %v", r, wrote, err)
			}
			if name == "change error" && !errors.Is(err, boom) || name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("error not preserved: %v", err)
			}
			if got, err := s.GetPublicInstance(ctx); err != nil || got != (PublicInstanceRow{}) {
				t.Fatalf("rollback %+v %v", got, err)
			}
			if name == "log failure" {
				if err := addAutopilot(ctx, s.db); err != nil {
					t.Fatal(err)
				}
			}
			_, changes, err := s.AutopilotSync(ctx)
			if err != nil || len(changes) != 0 {
				t.Fatalf("rolled back changes %+v %v", changes, err)
			}
			if _, wrote, err := s.UpdatePublicInstance(ctx, time.Now(), func(r PublicInstanceRow) (PublicInstanceRow, string, error) {
				r.Enabled = true
				return r, "public instance: recovered", nil
			}); err != nil || !wrote {
				t.Fatalf("after rollback %t %v", wrote, err)
			}
		})
	}
}

func TestUpdatePublicInstanceConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f.db")
	a, b := open2(t, path), open2(t, path)
	var wg sync.WaitGroup
	var wrote [2]bool
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			_, wrote[i], err = s.UpdatePublicInstance(ctx, time.Now(), func(r PublicInstanceRow) (PublicInstanceRow, string, error) {
				r.Enabled = true
				return r, "public instance: turn on", nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wrote[0] == wrote[1] {
		t.Fatalf("expected exactly one write: %v", wrote)
	}
	if row, err := a.GetPublicInstance(ctx); err != nil || !row.Enabled {
		t.Fatalf("committed %+v %v", row, err)
	}
	_, changes, err := a.AutopilotSync(ctx)
	if err != nil || len(changes) != 1 {
		t.Fatalf("changes %+v %v", changes, err)
	}
}
