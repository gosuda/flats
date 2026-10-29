package agentsetup

import (
	"errors"
	"strings"
	"testing"
)

func block(eol string) string {
	return strings.Join(blockLines(), eol) + eol
}

func TestRoutingTextIsShortAndHostFree(t *testing.T) {
	if n := len(RoutingText); n > 800 {
		t.Errorf("routing text is %d bytes, want at most 800", n)
	}
	for _, bad := range []string{"http", "127.0.0.1", "://", "localhost"} {
		if strings.Contains(RoutingText, bad) {
			t.Errorf("routing text names a host: %q", bad)
		}
	}
	for _, want := range []string{"check_client", "guide", "topic.docs", "create_flat", "save_document", "tell me"} {
		if !strings.Contains(RoutingText, want) {
			t.Errorf("routing text lacks %q", want)
		}
	}
}

func TestSetBlock(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		action         Action
	}{
		{"empty file gets the block alone", "", block("\n"), Add},
		{"text ending in a newline", "# Notes\n", "# Notes\n\n" + block("\n"), Add},
		{"text without a final newline gains one", "# Notes", "# Notes\n\n" + block("\n"), Add},
		{"text already ending in a blank line still gets one", "a\n\n", "a\n\n\n" + block("\n"), Add},
		{"whitespace-only file", "\n", "\n\n" + block("\n"), Add},
		{"CRLF file uses CRLF", "a\r\nb\r\n", "a\r\nb\r\n\r\n" + block("\r\n"), Add},
		{"current block is unchanged", "a\n\n" + block("\n"), "a\n\n" + block("\n"), Unchanged},
		{"current block in a CRLF file is unchanged", "a\r\n\r\n" + block("\r\n"), "a\r\n\r\n" + block("\r\n"), Unchanged},
		{
			"stale block is replaced where it is",
			"top\n\n" + BeginMarker + "\nold text\n" + EndMarker + "\n\nbottom\n",
			"top\n\n" + block("\n") + "\nbottom\n",
			Replace,
		},
		{
			"stale block at the end without a final newline",
			"top\n" + BeginMarker + "\nold\n" + EndMarker,
			"top\n" + strings.TrimSuffix(block("\n"), "\n"),
			Replace,
		},
		{
			"markers inside a fenced code block are not a block",
			"```\n" + BeginMarker + "\n```\n",
			"```\n" + BeginMarker + "\n```\n\n" + block("\n"),
			Add,
		},
		{
			"markers inside a tilde fence are not a block",
			"~~~md\n" + BeginMarker + "\n" + EndMarker + "\n~~~\n",
			"~~~md\n" + BeginMarker + "\n" + EndMarker + "\n~~~\n\n" + block("\n"),
			Add,
		},
		{
			"a marker with other text on its line is not a marker",
			"see " + BeginMarker + " here\n",
			"see " + BeginMarker + " here\n\n" + block("\n"),
			Add,
		},
		{
			"an indented marker is not a marker",
			"    " + BeginMarker + "\n",
			"    " + BeginMarker + "\n\n" + block("\n"),
			Add,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, action, err := SetBlock([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want || action != tc.action {
				t.Errorf("SetBlock(%q) = %q, %v; want %q, %v", tc.in, got, action, tc.want, tc.action)
			}
			again, action2, err := SetBlock(got)
			if err != nil || action2 != Unchanged || string(again) != string(got) {
				t.Errorf("second SetBlock = %q, %v, %v; want unchanged", again, action2, err)
			}
		})
	}
}

func TestSetBlockRefusesMalformedBlocks(t *testing.T) {
	for name, in := range map[string]string{
		"begin without end":  BeginMarker + "\ntext\n",
		"end before begin":   EndMarker + "\n" + BeginMarker + "\n",
		"end without begin":  "text\n" + EndMarker + "\n",
		"two blocks":         block("\n") + block("\n"),
		"two begins one end": BeginMarker + "\n" + BeginMarker + "\n" + EndMarker + "\n",
		"one begin two ends": BeginMarker + "\n" + EndMarker + "\n" + EndMarker + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			for _, fn := range []func([]byte) ([]byte, Action, error){SetBlock, DropBlock} {
				out, _, err := fn([]byte(in))
				if !errors.Is(err, ErrBlock) || out != nil {
					t.Errorf("err = %v, out = %q; want ErrBlock and no output", err, out)
				}
			}
		})
	}
}

// A block appended after a fence that never closes would be code to a reader,
// and the next run would not see its markers, so it would append another.
func TestSetBlockRefusesAFileThatEndsInsideAFence(t *testing.T) {
	for name, in := range map[string]string{
		"a lone fence":                          "```\n",
		"a fence with an info string and text":  "# Notes\n\n```go\ncode\n",
		"a marker inside the fence":             "```\n" + BeginMarker + "\n",
		"a tilde fence without a final newline": "~~~\ntext",
		"a shorter fence does not close it":     "````\n```\n",
		"a fence after a closed one":            "```\nx\n```\n\n~~~\ny\n",
		"CRLF":                                  "```\r\ncode\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, action, err := SetBlock([]byte(in))
			if !errors.Is(err, ErrOpenFence) || out != nil || action != Unchanged {
				t.Errorf("SetBlock = %q, %v, %v; want ErrOpenFence and no output", out, action, err)
			}
			// There is nothing to undo, and undo says so instead of refusing.
			out, action, err = DropBlock([]byte(in))
			if err != nil || action != Unchanged || string(out) != in {
				t.Errorf("DropBlock = %q, %v, %v; want the file unchanged", out, action, err)
			}
		})
	}
}

// A fence left open after the block does not hide the block.
func TestABlockBeforeAnOpenFenceIsStillFound(t *testing.T) {
	tail := "\n```\ncode\n"
	current := block("\n") + tail
	if got, action, err := SetBlock([]byte(current)); err != nil || action != Unchanged || string(got) != current {
		t.Errorf("SetBlock(current) = %q, %v, %v; want unchanged", got, action, err)
	}
	stale := BeginMarker + "\nold\n" + EndMarker + "\n" + tail
	if got, action, err := SetBlock([]byte(stale)); err != nil || action != Replace || string(got) != current {
		t.Errorf("SetBlock(stale) = %q, %v, %v; want the block replaced", got, action, err)
	}
	if got, action, err := DropBlock([]byte(current)); err != nil || action != Remove || string(got) != tail {
		t.Errorf("DropBlock = %q, %v, %v", got, action, err)
	}
}

// Setup then undo returns the text, apart from the final newline that setup
// adds when it was missing.
func TestDropBlockInvertsSetBlock(t *testing.T) {
	for _, in := range []string{
		"", "# Notes\n", "# Notes\n\nmore\n", "a\n\n", "\n", "a\r\nb\r\n", "no newline at the end\n\n\n",
	} {
		set, _, err := SetBlock([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		got, action, err := DropBlock(set)
		if err != nil || action != Remove || string(got) != in {
			t.Errorf("DropBlock(SetBlock(%q)) = %q, %v, %v", in, got, action, err)
		}
		again, action, err := DropBlock(got)
		if err != nil || action != Unchanged || string(again) != in {
			t.Errorf("second DropBlock(%q) = %q, %v, %v; want unchanged", in, again, action, err)
		}
	}
	set, _, _ := SetBlock([]byte("no final newline"))
	got, _, _ := DropBlock(set)
	if string(got) != "no final newline\n" {
		t.Errorf("a missing final newline stays added: %q", got)
	}
}

func TestDropBlockKeepsBytesOutsideTheBlock(t *testing.T) {
	in := "intro\n\n" + block("\n") + "\nafter\n"
	got, _, err := DropBlock([]byte(in))
	if err != nil || string(got) != "intro\n\nafter\n" {
		t.Errorf("DropBlock = %q, %v", got, err)
	}
}

func TestOnlyFrontMatter(t *testing.T) {
	for in, want := range map[string]bool{
		"":                              true,
		"  \n\n":                        true,
		"---\nalwaysApply: true\n---\n": true,
		"---\r\nalwaysApply: true\r\n---\r\n\r\n": true,
		"---\nalwaysApply: true\n---\n\nnote\n":   false,
		"---\nunterminated\n":                     false,
		"\n---\na: b\n---\n":                      false,
		"just text\n":                             false,
		"text\n---\n":                             false,
		"text\n---\nmore\n---\n":                  false,
	} {
		if got := OnlyFrontMatter([]byte(in)); got != want {
			t.Errorf("OnlyFrontMatter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCursorRuleIsFrontMatterThenBlock(t *testing.T) {
	rule := CursorRule()
	if !strings.HasPrefix(string(rule), "---\n") || !strings.Contains(string(rule), "alwaysApply: true\n---\n\n"+BeginMarker) {
		t.Errorf("rule = %q", rule)
	}
	dropped, _, err := DropBlock(rule)
	if err != nil || string(dropped) != cursorFrontMatter || !OnlyFrontMatter(dropped) {
		t.Errorf("DropBlock(rule) = %q, %v", dropped, err)
	}
}
