package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSkillSync(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "skill/SKILL.md")
	cfg := filepath.Join(dir, "config")
	updated, path, e := SyncSkill(t.Context(), p, []byte("first"), "", cfg, nil)
	if e != nil || !updated {
		t.Fatalf("%v %v", updated, e)
	}
	updated, _, e = SyncSkill(t.Context(), p, []byte("first"), "", cfg, nil)
	if e != nil || updated {
		t.Fatalf("%v %v", updated, e)
	}
	_, _, e = SyncSkill(t.Context(), p, []byte("second"), "", cfg, nil)
	if Code(e) != "file_conflict" {
		t.Fatal(e)
	}
	_, _, e = SyncSkill(t.Context(), p, []byte("second"), Digest([]byte("wrong")), cfg, nil)
	if Code(e) != "file_conflict" {
		t.Fatal(e)
	}
	updated, _, e = SyncSkill(t.Context(), p, []byte("second"), Digest([]byte("first")), cfg, nil)
	if e != nil || !updated {
		t.Fatal(e)
	}
	b, e := os.ReadFile(path)
	if e != nil || string(b) != "second" {
		t.Fatalf("%q %v", b, e)
	}
}
func TestSkillSyncRefusals(t *testing.T) {
	for _, name := range []string{"symlink", "locked", "changed", "recheck", "cancelled", "name", "missing expected"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "SKILL.md")
			cfg := filepath.Join(dir, "config")
			os.WriteFile(p, []byte("old"), 0644)
			ctx := t.Context()
			var check func() error
			want := ""
			switch name {
			case "symlink":
				os.Rename(p, p+".real")
				os.Symlink(p+".real", p)
				want = "invalid_target"
			case "locked":
				real, e := canonical(p)
				if e != nil {
					t.Fatal(e)
				}
				unlock, e := lock(real, cfg)
				if e != nil {
					t.Fatal(e)
				}
				defer unlock()
				want = "locked"
			case "changed":
				check = func() error { return os.WriteFile(p, []byte("external"), 0644) }
				want = "file_conflict"
			case "recheck":
				check = func() error { return failure("server_changed", "changed") }
				want = "server_changed"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = "cancelled"
			case "name":
				p = filepath.Join(dir, "other")
				want = "invalid_target"
			case "missing expected":
				os.Remove(p)
				want = "file_conflict"
			}
			_, _, e := SyncSkill(ctx, p, []byte("new"), Digest([]byte("old")), cfg, check)
			if e == nil || Code(e) != want {
				t.Fatalf("want %s got %v", want, e)
			}
			if name != "changed" && name != "name" && name != "missing expected" {
				b, e := os.ReadFile(p)
				if e != nil || string(b) != "old" {
					t.Fatalf("original changed %q %v", b, e)
				}
			}
			if _, e = os.Stat(stageName(p)); e != nil && !errors.Is(e, os.ErrNotExist) {
				t.Fatal(e)
			}
		})
	}
}

func TestCancellationReleasesFileLock(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "SKILL.md")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, canonicalPath, e := SyncSkill(ctx, p, []byte("new"), "", dir, nil)
	if Code(e) != "cancelled" {
		t.Fatal(e)
	}
	unlock, e := lock(canonicalPath, dir)
	if e != nil {
		t.Fatal(e)
	}
	unlock()
	if _, e = os.Stat(stageName(canonicalPath)); !os.IsNotExist(e) {
		t.Fatalf("staging remained: %v", e)
	}
}
