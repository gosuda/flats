package doccheck

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// writeTree writes files under a temporary root and returns the root and
// the sorted file list Check expects.
func writeTree(t *testing.T, files map[string]string) (string, []string) {
	t.Helper()
	root := t.TempDir()
	var list []string
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		list = append(list, name)
	}
	sort.Strings(list)
	return root, list
}

func check(t *testing.T, files map[string]string) *Report {
	t.Helper()
	root, list := writeTree(t, files)
	r, err := Check(root, list)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func errorStrings(r *Report) []string {
	var out []string
	for _, f := range r.Errors {
		out = append(out, f.String())
	}
	return out
}

func TestLinksAndAnchors(t *testing.T) {
	r := check(t, map[string]string{
		"README.md": `# Flats

See [docs](docs/README.md), [the guide](docs/guide.md#set-up-flats) and
[a split
link](docs/guide.md#use-flats-serve).
[Reference][ref] and [self](#flats).

[ref]: docs/guide.md#api--cli
[unused]: docs/missing-def.md

` + "```" + `
[ignored](docs/inside-fence.md)
` + "```" + `

Inline ` + "`[ignored](docs/inside-code.md)`" + ` code.
<!-- [ignored](docs/inside-comment.md) -->
`,
		"docs/README.md": `# Docs

- [missing](nope.md)
- [bad anchor](guide.md#no-such-heading)
- [wrong case](Guide.md)
- [outside](../../etc/passwd)
- [dir](../examples)
- [image](../assets/logo.png)
- <a href="guide.md#custom-id">html</a>
- <img src="missing.png">
`,
		"docs/guide.md": `# Set up Flats

## Use ` + "`flats serve`" + `

## API & CLI

<a id="custom-id"></a>
`,
		"examples/demo/main.js": "",
		"assets/logo.png":       "",
	})
	want := []string{
		`README.md:9: broken link "docs/missing-def.md" (file not found)`,
		`docs/README.md:3: broken link "nope.md" (file not found)`,
		`docs/README.md:4: broken anchor "guide.md#no-such-heading" (#no-such-heading is not a heading or id in docs/guide.md)`,
		`docs/README.md:5: broken link "Guide.md" (case mismatch: the repository has docs/guide.md, which breaks on Linux and GitHub)`,
		`docs/README.md:6: link "../../etc/passwd" points outside the repository`,
		`docs/README.md:10: broken link "missing.png" (file not found)`,
	}
	if got := errorStrings(r); !slices.Equal(got, want) {
		t.Errorf("errors:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestAgentTopicsRejectRelativeLinks(t *testing.T) {
	r := check(t, map[string]string{
		"README.md":           "[guide](docs/agent/topic.md)\n",
		"docs/agent/topic.md": "# Topic\n\n[other](other.md) [self](#topic) [web](https://example.com)\n",
		"docs/agent/other.md": "# Other\n",
	})
	want := []string{`docs/agent/topic.md:3: relative link "other.md" in docs/agent/ breaks when served through MCP or llms.txt`}
	if got := errorStrings(r); !slices.Equal(got, want) {
		t.Errorf("errors = %q, want %q", got, want)
	}
}

func TestReachability(t *testing.T) {
	r := check(t, map[string]string{
		"README.md":                    "[docs](docs) [example](examples/demo/main.js)\n",
		"examples/demo/main.js":        "",
		"examples/demo/README.md":      "# Demo\n",
		"docs/README.md":               "[a](a.md)\n",
		"docs/a.md":                    "# A\n",
		"docs/orphan.md":               "# Orphan\n",
		"docs/embedded/topic.md":       "# Embedded\n",
		"docs/embedded/_hidden.md":     "# Hidden\n",
		"docs/embed.go":                "package docs\n\n//go:embed embedded\nvar fs embed.FS\n",
		"docs/named.md":                "# Named\n",
		"docs/loader.go":               "package docs\n\nconst p = \"named.md\"\n",
		"docs/tested.md":               "# Tested\n",
		"docs/loader_test.go":          "package docs\n\nconst q = \"tested.md\"\n",
		"internal/pkg/README.md":       "# Package notes\n",
		".cursor/skills/x/SKILL.md":    "[broken](nope.md)\n",
		"docs/embedded/sub/_skip/x.md": "# Skipped\n",
	})
	if len(r.Errors) != 0 {
		t.Errorf("errors = %v", r.Errors)
	}
	if want := []string{"docs/embedded/_hidden.md", "docs/embedded/sub/_skip/x.md", "docs/orphan.md"}; !slices.Equal(r.Orphans, want) {
		t.Errorf("orphans = %q, want %q", r.Orphans, want)
	}
	want := []Colocated{{Path: "docs/tested.md", TestOnly: true}, {Path: "internal/pkg/README.md"}}
	if !slices.Equal(r.Colocated, want) {
		t.Errorf("colocated = %v, want %v", r.Colocated, want)
	}
}

func TestHashLiterals(t *testing.T) {
	r := check(t, map[string]string{
		"README.md": "Commit `0123456789abcdef0123456789abcdef01234567`.\n\n```\n" +
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n```\n\nShort 0123456 and 41 chars 0123456789abcdef0123456789abcdef012345678.\n",
	})
	if len(r.Hashes) != 1 || r.Hashes[0].Line != 1 {
		t.Errorf("hashes = %v, want only line 1", r.Hashes)
	}
}

func TestGitHubSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Set up Flats":          "set-up-flats",
		"API & CLI":             "api--cli",
		"Use `flats serve`":     "use-flats-serve",
		"[Linked](x.md) title":  "linked-title",
		"**Bold** _it_":         "bold-it",
		"한국어 제목":                "한국어-제목",
		"snake_case v1.2":       "snake_case-v12",
		"Trailing #":            "trailing",
		"<span>Tagged</span> x": "tagged-x",
	} {
		r := check(t, map[string]string{"README.md": "# " + in + "\n\n[x](#" + want + ")\n", "x.md": ""})
		if len(r.Errors) != 0 {
			t.Errorf("heading %q: %v", in, r.Errors)
		}
	}
}

func TestDuplicateHeadingSlugs(t *testing.T) {
	r := check(t, map[string]string{"README.md": "# Setup\n\n## Setup\n\nSetup\n-----\n\n[a](#setup) [b](#setup-1) [c](#setup-2)\n"})
	if len(r.Errors) != 0 {
		t.Errorf("errors = %v", r.Errors)
	}
}

func TestEmbedPatterns(t *testing.T) {
	got := embedPatterns("a.md \"b c.md\" `d.md`\tall:e")
	if want := []string{"a.md", "b c.md", "d.md", "all:e"}; !slices.Equal(got, want) {
		t.Errorf("embedPatterns = %q, want %q", got, want)
	}
}

// Regression cases from the first independent review.
func TestReviewRegressions(t *testing.T) {
	t.Run("destinations are decoded like a renderer", func(t *testing.T) {
		r := check(t, map[string]string{
			"README.md": "[amp](a&amp;b.md) [paren](a\\(b\\).md) [ref][r] [pct](a%26b.md)\n\n[r]: a&#38;b.md\n",
			"a&b.md":    "",
			"a(b).md":   "",
		})
		if len(r.Errors) != 0 {
			t.Errorf("errors = %v", r.Errors)
		}
	})
	t.Run("raw HTML headings get anchors and share numbering", func(t *testing.T) {
		r := check(t, map[string]string{"README.md": "<h1 align=\"center\">Flats <em>docs</em></h1>\n\n# Flats docs\n\n[a](#flats-docs) [b](#flats-docs-1)\n"})
		if len(r.Errors) != 0 {
			t.Errorf("errors = %v", r.Errors)
		}
	})
	t.Run("explicit anchors do not shift heading numbers", func(t *testing.T) {
		r := check(t, map[string]string{"README.md": "<a name=\"setup\"></a>\n\n# Setup\n\n## Setup\n\n[a](#setup) [b](#setup-1) [c](#setup-2)\n"})
		want := []string{`README.md:7: broken anchor "#setup-2" (#setup-2 is not a heading or id in README.md)`}
		if got := errorStrings(r); !slices.Equal(got, want) {
			t.Errorf("errors = %q, want %q", got, want)
		}
	})
	t.Run("agent topics may not link to their own file by name", func(t *testing.T) {
		r := check(t, map[string]string{
			"README.md":                    "[t](docs/agent/topics/example.md)\n",
			"docs/agent/topics/example.md": "# Example\n\n[x](example.md#example) [y](#example)\n",
		})
		if len(r.Errors) != 1 || !strings.Contains(r.Errors[0].Message, `relative link "example.md#example"`) {
			t.Errorf("errors = %v", r.Errors)
		}
	})
	t.Run("tab-separated embed patterns", func(t *testing.T) {
		got := embedPatterns("a/*.md\tb/*.md \"c\\x2e.md\"")
		if want := []string{"a/*.md", "b/*.md", "c..md"}; !slices.Equal(got, want) {
			t.Errorf("embedPatterns = %q, want %q", got, want)
		}
	})
	t.Run("entity spaces count in slugs", func(t *testing.T) {
		r := check(t, map[string]string{"README.md": "# Setup&#32;\n\n[x](#setup-)\n"})
		if len(r.Errors) != 0 {
			t.Errorf("errors = %v", r.Errors)
		}
	})
	t.Run("HTML block links report their own line", func(t *testing.T) {
		r := check(t, map[string]string{"README.md": "<div>\n<a href=\"missing.md\">x</a>\n</div>\n"})
		want := []string{`README.md:2: broken link "missing.md" (file not found)`}
		if got := errorStrings(r); !slices.Equal(got, want) {
			t.Errorf("errors = %q, want %q", got, want)
		}
	})
}

// Regression cases from the second independent review.
func TestReviewRound2Regressions(t *testing.T) {
	cases := map[string]map[string]string{
		"references are decoded once": {
			"README.md":  "[x](a&#38;amp;b.md)\n",
			"a&amp;b.md": "",
		},
		"inline HTML headings count": {
			"README.md": "text <h2>Title</h2>\n\n# Title\n\n[x](#title) [y](#title-1)\n",
		},
		"entities need a semicolon": {
			"README.md": "# &copy\n\n[x](#copy)\n",
		},
		"code span line endings render as spaces": {
			"README.md": "`foo\nbar`\n---\n\n[x](#foo-bar)\n",
		},
		"empty slugs are numbered": {
			"README.md": "# !!!\n\n# !!!\n\n[x](#-1)\n",
		},
		"code spans in inline HTML headings stay literal": {
			"README.md": "text <h2>`a&amp;b`</h2>\n\n[x](#aampb)\n",
		},
		"empty destinations are this page": {
			"README.md":           "[reload]() [t](docs/agent/topic.md)\n",
			"docs/agent/topic.md": "# Topic\n\n[reload]()\n",
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			if r := check(t, files); len(r.Errors) != 0 {
				t.Errorf("errors = %v", r.Errors)
			}
		})
	}
}
