package agentsetup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	entryDenied = `{"serverName": "claude.ai Claude Docs"}`
	entrySkill  = `"Skill(anthropic-skills:docs)"`
	entryTools  = `"mcp__claude_ai_Claude_Docs__*"`
)

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// isSubsequence reports whether in can be turned into out by inserting bytes
// only.
func isSubsequence(in, out []byte) bool {
	i := 0
	for _, c := range out {
		if i < len(in) && in[i] == c {
			i++
		}
	}
	return i == len(in)
}

func countOf(t *testing.T, b []byte, want string) int {
	t.Helper()
	return strings.Count(string(b), want)
}

var settingsCases = []struct {
	name, in, want string
}{
	{"missing or empty file", "", `{
  "deniedMcpServers": [
    ` + entryDenied + `
  ],
  "permissions": {
    "deny": [
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  }
}
`},
	{"empty object", "{}", `{
  "deniedMcpServers": [
    ` + entryDenied + `
  ],
  "permissions": {
    "deny": [
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  }
}`},
	{"empty object over two lines", "{\n}\n", `{
  "deniedMcpServers": [
    ` + entryDenied + `
  ],
  "permissions": {
    "deny": [
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  }
}
`},
	{"two-space file", "{\n  \"a\": 1\n}\n", `{
  "a": 1,
  "deniedMcpServers": [
    ` + entryDenied + `
  ],
  "permissions": {
    "deny": [
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  }
}
`},
	{"four-space file", "{\n    \"a\": 1\n}\n", `{
    "a": 1,
    "deniedMcpServers": [
        ` + entryDenied + `
    ],
    "permissions": {
        "deny": [
            ` + entrySkill + `,
            ` + entryTools + `
        ]
    }
}
`},
	{"tab file", "{\n\t\"a\": 1\n}\n", "{\n\t\"a\": 1,\n\t\"deniedMcpServers\": [\n\t\t" + entryDenied + "\n\t],\n\t\"permissions\": {\n\t\t\"deny\": [\n\t\t\t" + entrySkill + ",\n\t\t\t" + entryTools + "\n\t\t]\n\t}\n}\n"},
	{"CRLF file", crlf("{\n  \"a\": 1\n}\n"), crlf(`{
  "a": 1,
  "deniedMcpServers": [
    ` + entryDenied + `
  ],
  "permissions": {
    "deny": [
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  }
}
`)},
	{"minified file", `{"a":1}`, `{"a":1, "deniedMcpServers": [` + entryDenied + `], "permissions": {"deny": [` + entrySkill + `, ` + entryTools + `]}}`},
	{"permissions without deny", "{\n  \"permissions\": {\n    \"allow\": [\"x\"]\n  }\n}\n", `{
  "permissions": {
    "allow": ["x"],
    "deny": [
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  },
  "deniedMcpServers": [
    ` + entryDenied + `
  ]
}
`},
	{"permissions as an empty object", "{\n  \"permissions\": {}\n}\n", `{
  "permissions": {
    "deny": [
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  },
  "deniedMcpServers": [
    ` + entryDenied + `
  ]
}
`},
	{"multi-line deny array", "{\n  \"permissions\": {\n    \"deny\": [\n      \"Bash(rm:*)\"\n    ]\n  }\n}\n", `{
  "permissions": {
    "deny": [
      "Bash(rm:*)",
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  },
  "deniedMcpServers": [
    ` + entryDenied + `
  ]
}
`},
	{"inline deny array", "{\n  \"permissions\": {\"deny\": [\"Bash(rm:*)\"]}\n}\n", `{
  "permissions": {"deny": ["Bash(rm:*)", ` + entrySkill + `, ` + entryTools + `]},
  "deniedMcpServers": [
    ` + entryDenied + `
  ]
}
`},
	{"empty inline arrays", "{\n  \"permissions\": {\"deny\": []}, \"deniedMcpServers\": []\n}\n", `{
  "permissions": {"deny": [` + entrySkill + `, ` + entryTools + `]}, "deniedMcpServers": [` + entryDenied + `]
}
`},
	{"empty multi-line arrays", "{\n  \"permissions\": {\n    \"deny\": [\n    ]\n  },\n  \"deniedMcpServers\": [\n  ]\n}\n", `{
  "permissions": {
    "deny": [
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  },
  "deniedMcpServers": [
    ` + entryDenied + `
  ]
}
`},
	{"another server already denied", "{\n  \"deniedMcpServers\": [\n    {\"serverName\": \"other\"}\n  ]\n}\n", `{
  "deniedMcpServers": [
    {"serverName": "other"},
    ` + entryDenied + `
  ],
  "permissions": {
    "deny": [
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  }
}
`},
	{"an element with more keys is a different rule", "{\n  \"deniedMcpServers\": [\n    {\"serverName\": \"claude.ai Claude Docs\", \"serverUrl\": \"x\"}\n  ]\n}\n", `{
  "deniedMcpServers": [
    {"serverName": "claude.ai Claude Docs", "serverUrl": "x"},
    ` + entryDenied + `
  ],
  "permissions": {
    "deny": [
      ` + entrySkill + `,
      ` + entryTools + `
    ]
  }
}
`},
}

func TestSetSettings(t *testing.T) {
	for _, tc := range settingsCases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			ch, err := SetSettings(in)
			if err != nil {
				t.Fatal(err)
			}
			if ch.Action != Add || string(ch.Out) != tc.want {
				t.Fatalf("SetSettings(%q)\n got %v:\n%s\nwant:\n%s", tc.in, ch.Action, ch.Out, tc.want)
			}
			if len(ch.Entries) != 3 || len(ch.Present) != 0 {
				t.Errorf("entries = %v, present = %v", ch.Entries, ch.Present)
			}
			if !isSubsequence(in, ch.Out) {
				t.Errorf("the result is not the input plus inserted bytes:\n%s", ch.Out)
			}
			if !json.Valid(ch.Out) {
				t.Errorf("not valid JSON:\n%s", ch.Out)
			}
			if bytes.Contains(in, []byte("\r\n")) != bytes.Contains(ch.Out, []byte("\r\n")) || (bytes.Contains(in, []byte("\r\n")) && strings.Count(string(ch.Out), "\n") != strings.Count(string(ch.Out), "\r\n")) {
				t.Errorf("line endings are mixed: %q", ch.Out)
			}
			for _, e := range []string{entryDenied, entrySkill, entryTools} {
				if n := countOf(t, ch.Out, e); n != 1 {
					t.Errorf("%s appears %d times, want 1", e, n)
				}
			}
			again, err := SetSettings(ch.Out)
			if err != nil || again.Action != Unchanged || !bytes.Equal(again.Out, ch.Out) || len(again.Present) != 3 {
				t.Errorf("second SetSettings = %v, %v, %v; want unchanged with three present", again.Action, again.Present, err)
			}
		})
	}
}

func TestDropSettings(t *testing.T) {
	for _, tc := range settingsCases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := SetSettings([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			ch, err := DropSettings(set.Out)
			if err != nil {
				t.Fatal(err)
			}
			if ch.Action != Remove || len(ch.Entries) != 3 {
				t.Fatalf("DropSettings = %v %v", ch.Action, ch.Entries)
			}
			for _, e := range []string{entryDenied, entrySkill, entryTools} {
				if n := countOf(t, ch.Out, e); n != 0 {
					t.Errorf("%s still appears %d times:\n%s", e, n, ch.Out)
				}
			}
			if !json.Valid(ch.Out) {
				t.Errorf("not valid JSON:\n%s", ch.Out)
			}
			// Undo keeps emptied arrays, so setting again lands where the first
			// setup did, as the same value.
			again, err := SetSettings(ch.Out)
			if err != nil || again.Action != Add {
				t.Fatalf("setup after undo = %v, %v", again.Action, err)
			}
			first, _ := decodeObject(set.Out)
			second, _ := decodeObject(again.Out)
			if fmt.Sprint(first) != fmt.Sprint(second) {
				t.Errorf("setup, undo and setup differ in value:\n%s\n%s", set.Out, again.Out)
			}
			second2, err := DropSettings(again.Out)
			if err != nil || !bytes.Equal(second2.Out, ch.Out) {
				t.Errorf("undo is not stable:\n%s\n%s", ch.Out, second2.Out)
			}
			nothing, err := DropSettings(ch.Out)
			if err != nil || nothing.Action != Unchanged || !bytes.Equal(nothing.Out, ch.Out) {
				t.Errorf("a second undo changed the file: %v %v", nothing.Action, err)
			}
		})
	}
}

// When every container existed with elements, setup then undo restores the
// exact bytes.
func TestDropSettingsRestoresExistingContainers(t *testing.T) {
	for _, in := range []string{
		"{\n  \"permissions\": {\n    \"deny\": [\n      \"Bash(rm:*)\"\n    ]\n  },\n  \"deniedMcpServers\": [\n    {\"serverName\": \"other\"}\n  ]\n}\n",
		"{\"permissions\": {\"deny\": [\"a\", \"b\"]}, \"deniedMcpServers\": [{\"serverName\": \"x\"}]}",
		crlf("{\n\t\"deniedMcpServers\": [\n\t\t{\"serverName\": \"x\"}\n\t],\n\t\"permissions\": {\n\t\t\"deny\": [\n\t\t\t\"a\"\n\t\t]\n\t}\n}\n"),
	} {
		set, err := SetSettings([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		got, err := DropSettings(set.Out)
		if err != nil || string(got.Out) != in {
			t.Errorf("round trip of %q\n got %q (%v)", in, got.Out, err)
		}
	}
}

func TestSetSettingsKeepsPresentEntries(t *testing.T) {
	all := "{\n  \"deniedMcpServers\": [\n    " + entryDenied + "\n  ],\n  \"permissions\": {\n    \"deny\": [\n      " + entrySkill + ",\n      " + entryTools + "\n    ]\n  }\n}\n"
	ch, err := SetSettings([]byte(all))
	if err != nil || ch.Action != Unchanged || string(ch.Out) != all || len(ch.Present) != 3 || len(ch.Entries) != 0 {
		t.Errorf("all present: %v %v %v", ch.Action, ch.Present, err)
	}

	partial := "{\n  \"permissions\": {\n    \"deny\": [\n      " + entrySkill + "\n    ]\n  }\n}\n"
	ch, err = SetSettings([]byte(partial))
	if err != nil || ch.Action != Add || len(ch.Present) != 1 || len(ch.Entries) != 2 {
		t.Fatalf("partial: %v %v %v", ch.Action, ch.Present, err)
	}
	if n := countOf(t, ch.Out, entrySkill); n != 1 {
		t.Errorf("a present entry was added again:\n%s", ch.Out)
	}
}

func TestDropSettingsRemovesEveryMatchAndKeepsOthers(t *testing.T) {
	in := `{
  "deniedMcpServers": [
    {"serverName": "claude.ai Claude Docs"},
    {"serverName": "other"},
    {"serverName": "claude.ai Claude Docs", "serverUrl": "x"},
    {"serverName": "claude.ai Claude Docs"}
  ],
  "permissions": {
    "deny": ["Skill(anthropic-skills:docs)", "Bash(rm:*)", "Skill(anthropic-skills:docs)", "mcp__claude_ai_Claude_Docs__*"]
  },
  "note": "mcp__claude_ai_Claude_Docs__*"
}
`
	ch, err := DropSettings([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "deniedMcpServers": [
    {"serverName": "other"},
    {"serverName": "claude.ai Claude Docs", "serverUrl": "x"}
  ],
  "permissions": {
    "deny": ["Bash(rm:*)"]
  },
  "note": "mcp__claude_ai_Claude_Docs__*"
}
`
	if string(ch.Out) != want || len(ch.Entries) != 5 {
		t.Errorf("got %d entries:\n%s\nwant:\n%s", len(ch.Entries), ch.Out, want)
	}
}

func TestDropSettingsLineAndCommaHandling(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"last element on its own line": {
			"{\n  \"permissions\": {\n    \"deny\": [\n      \"a\",\n      " + entrySkill + "\n    ]\n  }\n}\n",
			"{\n  \"permissions\": {\n    \"deny\": [\n      \"a\"\n    ]\n  }\n}\n",
		},
		"first element on its own line": {
			"{\n  \"permissions\": {\n    \"deny\": [\n      " + entrySkill + ",\n      \"a\"\n    ]\n  }\n}\n",
			"{\n  \"permissions\": {\n    \"deny\": [\n      \"a\"\n    ]\n  }\n}\n",
		},
		"sole element on its own line leaves the brackets": {
			"{\n  \"permissions\": {\n    \"deny\": [\n      " + entrySkill + "\n    ]\n  }\n}\n",
			"{\n  \"permissions\": {\n    \"deny\": [\n    ]\n  }\n}\n",
		},
		"sole inline element": {
			`{"permissions": {"deny": [` + entrySkill + `]}}`,
			`{"permissions": {"deny": []}}`,
		},
		"last inline element": {
			`{"permissions": {"deny": ["a", ` + entrySkill + `]}}`,
			`{"permissions": {"deny": ["a"]}}`,
		},
		"middle inline element": {
			`{"permissions": {"deny": ["a", ` + entrySkill + `, "b"]}}`,
			`{"permissions": {"deny": ["a", "b"]}}`,
		},
		"CRLF lines": {
			crlf("{\n  \"permissions\": {\n    \"deny\": [\n      \"a\",\n      " + entrySkill + "\n    ]\n  }\n}\n"),
			crlf("{\n  \"permissions\": {\n    \"deny\": [\n      \"a\"\n    ]\n  }\n}\n"),
		},
		"an object element over several lines": {
			"{\n  \"deniedMcpServers\": [\n    {\n      \"serverName\": \"claude.ai Claude Docs\"\n    },\n    {\"serverName\": \"x\"}\n  ]\n}\n",
			"{\n  \"deniedMcpServers\": [\n    {\"serverName\": \"x\"}\n  ]\n}\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ch, err := DropSettings([]byte(tc.in))
			if err != nil || string(ch.Out) != tc.want {
				t.Errorf("got %q (%v)\nwant %q", ch.Out, err, tc.want)
			}
		})
	}
}

func TestSettingsRefusals(t *testing.T) {
	for name, in := range map[string]string{
		"invalid JSON":                  `{"a": }`,
		"comments":                      "{\n  // comment\n  \"a\": 1\n}\n",
		"trailing comma":                `{"a": 1,}`,
		"not an object":                 `[1]`,
		"a string":                      `"x"`,
		"BOM":                           "\ufeff{}",
		"deniedMcpServers not an array": `{"deniedMcpServers": {}}`,
		"deniedMcpServers null":         `{"deniedMcpServers": null}`,
		"permissions not an object":     `{"permissions": []}`,
		"permissions null":              `{"permissions": null}`,
		"deny not an array":             `{"permissions": {"deny": "x"}}`,
		"deniedMcpServers twice":        `{"deniedMcpServers": [], "deniedMcpServers": []}`,
		"permissions twice":             `{"permissions": {}, "permissions": {}}`,
		"deny twice":                    `{"permissions": {"deny": [], "deny": []}}`,
	} {
		t.Run(name, func(t *testing.T) {
			for label, fn := range map[string]func([]byte) (SettingsChange, error){"set": SetSettings, "drop": DropSettings} {
				ch, err := fn([]byte(in))
				if !errors.Is(err, ErrSettings) || ch.Out != nil {
					t.Errorf("%s: err = %v, out = %q; want ErrSettings and no output", label, err, ch.Out)
				}
			}
		})
	}
	if ch, err := DropSettings([]byte("  \n")); err != nil || ch.Action != Unchanged {
		t.Errorf("DropSettings of whitespace = %v, %v", ch.Action, err)
	}
}

func TestSettingsKeepsValuesOutsideTheEntries(t *testing.T) {
	in := `{
  "env": {"EMOJI": "\u00e9\ud83d\ude00", "BIG": 12345678901234567890123, "F": 1.50, "E": 1e400},
  "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "echo \"quoted\" \\ back"}]}]},
  "unusedFlag": true,
  "nothing": null
}
`
	ch, err := SetSettings([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if !isSubsequence([]byte(in), ch.Out) {
		t.Errorf("not insertion-only:\n%s", ch.Out)
	}
	for _, keep := range []string{`"BIG": 12345678901234567890123`, `"F": 1.50`, `"E": 1e400`, `"\u00e9\ud83d\ude00"`, `\"quoted\" \\ back`} {
		if !strings.Contains(string(ch.Out), keep) {
			t.Errorf("lost %s", keep)
		}
	}
	back, err := DropSettings(ch.Out)
	if err != nil || !isSubsequence([]byte(in), back.Out) {
		t.Errorf("undo lost original bytes: %v\n%s", err, back.Out)
	}
}

// checkSettings is the safety net behind every edit: it must refuse an
// edited file that is not the original plus (or minus) exactly the entries.
func TestCheckSettingsRefusesWrongResults(t *testing.T) {
	before := []byte("{\n  \"a\": 1\n}\n")
	good, err := SetSettings(before)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSettings(before, good.Out, setEntries); err != nil {
		t.Fatalf("a correct result is refused: %v", err)
	}
	for name, after := range map[string]string{
		"an entry is missing":     `{"a": 1, "deniedMcpServers": [{"serverName": "claude.ai Claude Docs"}], "permissions": {"deny": ["Skill(anthropic-skills:docs)"]}}`,
		"another value changed":   `{"a": 2, "deniedMcpServers": [{"serverName": "claude.ai Claude Docs"}], "permissions": {"deny": ["Skill(anthropic-skills:docs)", "mcp__claude_ai_Claude_Docs__*"]}}`,
		"a key was dropped":       `{"deniedMcpServers": [{"serverName": "claude.ai Claude Docs"}], "permissions": {"deny": ["Skill(anthropic-skills:docs)", "mcp__claude_ai_Claude_Docs__*"]}}`,
		"an extra key appeared":   `{"a": 1, "b": 2, "deniedMcpServers": [{"serverName": "claude.ai Claude Docs"}], "permissions": {"deny": ["Skill(anthropic-skills:docs)", "mcp__claude_ai_Claude_Docs__*"]}}`,
		"the result is not JSON":  `{"a": 1,`,
		"the entry is duplicated": `{"a": 1, "deniedMcpServers": [{"serverName": "claude.ai Claude Docs"}, {"serverName": "claude.ai Claude Docs"}], "permissions": {"deny": ["Skill(anthropic-skills:docs)", "mcp__claude_ai_Claude_Docs__*"]}}`,
	} {
		if err := checkSettings(before, []byte(after), setEntries); !errors.Is(err, ErrSettings) {
			t.Errorf("%s: err = %v, want ErrSettings", name, err)
		}
	}
	dropped, err := DropSettings(good.Out)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSettings(good.Out, dropped.Out, dropEntries); err != nil {
		t.Errorf("a correct removal is refused: %v", err)
	}
	// Undo keeps emptied arrays, so a result that also dropped them is wrong.
	if err := checkSettings(good.Out, before, dropEntries); !errors.Is(err, ErrSettings) {
		t.Errorf("a removal that dropped the emptied arrays: err = %v, want ErrSettings", err)
	}
	if err := checkSettings(good.Out, good.Out, dropEntries); !errors.Is(err, ErrSettings) {
		t.Errorf("a removal that removed nothing: err = %v, want ErrSettings", err)
	}
}
