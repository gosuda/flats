package bundle

import (
	"strings"
	"testing"
)

func TestWindowsRefusesNamesItCannotStore(t *testing.T) {
	for _, name := range []string{
		"NUL", "nul.txt", "assets/COM1", "CON.html", "a:b", "index.html::$DATA", "a<b.txt", "a|b.txt",
		"dot.", "space ", "LONGFI~1.TXT",
	} {
		c := newCollector(Limits{MaxBytes: 1 << 20})
		if err := c.addFile(name, strings.NewReader("x"), 0o644); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if len(c.verr.Problems) == 0 {
			t.Errorf("%q was accepted", name)
		}
	}
}

func TestWindowsRefusesACaseCollision(t *testing.T) {
	c := newCollector(Limits{MaxBytes: 1 << 20})
	for _, name := range []string{"Assets/Logo.png", "assets/logo.PNG"} {
		if err := c.addFile(name, strings.NewReader("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.verr.Problems) != 1 || !strings.Contains(c.verr.Problems[0].Message, "case") {
		t.Fatalf("problems: %+v", c.verr.Problems)
	}
}

func TestWindowsKeepsOrdinaryNames(t *testing.T) {
	c := newCollector(Limits{MaxBytes: 1 << 20})
	for _, name := range []string{"index.html", "assets/app.js", "nullable.txt", "com.json", ".well-known/x", "한글.md"} {
		if err := c.addFile(name, strings.NewReader("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.verr.Problems) != 0 {
		t.Fatalf("problems: %+v", c.verr.Problems)
	}
}

func TestWindowsReservesTheDocsPathInAnyCase(t *testing.T) {
	for _, p := range []string{"_docs", "_docs/x", "_DOCS", "_Docs/x.md"} {
		if !reservedDocsPath(p) {
			t.Errorf("%q is not reserved", p)
		}
	}
	if reservedDocsPath("docs/x.md") {
		t.Error("docs/x.md is reserved")
	}
}
