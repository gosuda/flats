package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEnvLifecycleAndCollisions(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, slug := range []string{"a", "b"} {
		if err := s.CreateFlat(ctx, Flat{Slug: slug, Name: slug, Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutEnv(ctx, "a", EnvVar{Name: "MODE", Value: "one", UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEnv(ctx, "a", EnvVar{Name: "MODE", Value: "two", UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if vars, err := s.ListEnv(ctx, "b"); err != nil || len(vars) != 0 {
		t.Fatalf("isolation: %v %v", vars, err)
	}
	if err := s.PutSecret(ctx, "a", SealedSecret{Name: "MODE", Nonce: []byte{1}, Ciphertext: []byte{2}, UpdatedAt: now}); !errors.Is(err, ErrEnvCollision) {
		t.Fatalf("secret collision: %v", err)
	}
	if err := s.PutSecret(ctx, "a", SealedSecret{Name: "TOKEN", Nonce: []byte{1}, Ciphertext: []byte{2}, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEnv(ctx, "a", EnvVar{Name: "TOKEN", Value: "ordinary", UpdatedAt: now}); !errors.Is(err, ErrEnvCollision) {
		t.Fatalf("env collision: %v", err)
	}
	if err := s.RenameFlat(ctx, "a", "renamed", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	vars, err := s.ListEnv(ctx, "renamed")
	if err != nil || len(vars) != 1 || vars[0].Value != "two" || !vars[0].UpdatedAt.Equal(now) {
		t.Fatalf("renamed: %+v %v", vars, err)
	}
	if err := s.DeleteFlat(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	if vars, err := s.ListEnv(ctx, "renamed"); err != nil || len(vars) != 0 {
		t.Fatalf("cascade: %v %v", vars, err)
	}
}

func TestConcurrentEnvSecretCollisionAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	now := time.Now()
	if err := a.CreateFlat(t.Context(), Flat{Slug: "app", Name: "app", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	start := make(chan struct{})
	var envErr, secretErr error
	go func() {
		defer wg.Done()
		<-start
		envErr = a.PutEnv(t.Context(), "app", EnvVar{Name: "SHARED", Value: "plain", UpdatedAt: now})
	}()
	go func() {
		defer wg.Done()
		<-start
		secretErr = b.PutSecret(t.Context(), "app", SealedSecret{Name: "SHARED", Nonce: []byte{1}, Ciphertext: []byte{2}, UpdatedAt: now})
	}()
	close(start)
	wg.Wait()
	if !((envErr == nil && errors.Is(secretErr, ErrEnvCollision)) || (secretErr == nil && errors.Is(envErr, ErrEnvCollision))) {
		t.Fatalf("writes: env=%v secret=%v", envErr, secretErr)
	}
}

func TestEnvMigrationPreservesSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flats.db")
	withMigrations(t, migrations[:2])
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.CreateFlat(t.Context(), Flat{Slug: "app", Name: "app", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(t.Context(), "app", SealedSecret{Name: "TOKEN", Nonce: []byte{1}, Ciphertext: []byte{2}, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	migrations = append(migrations, migration{7, "application environment variables", addEnvVars})
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := Inspect(path)
	if err != nil || info.Version != 7 {
		t.Fatalf("migration: %+v %v", info, err)
	}
	secs, err := s.ListSecrets(t.Context(), "app")
	if err != nil || len(secs) != 1 || string(secs[0].Ciphertext) != string([]byte{2}) {
		t.Fatalf("secrets: %+v %v", secs, err)
	}
	if err := s.PutEnv(t.Context(), "app", EnvVar{Name: "TOKEN", Value: "no", UpdatedAt: now}); !errors.Is(err, ErrEnvCollision) {
		t.Fatalf("migration collision: %v", err)
	}
}

func TestEnvironmentSnapshotReadsNamespacesTogether(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	now := time.Now()
	if err := s.CreateFlat(ctx, Flat{Slug: "app", Name: "app", Visibility: Private, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEnv(ctx, "app", EnvVar{Name: "MODE", Value: "a", UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret(ctx, "app", SealedSecret{Name: "TOKEN", Nonce: []byte{1}, Ciphertext: []byte("a"), UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNetworkPolicy(ctx, "app", NetworkPolicy{Origins: []string{"a"}, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			value := []string{"a", "b"}[i%2]
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				done <- err
				return
			}
			for _, update := range []struct {
				query string
				value any
			}{
				{`UPDATE env_vars SET value=? WHERE flat='app'`, value},
				{`UPDATE secrets SET ciphertext=? WHERE flat='app'`, []byte(value)},
				{`UPDATE network_settings SET origins=? WHERE flat='app'`, `["` + value + `"]`},
			} {
				if _, err = tx.ExecContext(ctx, update.query, update.value); err != nil {
					tx.Rollback()
					done <- err
					return
				}
			}
			if err := tx.Commit(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 100; i++ {
		vars, secs, origins, err := s.RuntimeSettingsSnapshot(ctx, "app")
		if err != nil {
			t.Fatal(err)
		}
		if len(vars) != 1 || len(secs) != 1 || vars[0].Value != string(secs[0].Ciphertext) || len(origins) != 1 || origins[0] != vars[0].Value {
			t.Fatalf("split snapshot: %v %v", vars, secs)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	vars, secs, err := s.EnvironmentSnapshot(ctx, "other")
	if err != nil || len(vars) != 0 || len(secs) != 0 {
		t.Fatalf("cross-flat snapshot: %v %v %v", vars, secs, err)
	}
}
