package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestRuntimeGenerationConcurrentAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	generations := make(chan int64, 20)
	for range 20 {
		wg.Go(func() {
			n, err := s.NextRuntimeGeneration(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			generations <- n
		})
	}
	wg.Wait()
	close(generations)
	seen := map[int64]bool{}
	var highest int64
	for n := range generations {
		if n <= 0 || seen[n] {
			t.Fatal("invalid generation", n)
		}
		seen[n] = true
		highest = max(highest, n)
	}
	if len(seen) != 20 {
		t.Fatal(seen)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := s.NextRuntimeGeneration(t.Context())
	if err != nil || n != highest+1 {
		t.Fatal(n, highest, err)
	}
}

// Main's schema 6 has no generation table; the old docs branch's schema 5
// may already have one. Both upgrade paths must preserve activation ordering.
func TestRuntimeGenerationUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name       string
		version    int
		generation int64
	}{
		{name: "main schema 6", version: 6},
		{name: "docs schema 5", version: 5, generation: 123},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "host.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.generation == 0 {
				_, err = s.db.Exec(`DROP TABLE runtime_generation`)
			} else {
				_, err = s.db.Exec(`INSERT INTO runtime_generation(id,generation) VALUES(1,?)`, tc.generation)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, tc.version)); err != nil {
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
			n, err := s.NextRuntimeGeneration(t.Context())
			if err != nil || n != tc.generation+1 {
				t.Fatalf("generation %d, want %d: %v", n, tc.generation+1, err)
			}
		})
	}
}
