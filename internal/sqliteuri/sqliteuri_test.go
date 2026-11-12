package sqliteuri

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// The URI must open the same file in a directory with characters that URI
// syntax treats specially.
func TestFileOpensPathsWithURISyntax(t *testing.T) {
	for _, name := range []string{"plain.db", "with space.db", "per%cent.db"} {
		path := filepath.Join(t.TempDir(), name)
		db, err := sql.Open("sqlite", File(path))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("CREATE TABLE t (n INTEGER)"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		db.Close()
		if matches, _ := filepath.Glob(path); len(matches) != 1 {
			t.Fatalf("%s was not created at its path", name)
		}
	}
}
