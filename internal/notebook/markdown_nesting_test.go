package notebook

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMarkdownNestingFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "empty quotes", source: strings.Repeat(">", 128)},
		{name: "empty quote line", source: strings.Repeat(">", 128) + "\n"},
		{name: "empty lists", source: strings.Repeat("- ", 128)},
		{name: "tabbed quotes", source: strings.Repeat(">\t", 128) + "text", want: "text"},
		{name: "ordered lists", source: strings.Repeat("1. ", 128) + "text", want: "text"},
		{name: "mixed containers", source: strings.Repeat("> - ", 128) + "text\r\n", want: "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops, warnings, err := ParseMarkdown(tc.source, Placement{}, nil)
			if err != nil {
				t.Fatalf("warnings=%v err=%v", warnings, err)
			}
			var visible strings.Builder
			for _, op := range ops {
				visible.WriteString(markdownPlain(op.(CreateBlockOp).Content.Text))
			}
			if visible.String() != tc.want {
				t.Fatalf("visible text=%q, want %q", visible.String(), tc.want)
			}
		})
	}
}

func TestMarkdownDeepContainersBounded(t *testing.T) {
	var lists strings.Builder
	for depth := range 1024 {
		fmt.Fprintf(&lists, "%s- item %d\n", strings.Repeat("  ", depth), depth)
	}
	for _, tc := range []struct {
		name   string
		source string
		texts  int
		last   string
	}{
		{name: "quotes", source: strings.Repeat(">", 1<<20) + " x\n", texts: 1, last: "x"},
		{name: "inline lists", source: strings.Repeat("- ", 1<<19) + "x\n", texts: 1, last: "x"},
		{name: "indented lists", source: lists.String(), texts: 1024, last: "item 1023"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			ops, warnings, err := ParseMarkdown(tc.source, Placement{}, nil)
			elapsed := time.Since(start)
			t.Logf("bytes=%d elapsed=%s operations=%d warnings=%d", len(tc.source), elapsed, len(ops), len(warnings))
			limit := 2 * time.Second
			if raceEnabled {
				// Instrumented parser byte scans have a separate CPU budget.
				limit = 20 * time.Second
			}
			if elapsed > limit {
				t.Errorf("deep container conversion took %s, limit %s", elapsed, limit)
			}
			if err != nil {
				t.Fatal(err)
			}
			limited := false
			for _, warning := range warnings {
				limited = limited || warning.Code == "nesting_limited"
			}
			if !limited {
				t.Fatal("missing parser nesting warning")
			}
			depths := map[string]int{}
			texts, last := 0, ""
			for _, op := range ops {
				block := op.(CreateBlockOp)
				depth := 1
				if block.Place.Parent != nil {
					depth += depths[block.Place.Parent.Tmp]
				}
				if depth > MaxBlockDepth {
					t.Fatalf("block depth = %d", depth)
				}
				depths[block.Ref] = depth
				if text := markdownPlain(block.Content.Text); text != "" {
					texts++
					last = text
				}
			}
			if texts != tc.texts || last != tc.last {
				t.Fatalf("visible texts=%d last=%q, want %d %q", texts, last, tc.texts, tc.last)
			}
		})
	}
}
