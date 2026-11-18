package runtimeref

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestSheetTopicIsRoutedAndReferenced(t *testing.T) {
	md, ok := Topic("sheet")
	if !ok || !strings.HasPrefix(md, "## Sheets\n") {
		t.Fatal("topic.sheet must exist and start with its heading")
	}
	index, _ := Topic("index")
	for _, want := range []string{"`topic.sheet`", "`get_sheet`", "`update_sheet`", "`create_flat {type: \"sheet\"}`"} {
		if !strings.Contains(index, want) {
			t.Errorf("topic.index does not route with %s", want)
		}
	}
	if !strings.Contains(ContentTypesMarkdown, md) || !strings.Contains(ContentTypesMarkdown, "| `sheet` |") {
		t.Error("the content-types reference must carry the sheet row and topic")
	}
	if strings.Contains(Markdown, "## Sheets") {
		t.Error("the runtime reference describes server flats, not sheets")
	}
	for _, name := range []string{"docs", "canvas", "board", "page", "notebook"} {
		if other, _ := Topic(name); !strings.Contains(other, "`topic.sheet`") {
			t.Errorf("topic.%s's type chooser omits topic.sheet", name)
		}
	}
	for _, name := range []string{"notebook", "docs", "page"} {
		if !strings.Contains(md, "`topic."+name+"`") {
			t.Errorf("topic.sheet's type chooser omits topic.%s", name)
		}
	}
}

// The topic names every agent operation of section 18.2 of the technical
// design, the selectors of a read, every function and error of the product
// spec, and the refusals agents receive from sheet tools.
func TestSheetTopicCoversOperationsFunctionsAndRefusals(t *testing.T) {
	md, _ := Topic("sheet")
	for _, op := range []string{
		"write", "clear", "format", "widths", "insert_rows", "delete_rows", "insert_cols", "delete_cols",
		"sort", "add_tab", "rename_tab", "move_tab", "delete_tab", "tab_settings", "import_csv",
	} {
		if !strings.Contains(md, "`"+op+"`") {
			t.Errorf("topic.sheet does not name operation %s", op)
		}
	}
	for _, field := range []string{"`state`", "`op_id`", "`guard`", "`include`", "`cursor`", "`limit`", "`format: \"csv\"`", "`formula_errors`", "`replay: true`"} {
		if !strings.Contains(md, field) {
			t.Errorf("topic.sheet does not explain %s", field)
		}
	}
	for _, name := range []string{
		"ABS", "SUMPRODUCT", "TRUNC", "AVERAGE", "VARP", "AVERAGEIF", "SUMIFS", "AND", "XOR", "CONCAT", "VALUE",
		"CHOOSE", "XLOOKUP", "DATE", "TODAY", "YEAR",
	} {
		if !strings.Contains(md, "`"+name+"`") {
			t.Errorf("topic.sheet does not list function %s", name)
		}
	}
	_, table, _ := strings.Cut(md, "The functions, by group.")
	table, _, _ = strings.Cut(table, "A formula's value is")
	if got := len(regexp.MustCompile("`[A-Z][A-Z0-9]*`").FindAllString(table, -1)); got != 104 {
		t.Errorf("topic.sheet lists %d functions, want the 104 of the product spec", got)
	}
	for _, e := range []string{"#ERROR!", "#NAME?", "#REF!", "#VALUE!", "#DIV/0!", "#NUM!", "#N/A", "#CYCLE!", "#LIMIT!"} {
		if !strings.Contains(md, "`"+e+"`") {
			t.Errorf("topic.sheet does not explain %s", e)
		}
	}
	for _, c := range []string{"not_sheet", "sheet_capacity"} {
		if !slices.Contains(RefusalCategories(), c) || !strings.Contains(md, c) {
			t.Errorf("refusal.%s must exist and be named by topic.sheet", c)
		}
	}
	// Every limit a sheet refusal can name is in the refusal text and in
	// the topic, which gives its bound.
	capacity, _ := Refusal("sheet_capacity")
	for _, limit := range []string{
		"tabs", "rows", "columns", "cells", "formulas", "text_length", "formula_length", "formula_depth",
		"formula_reads", "sheet_reads", "sheet_text", "call_cells", "call_bytes", "recalculation_time",
		"action_changes",
	} {
		if !strings.Contains(capacity, "`"+limit+"`") || !strings.Contains(md, "`"+limit+"`") {
			t.Errorf("limit %s is not named by refusal.sheet_capacity and topic.sheet", limit)
		}
	}
	conflict, _ := Refusal("conflict")
	for _, reason := range []string{"state_required", "state_unknown", "state_too_old", "tab_changed", "structure_changed", "rolled_back", "guard", "op_id_reuse", "read_expired", "get_sheet"} {
		if !strings.Contains(conflict, reason) {
			t.Errorf("refusal.conflict does not explain %s", reason)
		}
		if reason != "get_sheet" && !strings.Contains(md, "`"+reason+"`") {
			t.Errorf("topic.sheet does not name the conflict reason %s", reason)
		}
	}
	// Sheets have no anonymous editing: only board and docs offer it.
	approvals, _ := Topic("approvals")
	if strings.Contains(md, "public_access") || strings.Contains(approvals, "sheet") {
		t.Error("sheets must not inherit the board and docs anonymous-edit guidance")
	}
}

func TestSheetInstructionsStaySmall(t *testing.T) {
	for _, limit := range []int64{20 << 20, 1 << 40} {
		for name, ins := range map[string]string{
			"off": Instructions(limit), "autopilot": AutopilotInstructions(limit),
			"public": ServerInstructions(limit, false, true), "autopilot and public": ServerInstructions(limit, true, true),
		} {
			if n := len(ins); n > 2300 {
				t.Errorf("%s instructions are %d bytes at upload limit %d", name, n, limit)
			}
			if !strings.Contains(ins, "sheets and notebooks have no Draft, versions or publish") {
				t.Errorf("%s instructions omit the sheet exception", name)
			}
		}
	}
}

func TestSheetTopicOverlays(t *testing.T) {
	off, _ := Topic("sheet")
	if on, ok := AutopilotTopicOverlay("sheet"); !ok || on != off {
		t.Fatal("topic.sheet needs no Autopilot replacement: it never claims the operator decides approvals")
	}
	public := PublicInstanceTopicOverlay("sheet", off)
	for _, gone := range []string{"A new sheet is Private.", "`private_url`", "open History in the sheet"} {
		if strings.Contains(public, gone) {
			t.Errorf("the public instance overlay of topic.sheet keeps %q", gone)
		}
	}
	if !strings.Contains(public, "`public_url`") || !strings.Contains(public, "`topic.public-instance`") {
		t.Error("the public instance overlay of topic.sheet does not hand out the public link")
	}
}
