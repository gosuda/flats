package provider

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/flats/internal/expose/local"
)

// Grants from config.json give the same policy token as the same grants in
// the host file, so approvals requested before the migration stay valid.
func TestInjectedGrantsKeepPolicyToken(t *testing.T) {
	for _, f := range []File{
		{Version: 1, Permitted: []ID{}},
		{Version: 1, Permitted: []ID{Portal, Tailscale}, PrivateBackend: "tailscale"},
		{Version: 1, Permitted: []ID{Funnel}, PrivateBackend: "local"},
	} {
		fileMode, ln := managerWith(t, f)
		before, err := fileMode.ExposurePolicy(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		grants := File{Permitted: []ID{}, PrivateBackend: f.PrivateBackend}
		// config.json lists providers in its canonical order.
		for _, id := range []ID{Tailscale, Funnel, Portal} {
			if f.Allows(id) {
				grants.Permitted = append(grants.Permitted, id)
			}
		}
		injected, err := New(dir, Options{Local: ln, Tailscale: &fakeTail{}, Portal: &fakePortal{}, Grants: &grants})
		if err != nil {
			t.Fatal(err)
		}
		after, err := injected.ExposurePolicy(context.Background())
		if err != nil || after != before {
			t.Fatalf("%+v: token %s, want %s (%v)", f, after, before, err)
		}
		if _, err := os.Stat(filepath.Join(dir, FileName)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("injected grants wrote the host file: %v", err)
		}
		// Reload keeps the injected grants even if a host file appears.
		if err := Save(dir, File{Version: 1, Permitted: []ID{Portal, Tailscale, Funnel}}); err != nil {
			t.Fatal(err)
		}
		if err := injected.Reload(); err != nil {
			t.Fatal(err)
		}
		if again, _ := injected.ExposurePolicy(context.Background()); again != before {
			t.Fatal("Reload replaced the injected grants")
		}
		ln.Close()
	}
}

func TestReadDoesNotCreateTheHostFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tsnet", "blog"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, exists, err := Read(dir)
	if err != nil || exists || !f.Migration.HistoricalTSNet || len(f.Permitted) != 0 {
		t.Fatalf("Read = %+v, %t, %v", f, exists, err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read created the file: %v", err)
	}
	if err := Save(dir, File{Version: 1, Permitted: []ID{Portal}, PrivateBackend: "local"}); err != nil {
		t.Fatal(err)
	}
	f, exists, err = Read(dir)
	if err != nil || !exists || !f.Allows(Portal) || f.PrivateBackend != "local" || f.Migration.HistoricalTSNet {
		t.Fatalf("Read = %+v, %t, %v", f, exists, err)
	}
	if _, err := New(t.TempDir(), Options{Local: &local.Net{}, Grants: &File{Permitted: []ID{Local}}}); err == nil {
		t.Fatal("local accepted as an injected grant")
	}
}
