package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// uEscape is the JSON escape of r: a backslash, the letter u and four hex digits.
func uEscape(r rune) string { return fmt.Sprintf("%cu%04x", 92, r) }

// `flats audit ls --json` prints a line that the output filter of the command line leaves as it is: a character that the
// filter rewrites is a JSON escape in the line, and a reader of the JSON gets the stored text back. The export has the
// same rule (TestAuditExportLinesPassTheFilterOfTheCommandLineUnchanged).
func TestAuditLsJSONEscapesWhatTheTerminalFilterRewrites(t *testing.T) {
	dir := t.TempDir()
	config := initConfig(t, dir)
	// A C1 control, a bidi override and DEL. They are built from their numbers so that this file holds none of them.
	rewritten := []rune{0x85, 0x202e, 0x7f}
	actor := "ann" + string(rewritten[0]) + string(rewritten[1]) + "bob" + string(rewritten[2]) + " end"
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendAudit(context.Background(), store.AuditEvent{Kind: store.AuditKeyBackup, Via: store.AuditViaCommand, Actor: actor}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := runAudit(t, config, "--json")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rewritten {
		if strings.ContainsRune(out, r) {
			t.Errorf("the line holds the character %U as it is: %q", r, out)
		}
		if !strings.Contains(out, uEscape(r)) {
			t.Errorf("the line has no escape of %U: %q", r, out)
		}
	}
	rows := jsonLines(t, out)
	if len(rows) != 1 || rows[0]["actor"] != actor {
		t.Fatalf("decoded actor = %q, want %q", rows[0]["actor"], actor)
	}
}

// A JSON text without such a character is returned without a copy.
func TestJSONForTerminalCopiesOnlyWhenItEscapes(t *testing.T) {
	t.Parallel()
	clean := []byte(`{"actor":"ann é 한글 😀","n":1}`)
	if got := jsonForTerminal(clean); &got[0] != &clean[0] || string(got) != string(clean) {
		t.Errorf("a clean text was copied or changed: %q", got)
	}
	c1 := `{"a":"x` + string(rune(0x9f)) + `y"}`
	if got, want := string(jsonForTerminal([]byte(c1))), `{"a":"x`+uEscape(0x9f)+`y"}`; got != want {
		t.Errorf("a C1 control = %q, want %q", got, want)
	}
	notUTF8 := append(append([]byte(`{"a":"x`), 0xff), `y"}`...)
	if got, want := string(jsonForTerminal(notUTF8)), `{"a":"x`+uEscape(0xfffd)+`y"}`; got != want {
		t.Errorf("a byte that is not UTF-8 = %q, want %q", got, want)
	}
}
