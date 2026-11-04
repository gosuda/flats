package site

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOnlyTheExactNameAnswers(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"index.html":                       "<h1>home</h1>",
		"flats.json":                       `{"kind":"static"}`,
		"longfilename-for-short-alias.txt": "long",
		"sub/page.html":                    "<p>page</p>",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Static{Dir: dir, Entry: "index.html"}
	get := func(target string) int {
		req := httptest.NewRequest(http.MethodGet, "http://site.test/", nil)
		req.URL.Path = target
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w.Code
	}
	for _, target := range []string{"/index.html", "/longfilename-for-short-alias.txt", "/sub/page.html", "/"} {
		if code := get(target); code != http.StatusOK {
			t.Errorf("%s: %d, want 200", target, code)
		}
	}
	for _, target := range []string{
		"/FLATS.JSON", "/Flats.json", "/flats.json::$DATA", "/flats.json:$DATA", "/flats.json.", "/flats.json ", "/flats.json...",
		"/INDEX.HTML", "/index.html::$DATA", "/index.html:evil", "/index.html.", "/index.html ",
		"/LONGFI~1.TXT", "/longfi~1.txt",
		"/NUL", "/nul", "/con", "/CON", "/COM1", "/NUL.txt", "/sub/NUL", "/SUB/page.html", "/sub/PAGE.HTML",
	} {
		if code := get(target); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, code)
		}
	}
}

// allowedName refuses the form of a short name before a file opens. Should one
// pass it, exactName still refuses a real alias of the volume.
func TestExactNameRefusesARealShortAlias(t *testing.T) {
	dir := t.TempDir()
	long := filepath.Join(dir, "longfilename-for-short-alias.txt")
	if err := os.WriteFile(long, []byte("long"), 0o644); err != nil {
		t.Fatal(err)
	}
	wide, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(wide, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Base(windows.UTF16ToString(buf[:n]))
	if strings.EqualFold(alias, filepath.Base(long)) {
		t.Skip("this volume keeps no short names")
	}
	f, err := os.Open(filepath.Join(dir, alias))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if exactName(f, alias) {
		t.Errorf("the alias %s passed", alias)
	}
	if !exactName(f, filepath.Base(long)) {
		t.Error("the stored name did not pass")
	}
}
