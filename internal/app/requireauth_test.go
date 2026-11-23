package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

// docs/spec/portal-require-auth.md, rule 10: GET /api/status holds the value of portal.require_auth and the flats that it
// refuses. The list is empty with the setting off, and it is null when the host cannot read the store, so that the status
// still answers.
func TestStatusMembersOfRequireAuth(t *testing.T) {
	dir := t.TempDir()
	h, _ := consoleHostWith(t, dir, nil)
	ctx := context.Background()
	// Portal permitted for the host: turning it on contacts nothing until a flat allows it.
	if _, err := h.SetNetworkProvider(ctx, "portal", true, ""); err != nil {
		t.Fatal(err)
	}
	members := func() (on bool, refused string) {
		t.Helper()
		b, err := json.Marshal(h.Status(ctx))
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			On      bool            `json:"portal_require_auth"`
			Refused json.RawMessage `json:"portal_refused"`
		}
		if err := json.Unmarshal(b, &st); err != nil || st.Refused == nil {
			t.Fatalf("the status holds no portal_refused: %s, %v", b, err)
		}
		return st.On, strings.TrimSpace(string(st.Refused))
	}
	if on, refused := members(); on || refused != "[]" {
		t.Fatalf("the setting is off: portal_require_auth %t, portal_refused %s", on, refused)
	}
	if _, err := h.Svc.UpdateSettings(ctx, map[string]string{core.SetRequireAuth: "true"}); err != nil {
		t.Fatal(err)
	}
	if on, refused := members(); !on || refused != "[]" {
		t.Fatalf("the setting is on and no flat is refused: portal_require_auth %t, portal_refused %s", on, refused)
	}

	// With the store failing, the status still answers, and the list is null.
	db, err := sql.Open("sqlite", filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE flats RENAME TO flats_gone`); err != nil {
		t.Fatal(err)
	}
	if on, refused := members(); !on || refused != "null" {
		t.Fatalf("the store fails: portal_require_auth %t, portal_refused %s, want true and null", on, refused)
	}
}
