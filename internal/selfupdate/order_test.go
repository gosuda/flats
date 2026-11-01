package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A bad release archive is reported before any problem with the stage file,
// as when the archive was extracted in memory before staging.

func useArchive(o *Options, raw []byte) {
	o.Transport = fixtureTransport(raw, Digest(raw)+"  flats_linux_amd64.tar.gz\n")
}

// readOnlyInstallDir makes the directory holding the executable unwritable,
// so the stage file cannot be created, and moves the lock directory out of it.
func readOnlyInstallDir(t *testing.T, o *Options) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	o.ConfigDir = t.TempDir()
	dir := filepath.Dir(o.Path)
	if e := os.Chmod(dir, 0o555); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
}

// truncatedBody is a release archive whose gzip stream ends inside the flats
// entry's body, so extraction fails with a raw read error there.
func truncatedBody(t *testing.T) []byte {
	t.Helper()
	body := make([]byte, 256<<10)
	rand.NewChaCha8([32]byte{1}).Read(body)
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if e := tw.WriteHeader(&tar.Header{Name: "flats", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); e != nil {
		t.Fatal(e)
	}
	if _, e := tw.Write(body); e != nil {
		t.Fatal(e)
	}
	tw.Close()
	gz.Close()
	return b.Bytes()[:b.Len()/2]
}

var flatsEntry = &tar.Header{Name: "flats", Mode: 0o755, Typeflag: tar.TypeReg, Size: 3}

// badArchives fail validation with assets_unavailable at different points.
func badArchives(t *testing.T) map[string][]byte {
	return map[string][]byte{
		"not gzip":         []byte("not a gzip stream"),
		"missing binary":   archiveFixture(t, []*tar.Header{{Name: "other", Mode: 0o644, Typeflag: tar.TypeReg, Size: 3}}),
		"unsafe entry":     archiveFixture(t, []*tar.Header{flatsEntry, {Name: "../evil", Typeflag: tar.TypeReg}}),
		"duplicate binary": archiveFixture(t, []*tar.Header{flatsEntry, flatsEntry}),
	}
}

func TestBadArchiveBeforeUnwritableInstallDir(t *testing.T) {
	for name, raw := range badArchives(t) {
		t.Run(name, func(t *testing.T) {
			o, _ := setup(t)
			useArchive(&o, raw)
			readOnlyInstallDir(t, &o)
			_, _, e := Update(t.Context(), o)
			if Code(e) != "assets_unavailable" {
				t.Fatalf("want assets_unavailable, got %s: %v", Code(e), e)
			}
			assertOld(t, o)
		})
	}
	t.Run("truncated body", func(t *testing.T) {
		o, _ := setup(t)
		useArchive(&o, truncatedBody(t))
		readOnlyInstallDir(t, &o)
		_, _, e := Update(t.Context(), o)
		// A raw read error inside the entry body, as before staging streamed.
		if !errors.Is(e, io.ErrUnexpectedEOF) || Code(e) != "filesystem" || errors.Is(e, os.ErrPermission) {
			t.Fatalf("want the archive read error, got %s: %v", Code(e), e)
		}
		assertOld(t, o)
	})
	t.Run("valid archive", func(t *testing.T) {
		o, _ := setup(t)
		readOnlyInstallDir(t, &o)
		_, _, e := Update(t.Context(), o)
		if Code(e) != "filesystem" || !errors.Is(e, os.ErrPermission) {
			t.Fatalf("want the permission error, got %s: %v", Code(e), e)
		}
		assertOld(t, o)
	})
}

func TestBadArchiveBeforeStaleStageObstruction(t *testing.T) {
	// A stale stage path that cannot be removed stays as it is.
	obstruct := func(t *testing.T, o Options) string {
		inner := filepath.Join(stageName(o.Path), "keep")
		if e := os.MkdirAll(filepath.Dir(inner), 0o755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(inner, nil, 0o644); e != nil {
			t.Fatal(e)
		}
		return inner
	}
	for name, raw := range map[string][]byte{"bad archive": badArchives(t)["missing binary"], "valid archive": nil} {
		t.Run(name, func(t *testing.T) {
			o, valid := setup(t)
			if raw == nil {
				raw = valid
			}
			useArchive(&o, raw)
			inner := obstruct(t, o)
			_, _, e := Update(t.Context(), o)
			want := "assets_unavailable"
			if name == "valid archive" {
				want = "filesystem"
				if !errors.Is(e, syscall.ENOTEMPTY) && !errors.Is(e, syscall.EEXIST) {
					t.Fatalf("want the remove error, got %v", e)
				}
			}
			if Code(e) != want {
				t.Fatalf("want %s, got %s: %v", want, Code(e), e)
			}
			if _, se := os.Stat(inner); se != nil {
				t.Fatalf("obstruction changed: %v", se)
			}
			if b, _ := os.ReadFile(o.Path); string(b) != "old" {
				t.Fatalf("original changed: %q", b)
			}
		})
	}
}

var errInjected = errors.New("injected stage failure")

// faultyStage is a real stage file whose named operation fails. It has only
// the stageFile methods, so io.Copy cannot bypass Write through ReadFrom.
type faultyStage struct {
	f  *os.File
	op string
}

func (s faultyStage) Write(p []byte) (int, error) {
	if s.op == "write" {
		return 0, errInjected
	}
	return s.f.Write(p)
}

func (s faultyStage) Chmod(m os.FileMode) error {
	if s.op == "chmod" {
		return errInjected
	}
	return s.f.Chmod(m)
}

func (s faultyStage) Sync() error {
	if s.op == "sync" {
		return errInjected
	}
	return s.f.Sync()
}

func (s faultyStage) Close() error {
	e := s.f.Close()
	if s.op == "close" {
		return errInjected
	}
	return e
}

func failStage(t *testing.T, op string) {
	t.Helper()
	open := openStage
	t.Cleanup(func() { openStage = open })
	openStage = func(name string, mode os.FileMode) (stageFile, error) {
		f, e := open(name, mode)
		if e != nil {
			return nil, e
		}
		return faultyStage{f.(*os.File), op}, nil
	}
}

func TestBadArchiveBeforeStageFileFailures(t *testing.T) {
	for _, op := range []string{"write", "chmod", "sync", "close"} {
		for _, bad := range []bool{true, false} {
			name := op + " failure, valid archive"
			if bad {
				name = op + " failure, bad archive"
			}
			t.Run(name, func(t *testing.T) {
				o, _ := setup(t)
				if bad {
					// The flats entry is written before the defect.
					useArchive(&o, archiveFixture(t, []*tar.Header{flatsEntry, {Name: "../evil", Typeflag: tar.TypeReg}}))
				}
				probed, rechecked := false, false
				probe := o.Probe
				o.Probe = func(ctx context.Context, p string) (Identity, error) { probed = true; return probe(ctx, p) }
				o.Recheck = func() error { rechecked = true; return nil }
				failStage(t, op)
				_, _, e := Update(t.Context(), o)
				switch {
				case bad && (Code(e) != "assets_unavailable" || strings.Contains(e.Error(), errInjected.Error())):
					t.Fatalf("want the archive error alone, got %s: %v", Code(e), e)
				case !bad && !errors.Is(e, errInjected):
					t.Fatalf("want the injected error, got %s: %v", Code(e), e)
				case probed || rechecked:
					t.Fatalf("candidate went on to probe %v / recheck %v", probed, rechecked)
				}
				assertOld(t, o)
			})
		}
	}
}
