package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestNetworkPolicyMigrationLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	all := slices.Clone(migrations)
	withMigrations(t, migrations[:3])
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.CreateFlat(t.Context(), Flat{Slug: "app", Name: "app", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEnv(t.Context(), "app", EnvVar{Name: "MODE", Value: "original", UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	migrations = all
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.NetworkPolicy(t.Context(), "app")
	if err != nil || p.Origins == nil || len(p.Origins) != 0 {
		t.Fatalf("migration default: %+v %v", p, err)
	}
	origins := []string{"https://api.example.com"}
	if err := s.SetNetworkPolicy(t.Context(), "app", NetworkPolicy{Origins: origins, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	vars, _, snapshot, err := s.RuntimeSettingsSnapshot(t.Context(), "app")
	if err != nil || len(vars) != 1 || vars[0].Value != "original" || !slices.Equal(snapshot, origins) {
		t.Fatalf("snapshot: %+v %v %v", vars, snapshot, err)
	}
	if err := s.RenameFlat(t.Context(), "app", "renamed", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	p, err = s.NetworkPolicy(t.Context(), "renamed")
	if err != nil || !slices.Equal(p.Origins, origins) || !p.UpdatedAt.Equal(now) {
		t.Fatalf("rename: %+v %v", p, err)
	}
	if err := s.DeleteFlat(t.Context(), "renamed"); err != nil {
		t.Fatal(err)
	}
	p, err = s.NetworkPolicy(t.Context(), "renamed")
	if err != nil || len(p.Origins) != 0 {
		t.Fatalf("cascade: %+v %v", p, err)
	}
}
