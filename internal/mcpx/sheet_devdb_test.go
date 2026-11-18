package mcpx

import (
	"crypto/sha256"
	"database/sql"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/sqliteuri"
)

// TestSheetOfADevelopmentBuildIsRefused starts a host on a sheet whose
// sheet.db an unreleased development build wrote: one without the session
// index of this release, and one without the release mark that has the
// schema of this release. The sheet does not start. get_sheet and
// update_sheet through MCP and the read of the management API refuse with
// the cause, and get_logs names it too. Nothing in the sheet's directory
// changes.
func TestSheetOfADevelopmentBuildIsRefused(t *testing.T) {
	for name, query := range map[string]string{
		"without the session index": `DROP INDEX sessions_last`,
		"without the release mark":  `PRAGMA application_id = 0`,
	} {
		t.Run(name, func(t *testing.T) { developmentSheetRefused(t, query) })
	}
}

func developmentSheetRefused(t *testing.T, query string) {
	dir := t.TempDir()
	first := newEnvIn(t, dir, nil)
	if text, bad := call(t, first.local, "update_sheet", map[string]any{"slug": "budget", "ops": []any{map[string]any{"op": "write", "tab": "Sheet1", "at": "A1", "rows": []any{[]any{1}}}}}, nil); bad {
		t.Fatalf("create: %s", text)
	}
	first.close()
	sheetDir := filepath.Join(dir, "flats", "budget", "sheet")
	db, err := sql.Open("sqlite", sqliteuri.File(filepath.Join(sheetDir, "sheet.db")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	files := func() map[string][sha256.Size]byte {
		t.Helper()
		entries, err := os.ReadDir(sheetDir)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][sha256.Size]byte{}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(sheetDir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[entry.Name()] = sha256.Sum256(data)
		}
		return out
	}
	before := files()

	e := newEnvIn(t, dir, nil)
	const cause = "development build"
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"get_sheet", map[string]any{"slug": "budget", "tab": "Sheet1"}},
		{"update_sheet", map[string]any{"slug": "budget", "ops": []any{map[string]any{"op": "add_tab", "name": "Extra"}}}},
	} {
		text, bad := call(t, e.local, tc.tool, tc.args, nil)
		if !bad || !strings.Contains(text, `"category":"unavailable"`) || !strings.Contains(text, cause) {
			t.Errorf("%s: %s", tc.tool, text)
		}
	}
	res, err := http.Get(e.management.URL + "/api/flats/budget/sheet?tab=Sheet1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), cause) {
		t.Errorf("the API read: %d %s", res.StatusCode, body)
	}
	if text, bad := call(t, e.local, "get_logs", map[string]any{"slug": "budget"}, nil); bad || !strings.Contains(text, cause) {
		t.Errorf("get_logs: %s", text)
	}
	after := files()
	if len(after) != len(before) {
		t.Fatalf("the sheet's directory holds %d files after the refusals, %d before", len(after), len(before))
	}
	for name, sum := range before {
		if after[name] != sum {
			t.Fatalf("the refusals changed %s", name)
		}
	}
}
