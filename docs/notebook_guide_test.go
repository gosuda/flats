package runtimeref

import (
	"slices"
	"strings"
	"testing"
)

func TestNotebookTopicIsRoutedAndReferenced(t *testing.T) {
	md, ok := Topic("notebook")
	if !ok || !strings.HasPrefix(md, "## Notebooks\n") {
		t.Fatal("topic.notebook must exist and start with its heading")
	}
	index, _ := Topic("index")
	for _, want := range []string{"`topic.notebook`", "`get_notebook`", "`update_notebook`", "`create_flat {type: \"notebook\"}`"} {
		if !strings.Contains(index, want) {
			t.Errorf("topic.index does not route with %s", want)
		}
	}
	if !strings.Contains(ContentTypesMarkdown, md) || !strings.Contains(ContentTypesMarkdown, "| `notebook` |") {
		t.Error("the content-types reference must carry the notebook row and topic")
	}
	if strings.Contains(Markdown, "## Notebooks") {
		t.Error("the runtime reference describes server flats, not notebooks")
	}
	for _, name := range []string{"docs", "canvas", "board", "page"} {
		if other, _ := Topic(name); !strings.Contains(other, "`topic.notebook`") {
			t.Errorf("topic.%s's type chooser omits topic.notebook", name)
		}
	}
}

// The topic names every agent operation of the wire schema, and the refusal
// categories agents receive from notebook tools.
func TestNotebookTopicCoversOperationsAndRefusals(t *testing.T) {
	md, _ := Topic("notebook")
	for _, op := range []string{
		"create_page", "set_page_title", "move_page", "delete_page", "duplicate_page",
		"create_block", "set_block", "turn_blocks", "move_blocks", "delete_blocks", "duplicate_blocks",
		"create_database", "set_database_title", "create_property", "set_property", "delete_property",
		"set_property_order", "create_option", "set_option", "delete_option", "set_option_order",
		"create_row", "set_cell", "move_rows", "delete_rows", "create_view", "set_view", "delete_view",
		"set_view_order", "markdown_to_blocks",
	} {
		if !strings.Contains(md, "`"+op+"`") {
			t.Errorf("topic.notebook does not name operation %s", op)
		}
	}
	for _, c := range []string{"not_notebook", "notebook_capacity"} {
		if !slices.Contains(RefusalCategories(), c) || !strings.Contains(md, c) {
			t.Errorf("refusal.%s must exist and be named by topic.notebook", c)
		}
	}
	conflict, _ := Refusal("conflict")
	for _, reason := range []string{"read_expired", "result_expired", "get_notebook"} {
		if !strings.Contains(conflict, reason) {
			t.Errorf("refusal.conflict does not explain %s", reason)
		}
	}
	// Notebooks have no anonymous editing: only board and docs offer it.
	approvals, _ := Topic("approvals")
	if strings.Contains(md, "public_access") || strings.Contains(approvals, "notebook") {
		t.Error("notebooks must not inherit the board and docs anonymous-edit guidance")
	}
}

func TestNotebookInstructionsStaySmall(t *testing.T) {
	for _, limit := range []int64{20 << 20, 1 << 40} {
		for name, ins := range map[string]string{"off": Instructions(limit), "autopilot": AutopilotInstructions(limit)} {
			if n := len(ins); n > 2300 {
				t.Errorf("%s instructions are %d bytes at upload limit %d", name, n, limit)
			}
			if !strings.Contains(ins, "notebooks have no Draft, versions or publish") {
				t.Errorf("%s instructions omit the notebook exception", name)
			}
		}
	}
}

func TestNotebookTopicAutopilotOverlay(t *testing.T) {
	on, ok := AutopilotTopicOverlay("notebook")
	off, _ := Topic("notebook")
	if !ok || on != off {
		t.Fatal("topic.notebook needs no Autopilot replacement: it never claims the operator decides approvals")
	}
}
