package notebook

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

const spillOptionBatch = 256

type spillFixtureOption struct {
	ID   ID     `json:"id"`
	Name string `json:"name"`
}

func spillOptionValues(ids []ID, start int, name func(int) string) []spillFixtureOption {
	options := make([]spillFixtureOption, len(ids))
	for i, id := range ids {
		options[i] = spillFixtureOption{ID: id, Name: name(start + i)}
	}
	return options
}

// Fresh options need no update, deletion or group-version bookkeeping.
func spillOptions(t *testing.T, tx *sql.Tx, property ID, ids []ID, name func(int) string) error {
	t.Helper()
	for start := 0; start < len(ids); start += spillOptionBatch {
		batch := ids[start:min(start+spillOptionBatch, len(ids))]
		raw, err := json.Marshal(spillOptionValues(batch, start, name))
		if err != nil {
			return err
		}
		encoded := string(raw)
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO allocated_ids(id,kind) SELECT json_extract(value,'$.id'),'option' FROM json_each(?)`, encoded); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO items(id,kind,inc,alive,deleted_seq,own_version,aggregate_version,data) SELECT json_extract(value,'$.id'),'option','7',1,'0','7','7',json_object('property',?,'name',json_extract(value,'$.name'),'color','gray') FROM json_each(?)`, property, encoded); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO options(id,property_id,name_key,alive) SELECT json_extract(value,'$.id'),?,json_extract(value,'$.name'),1 FROM json_each(?)`, property, encoded); err != nil {
			return err
		}
	}
	return nil
}

type spillFixtureState struct {
	Journal     string   `json:"journal_mode"`
	ForeignKeys int      `json:"foreign_keys"`
	Synchronous int      `json:"synchronous"`
	Versions    []string `json:"versions"`
}

func spillFixtureStatus(t *testing.T, db *sql.DB) (spillFixtureState, error) {
	t.Helper()
	var s spillFixtureState
	for _, q := range []struct {
		sql string
		out any
	}{{`PRAGMA journal_mode`, &s.Journal}, {`PRAGMA foreign_keys`, &s.ForeignKeys}, {`PRAGMA synchronous`, &s.Synchronous}} {
		if err := db.QueryRowContext(t.Context(), q.sql).Scan(q.out); err != nil {
			return s, err
		}
	}
	rows, err := db.QueryContext(t.Context(), `SELECT CAST(value AS TEXT) FROM json_each('["18446744073709551615","100","10","1","99","9","2"]') ORDER BY CAST(value AS TEXT) COLLATE notebook_u64`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return s, err
		}
		s.Versions = append(s.Versions, v)
	}
	return s, rows.Err()
}

// Bulk SQL fixture setup is not the reader under test. Keep its SQLite byte
// loops out of the race binary; Instance.Read and all its oracles stay in it.
func spillExternalOptions(t *testing.T, in *Instance, property ID, ids []ID, name func(int) string) error {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "option-fixture")
	// Make the test cache observe source otherwise read only by the child build.
	if _, err := os.ReadFile(filepath.Join("testdata", "reader-options", "main.go")); err != nil {
		return fmt.Errorf("fixture source: %w", err)
	}
	// PATH may name an older bootstrap Go that auto-selected this test's SDK.
	build := exec.CommandContext(t.Context(), filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-race=false", "-mod=readonly", "-o", binary, "./testdata/reader-options")
	build.Env = append(os.Environ(), "GOROOT="+runtime.GOROOT(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build option fixture: %w: %s", err, out)
	}
	input, err := os.CreateTemp(dir, "options-")
	if err != nil {
		return fmt.Errorf("fixture input: %w", err)
	}
	defer input.Close()
	writer := bufio.NewWriterSize(input, 64<<10)
	encoder := json.NewEncoder(writer)
	for start := 0; start < len(ids); start += spillOptionBatch {
		batch := ids[start:min(start+spillOptionBatch, len(ids))]
		if err := encoder.Encode(spillOptionValues(batch, start, name)); err != nil {
			return fmt.Errorf("fixture input: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("fixture input: %w", err)
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("fixture input: %w", err)
	}
	want, err := spillFixtureStatus(t, in.db.db)
	if err != nil {
		return fmt.Errorf("fixture state: %w", err)
	}
	cmd := exec.CommandContext(t.Context(), binary, in.db.path, string(property))
	cmd.Stdin = input
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("seed options: %w: %s", err, diagnostic.String())
	}
	var got spillFixtureState
	if err := json.Unmarshal(out, &got); err != nil {
		return fmt.Errorf("fixture receipt: %w", err)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("fixture connection changed: got %+v, want %+v", got, want)
	}
	return nil
}

func TestSpillOptionsMatchStoredRecords(t *testing.T) {
	t.Parallel()
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprintf("external=%t", external), func(t *testing.T) {
			f, created := readerQueryFixture(t)
			property := created.Refs["tags"].Ref.ID
			ids := make([]ID, spillOptionBatch+1)
			names := make([]string, len(ids))
			for i := range ids {
				ids[i] = spillID(OptionKind, uint64(i+100))
				names[i] = fmt.Sprintf("choice%d \"quoted\" 한😀\n", i)
			}
			name := func(i int) string { return names[i] }
			if external {
				if err := spillExternalOptions(t, f.in, property, ids, name); err != nil {
					t.Fatal(err)
				}
			} else if err := f.in.db.transaction(t.Context(), func(tx *sql.Tx) error { return spillOptions(t, tx, property, ids, name) }); err != nil {
				t.Fatal(err)
			}
			var integrity string
			if err := f.in.db.db.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&integrity); err != nil {
				t.Fatal(err)
			}
			if integrity != "ok" {
				t.Fatalf("fixture integrity: %s", integrity)
			}
			if err := f.in.db.transaction(t.Context(), func(tx *sql.Tx) error {
				for _, query := range []string{
					`SELECT count(*) FROM allocated_ids WHERE kind='option'`,
					`SELECT count(*) FROM items WHERE kind='option'`,
					`SELECT count(*) FROM options`,
				} {
					var count int
					if err := tx.QueryRowContext(t.Context(), query).Scan(&count); err != nil {
						return err
					}
					if count != len(ids)+2 {
						t.Fatalf("seeded option count=%d, want %d: %s", count, len(ids)+2, query)
					}
				}
				for i, id := range ids {
					want := spillItem(id, Option{Property: property, Name: names[i], Color: Gray})
					got, err := loadRecord(t.Context(), tx, id)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(got, itemRecord{Item: want, Alive: true, Aggregate: 7}) {
						t.Fatalf("seeded option differs from stored item: %+v", got)
					}
					var kind, storedName string
					if err := tx.QueryRowContext(t.Context(), `SELECT a.kind,o.name_key FROM allocated_ids a JOIN options o ON o.id=a.id WHERE a.id=? AND o.property_id=? AND o.alive=1`, id, property).Scan(&kind, &storedName); err != nil {
						return err
					}
					if kind != string(OptionKind) || storedName != names[i] {
						t.Fatalf("seeded option indexes differ: %q %q", kind, storedName)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
