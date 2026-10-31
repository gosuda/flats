package store

import (
	"context"
	"errors"
	"testing"
)

func TestImmediateRollback(t *testing.T) {
	for _, name := range []string{"error", "no-op", "cancelled", "panic", "commit failure"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := open(t)
			if _, err := s.db.ExecContext(ctx, `
CREATE TABLE immediate_parent (id INTEGER PRIMARY KEY);
CREATE TABLE immediate_child (id INTEGER REFERENCES immediate_parent(id) DEFERRABLE INITIALLY DEFERRED);`); err != nil {
				t.Fatal(err)
			}
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			boom := errors.New("rollback")
			var wrote bool
			var err error
			var panicked any
			func() {
				defer func() { panicked = recover() }()
				wrote, err = s.immediate(cctx, func(x executor) (bool, error) {
					if _, err := x.ExecContext(cctx, `UPDATE autopilot SET enabled=1; UPDATE public_instance SET enabled=1`); err != nil {
						return false, err
					}
					switch name {
					case "error":
						return true, boom
					case "no-op":
						return false, nil
					case "cancelled":
						cancel()
					case "panic":
						panic(boom)
					case "commit failure":
						if _, err := x.ExecContext(cctx, `INSERT INTO immediate_child VALUES (1)`); err != nil {
							return false, err
						}
					}
					return true, nil
				})
			}()
			if wrote {
				t.Fatal("rolled back write reported committed")
			}
			switch name {
			case "no-op":
				if err != nil {
					t.Fatal(err)
				}
			case "panic":
				if panicked != boom {
					t.Fatalf("panic %v", panicked)
				}
			case "error":
				if !errors.Is(err, boom) {
					t.Fatalf("callback error %v", err)
				}
			case "cancelled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error %v", err)
				}
			default:
				if err == nil {
					t.Fatal("invalid foreign key committed")
				}
			}
			if name != "panic" && panicked != nil {
				t.Fatalf("unexpected panic %v", panicked)
			}
			if ap, err := s.GetAutopilot(ctx); err != nil || ap != (AutopilotRow{}) {
				t.Fatalf("Autopilot rollback %+v %v", ap, err)
			}
			if pi, err := s.GetPublicInstance(ctx); err != nil || pi != (PublicInstanceRow{}) {
				t.Fatalf("public instance rollback %+v %v", pi, err)
			}
			wrote, err = s.immediate(ctx, func(x executor) (bool, error) {
				_, err := x.ExecContext(ctx, `UPDATE autopilot SET enabled=1; UPDATE public_instance SET enabled=1`)
				return err == nil, err
			})
			if err != nil || !wrote {
				t.Fatalf("connection after rollback %t %v", wrote, err)
			}
			if ap, err := s.GetAutopilot(ctx); err != nil || !ap.Enabled {
				t.Fatalf("Autopilot commit %+v %v", ap, err)
			}
			if pi, err := s.GetPublicInstance(ctx); err != nil || !pi.Enabled {
				t.Fatalf("public instance commit %+v %v", pi, err)
			}
		})
	}
}
