package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicInstanceCreationMarkMigrationAndLifetime(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flats.db")
	db, err := sql.Open("sqlite", fileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > 22 {
			break
		}
		if err := runMigration(ctx, db, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE flats DROP COLUMN created_public_instance`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO flats(slug,name,visibility,created_at,updated_at) VALUES('old','Old','public',0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.GetFlat(ctx, "old")
	if err != nil || f.CreatedPublicInstance {
		t.Fatalf("migration marked existing Public flat: %+v %v", f, err)
	}
	now := time.Now()
	if err := s.CreateFlatRecorded(ctx, Flat{Slug: "marked", Name: "Marked", Visibility: Public, CreatedPublicInstance: true, CreatedAt: now, UpdatedAt: now}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameFlat(ctx, "marked", "renamed", time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, err = s.GetFlat(ctx, "renamed")
	if err != nil || !f.CreatedPublicInstance {
		t.Fatalf("lost mark on rename/restart: %+v %v", f, err)
	}
	info, err := Inspect(path)
	if err != nil || info.Version != 23 {
		t.Fatalf("schema: %+v %v", info, err)
	}
}
