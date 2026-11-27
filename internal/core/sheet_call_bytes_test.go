package core

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestSheetCallBytes sends update_sheet bodies whose bytes name a field
// twice or in another case, as a request carries them. The sheet reads the
// bytes itself and refuses each as invalid: none of them is settled by
// which of two fields a reader in front of the sheet happens to keep, and
// nothing of them is applied. A field name written with a JSON escape is
// the name it spells.
func TestSheetCallBytes(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.CreateSheet(ctx, "budget", "Budget", ViaMCP); err != nil {
		t.Fatal(err)
	}
	add := `{"op":"add_tab","name":"Extra"}`
	tabs := func() int {
		list, _ := sheetRead(t, s, "budget", 0, `{}`)["tabs"].([]any)
		return len(list)
	}
	for _, body := range []string{
		`{"ops":[],"OPS":[` + add + `]}`,
		`{"OPS":[` + add + `],"ops":[]}`,
		`{"ops":[],"ops":[` + add + `]}`,
		`{"ops":[` + add + `],"ops":[]}`,
		`{"ops":[` + add + `],"Ops":[` + add + `,` + add + `]}`,
		`{"SLUG":"other","ops":[` + add + `]}`,
		`{"ops":[{"op":"add_tab","name":"Extra","name":"More"}]}`,
		`{"ops":[{"op":"add_tab","OP":"delete_tab","name":"Extra"}]}`,
		fmt.Sprintf(`{"state":%q,"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[[{"formula":"=1","formula":"=2"}]]}]}`, sheetState(t, s, "budget")),
		fmt.Sprintf(`{"state":%q,"ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true,"bold":false}}]}`, sheetState(t, s, "budget")),
	} {
		state := sheetState(t, s, "budget")
		_, err := s.UpdateSheet(ctx, "budget", []byte(body), ViaAPI)
		if !errors.Is(err, ErrInvalid) || ErrorCategory(err) != "invalid" {
			t.Errorf("%s: %v (%s), want an invalid call", body, err, ErrorCategory(err))
		}
		if sheetState(t, s, "budget") != state || tabs() != 1 {
			t.Fatalf("%s changed the sheet", body)
		}
	}
	// A slug in the body is refused in every spelling that names it.
	for _, body := range []string{`{"slug":"other","ops":[]}`, `{"sl\u0075g":"other","ops":[]}`, `{"slug":"","ops":[]}`} {
		if _, err := s.UpdateSheet(ctx, "budget", []byte(body), ViaAPI); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want it refused for its slug", body, err)
		}
	}
	if _, err := s.UpdateSheet(ctx, "budget", []byte(`{"\u006fps":[{"o\u0070":"add_tab","n\u0061me":"Escaped"}]}`), ViaAPI); err != nil || tabs() != 2 {
		t.Fatalf("a call with escaped field names: %v, %d tabs", err, tabs())
	}
}
