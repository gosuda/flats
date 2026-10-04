package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestReadLegacyIsReadOnly(t *testing.T) {
	path := loadFixture(t, "v5-80b206b")
	before := snapshot(t, path)
	l, err := ReadLegacy(path)
	if err != nil {
		t.Fatal(err)
	}
	// docs still waits for the legacy Private Tailscale choice.
	if !l.PrivateUpgradePending || l.Settings["portal.relays"] != "https://rly.best" || len(l.Settings) != 1 {
		t.Fatalf("legacy = %+v", l)
	}
	if snapshot(t, path) != before {
		t.Fatal("ReadLegacy changed the database")
	}
	info, err := Inspect(path)
	if err != nil || !info.HasData {
		t.Fatalf("Inspect = %+v, %v", info, err)
	}

	// Before version 2 the baseline marks every flat pending.
	l, err = ReadLegacy(loadFixture(t, "v0-e02e9d8"))
	if err != nil || !l.PrivateUpgradePending {
		t.Fatalf("v0 legacy = %+v, %v", l, err)
	}
	// A new database has nothing.
	l, err = ReadLegacy(filepath.Join(t.TempDir(), "flats.db"))
	if err != nil || l.PrivateUpgradePending || len(l.Settings) != 0 {
		t.Fatalf("missing database = %+v, %v", l, err)
	}
}

func TestHasDataAndRebinding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flats.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1700000000, 0)
	if err := s.BindHost(ctx, HostBinding{InstanceID: "a", ConfigPath: "/c", BoundAt: now}); err != nil {
		t.Fatal(err)
	}
	// The binding row alone is not data.
	if info, err := Inspect(path); err != nil || info.HasData || info.Binding == nil {
		t.Fatalf("Inspect = %+v, %v", info, err)
	}
	if err := s.ReplaceHostBinding(ctx, HostBinding{InstanceID: "b", ConfigPath: "/d", BoundAt: now}); err != nil {
		t.Fatal(err)
	}
	if b, ok, err := s.HostBinding(ctx); err != nil || !ok || b.InstanceID != "b" || b.ConfigPath != "/d" || b.MigratedAt != nil {
		t.Fatalf("binding = %+v %t %v", b, ok, err)
	}
	if has, err := s.HasSecrets(ctx); err != nil || has {
		t.Fatalf("HasSecrets = %t, %v", has, err)
	}
	if err := s.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if info, err := Inspect(path); err != nil || !info.HasData {
		t.Fatalf("Inspect after a settings row = %+v, %v", info, err)
	}
}
