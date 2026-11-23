package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
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

	// A Public flat with a live version and a Portal grant but no sign-in is refused, so the list names it.
	now := time.Now()
	if err := h.Store.CreateFlatRecorded(ctx, store.Flat{Slug: "notes", Name: "notes", Visibility: store.Public, LiveVersion: 1, CreatedAt: now, UpdatedAt: now},
		[]string{store.ProviderPortal}, nil); err != nil {
		t.Fatal(err)
	}
	if on, refused := members(); !on || refused != `["notes"]` {
		t.Fatalf("a refused flat: portal_require_auth %t, portal_refused %s", on, refused)
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

// Rule 10: Portal serves no flat while it is not permitted for the host, so the list is empty then, whatever the flats hold.
func TestStatusRefusedListNeedsPortalOnTheHost(t *testing.T) {
	h, _ := consoleHostWith(t, t.TempDir(), nil)
	ctx := context.Background()
	now := time.Now()
	if err := h.Store.CreateFlatRecorded(ctx, store.Flat{Slug: "notes", Name: "notes", Visibility: store.Public, LiveVersion: 1, CreatedAt: now, UpdatedAt: now},
		[]string{store.ProviderPortal}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Svc.UpdateSettings(ctx, map[string]string{core.SetRequireAuth: "true"}); err != nil {
		t.Fatal(err)
	}
	refused := func() string {
		t.Helper()
		b, err := json.Marshal(h.Status(ctx))
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Refused json.RawMessage `json:"portal_refused"`
		}
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(st.Refused))
	}
	if got := refused(); got != "[]" {
		t.Fatalf("Portal is not permitted for the host and portal_refused is %s", got)
	}
	if _, err := h.SetNetworkProvider(ctx, "portal", true, ""); err != nil {
		t.Fatal(err)
	}
	if got := refused(); got != `["notes"]` {
		t.Fatalf("Portal is permitted for the host and portal_refused is %s", got)
	}
}
