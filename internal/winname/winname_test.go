package winname

import "testing"

func TestCheckSegment(t *testing.T) {
	for _, bad := range []string{
		"", "NUL", "nul", "Nul.txt", "COM1", "lpt9.log", "CON", "aux.html", "prn", "CONIN$", "com¹",
		"a:b", "index.html::$DATA", "a<b", "a>b", `a"b`, "a|b", "a?b", "a*b", `a\b`, "a\x01b",
		"dot.", "space ", "...", "LONGFI~1.TXT", "PROGRA~1", "a~12.b",
	} {
		if err := CheckSegment(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	for _, good := range []string{
		"index.html", "a.b.c", ".hidden", "notes", "console.js", "nullable.txt", "com.json", "lpt.txt", "com10", "a~b.txt", "~1.txt",
		"longer~name.txt", "file~.txt", "한글.md",
	} {
		if err := CheckSegment(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

func TestCheckPathChecksEverySegment(t *testing.T) {
	if err := CheckPath("assets/NUL/x.txt"); err == nil {
		t.Fatal("a device name in a directory passed")
	}
	if err := CheckPath("assets/img/x.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestFoldKeyFindsCaseCollisions(t *testing.T) {
	if FoldKey("Assets/A.TXT") != FoldKey("assets/a.txt") {
		t.Fatal("no collision")
	}
	if FoldKey("a.txt") == FoldKey("b.txt") {
		t.Fatal("false collision")
	}
}
