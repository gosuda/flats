package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

func newSheet(t *testing.T, s *Service, slugName string) *sheet.Instance {
	t.Helper()
	if _, err := s.CreateSheet(context.Background(), slugName, "Budget", ViaMCP); err != nil {
		t.Fatal(err)
	}
	in := s.liveSheet(slugName)
	if in == nil {
		t.Fatal("created sheet is not running")
	}
	return in
}

// sheetTestWait bounds the waits of the sheet tests for something the
// sheet or the host does on its own: a tick, a load, a closed connection.
// It is long, for slow test machines; the waits end as soon as they can.
const sheetTestWait = 60 * time.Second

// sheetRead reads a sheet, or one of its revisions when rev is positive.
func sheetRead(t *testing.T, s *Service, slugName string, rev int, query string) map[string]any {
	t.Helper()
	data, err := s.GetSheet(context.Background(), slugName, rev, []byte(query))
	for deadline := time.Now().Add(sheetTestWait); sheetCalculating(err) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		data, err = s.GetSheet(context.Background(), slugName, rev, []byte(query))
	}
	if err != nil {
		t.Fatalf("get_sheet %s: %v", query, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sheetState(t *testing.T, s *Service, slugName string) string {
	t.Helper()
	state, _ := sheetRead(t, s, slugName, 0, `{}`)["state"].(string)
	if state == "" {
		t.Fatal("the sheet read has no state")
	}
	return state
}

func sheetResult(t *testing.T, out SheetWriteResult) sheet.WriteResult {
	t.Helper()
	var result sheet.WriteResult
	if err := json.Unmarshal(out.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Created != out.Created || result.RevisionPending != out.Pending ||
		(result.Revision == nil) != (out.Revision == 0) ||
		(result.Revision != nil && *result.Revision != out.Revision) {
		t.Fatalf("inconsistent write envelope: %+v / %+v", out, result)
	}
	return result
}

// writeSheet applies ops against the current state of the sheet.
func writeSheet(t *testing.T, s *Service, slugName, opID, ops string) (SheetWriteResult, sheet.WriteResult) {
	t.Helper()
	body := fmt.Sprintf(`{"state":%q,"op_id":%q,"ops":[%s]}`, sheetState(t, s, slugName), opID, ops)
	out, err := s.UpdateSheet(context.Background(), slugName, []byte(body), ViaMCP)
	for deadline := time.Now().Add(sheetTestWait); sheetCalculating(err) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		out, err = s.UpdateSheet(context.Background(), slugName, []byte(body), ViaMCP)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out, sheetResult(t, out)
}

const sheetBudgetRows = `{"op":"write","tab":"Sheet1","at":"A1","rows":[["Item","Amount"],["Venue",1200],["Food",900],["Total","=SUM(B2:B3)"]]}`

const sheetCurrencyFormat = `{"op":"format","tab":"Sheet1","ranges":["B2:B4"],"format":{"number":{"kind":"currency","symbol":"$","decimals":2}}}`

func TestSheetCreateWriteAndRead(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	view, err := s.CreateSheet(ctx, "budget", "  Offsite   budget ", ViaMCP)
	if err != nil || view.Name != "Offsite budget" || view.Type != "sheet" || !view.Running || view.Visibility != store.Private {
		t.Fatalf("create = %+v, %v", view, err)
	}
	if view.LiveVersion != 0 || view.Draft != nil || view.Publication != "" || view.Versions != 0 {
		t.Fatalf("a sheet reports version state: %+v", view)
	}
	if _, err := s.CreateSheet(ctx, "budget", "Again", ViaMCP); !errors.Is(err, ErrConflict) {
		t.Fatalf("second create = %v", err)
	}
	page, err := s.ListRevisions(ctx, "budget", 0, 10)
	if err != nil || len(page.Revisions) != 1 || page.Revisions[0].Kind != "created" || len(page.Revisions[0].SheetTabs) != 1 || page.Revisions[0].SheetTabs[0].Change != "added" || page.Revisions[0].SheetTabs[0].NameAfter != "Sheet1" {
		t.Fatalf("first revision = %+v, %v", page, err)
	}
	body := []byte(fmt.Sprintf(`{"state":%q,"op_id":"rows-1","ops":[%s]}`, sheetState(t, s, "budget"), sheetBudgetRows))
	out, err := s.UpdateSheet(ctx, "budget", body, ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	result := sheetResult(t, out)
	if out.Revision != 2 || out.Pending || out.Created || result.CellsWritten != 8 || result.FormulasWritten != 1 || result.FormulaErrors != 0 {
		t.Fatalf("write = %+v / %+v", out, result)
	}
	read := sheetRead(t, s, "budget", 0, `{"tab":"Sheet1","range":"A4:B4"}`)
	rows, _ := read["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("read = %+v", read)
	}
	total, _ := rows[0].([]any)[1].(map[string]any)
	if total["formula"] != "=SUM(B2:B3)" || total["value"] != float64(2100) {
		t.Fatalf("total = %+v", total)
	}
	csv := sheetRead(t, s, "budget", 0, `{"tab":"Sheet1","format":"csv"}`)
	if text, _ := csv["csv"].(string); !strings.Contains(text, "Total,2100") {
		t.Fatalf("csv = %+v", csv)
	}
	replay, err := s.UpdateSheet(ctx, "budget", body, ViaMCP)
	if r := sheetResult(t, replay); err != nil || !r.Replay || replay.Revision != 2 {
		t.Fatalf("replay = %+v / %+v, %v", replay, r, err)
	}
	reused := []byte(fmt.Sprintf(`{"state":%q,"op_id":"rows-1","ops":[]}`, read["state"]))
	if _, err := s.UpdateSheet(ctx, "budget", reused, ViaMCP); !errors.Is(err, ErrConflict) || ErrorCategory(err) != "conflict" {
		t.Fatalf("op id reuse = %v", err)
	}
	if page, err := s.ListRevisions(ctx, "budget", 0, 10); err != nil || len(page.Revisions) != 2 {
		t.Fatalf("a replay or a refusal recorded a revision: %+v, %v", page, err)
	}
}

// TestSheetCreatingWriteReturnsItsAddress covers the write that creates a
// sheet: its result names where the sheet opens, the Public address alone
// while public instance mode is on.
func TestSheetCreatingWriteReturnsItsAddress(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprintf("public-instance-%t", public), func(t *testing.T) {
			s, _ := piCreationService(t, public)
			out, err := s.UpdateSheet(t.Context(), "created", []byte(`{"ops":[`+sheetBudgetRows+`]}`), ViaMCP)
			if err != nil || !out.Created || out.Flat == nil {
				t.Fatalf("creating write = %+v, %v", out, err)
			}
			f := out.Flat
			if f.Slug != "created" || f.Type != "sheet" || !f.Running {
				t.Fatalf("created flat = %+v", f)
			}
			if public {
				if f.Visibility != store.Public || f.ConnectionState != "ready" || f.PublicURL == "" || f.PrivateURL != "" {
					t.Fatalf("the creating write returns %+v, want the ready Public address and no private link", f)
				}
			} else if f.Visibility != store.Private || f.PrivateURL == "" || f.PublicURL != "" {
				t.Fatalf("the creating write returns %+v, want the Private address", f)
			}
			next, err := s.UpdateSheet(t.Context(), "created", []byte(fmt.Sprintf(`{"state":%q,"ops":[]}`, sheetResult(t, out).State)), ViaMCP)
			if err != nil || next.Created || next.Flat != nil {
				t.Fatalf("a write to the existing sheet = %+v, %v", next, err)
			}
		})
	}
}
