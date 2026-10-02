package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// Agent-controlled text must not carry terminal escape sequences, carriage
// returns or bidi overrides to the operator's terminal.
func TestServerTextCannotControlTerminal(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("GET /api/approvals", 200, `{"approvals": [{"id": "apr-9", "flat": "blog", "action": "set_visibility",
	  "params": {"visibility": "public-listed"}, "status": "pending", "via": "mcp",
	  "reason": "x\u001b[2K\u001b[1Gapr-9 blog set_visibility private‮\u009b"}]}`)
	api.handle("GET /api/flats/blog", 200, `{"slug": "blog", "name": "Blog\r\u001b]0;pwned\u0007", "visibility": "private"}`)
	api.handle("POST /api/flats/blog/visibility", 422, `{"error": "bad\u001b[31m red"}`)

	for _, args := range [][]string{{"approvals"}, {"info", "blog"}, {"visibility", "blog", "private"}} {
		r := run(t, srv.URL, "", args...)
		all := r.stdout + r.stderr
		if strings.ContainsAny(all, "\x1b\r\a‮\u009b") {
			t.Errorf("%v: raw control characters reached the terminal: %q", args, all)
		}
		if !strings.Contains(all, `\x1b`) {
			t.Errorf("%v: escape not shown in escaped form: %q", args, all)
		}
	}
	// --json output stays valid JSON (the server's encoder already escapes).
	r := run(t, srv.URL, "", "--json", "info", "blog")
	if !strings.Contains(r.stdout, `\u001b`) || strings.Contains(r.stdout, "\x1b") {
		t.Errorf("json: %q", r.stdout)
	}
}

func TestSafeWriterKeepsTextAndSplitRunes(t *testing.T) {
	var buf bytes.Buffer
	w := newSafeWriter(&buf)
	text := "Version 3 is live — café 日本\n\tnote\n"
	b := []byte(text)
	// Split inside multi-byte runes: they must arrive intact, not escaped.
	for i := 0; i < len(b); i += 3 {
		end := min(i+3, len(b))
		if n, err := w.Write(b[i:end]); err != nil || n != end-i {
			t.Fatalf("write: %d %v", n, err)
		}
	}
	if buf.String() != text {
		t.Fatalf("got %q, want %q", buf.String(), text)
	}
	buf.Reset()
	w.Write([]byte{'a', 0xff, 'b', 0x1b, '[', 'm'})
	if got := buf.String(); got != `a\xffb\x1b[m` {
		t.Fatalf("got %q", got)
	}
}

func TestOneLineTruncatesOnRuneBoundary(t *testing.T) {
	s := oneLine(strings.Repeat("日", 200))
	if !utf8.ValidString(s) || !strings.HasSuffix(s, "...") || utf8.RuneCountInString(s) != 120 {
		t.Fatalf("oneLine = %q (%d runes)", s, utf8.RuneCountInString(s))
	}
}

func TestLogsLimitRange(t *testing.T) {
	_, srv := newFakeAPI(t)
	for _, lim := range []string{"0", "1001"} {
		if r := run(t, srv.URL, "", "logs", "blog", "--limit", lim); r.code != ExitUsage {
			t.Errorf("--limit %s: exit %d", lim, r.code)
		}
	}
}

func TestDeployArchiveFile(t *testing.T) {
	api, srv := newFakeAPI(t)
	api.handle("POST /api/flats/blog/versions", 201, `{"version": {"flat": "blog", "number": 1, "files": 1, "size": 5}}`)
	site := t.TempDir()
	writeTree(t, site, map[string]string{"index.html": "hello"})
	tgz, _, err := packDir(site, 0)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "site.tgz")
	if err := os.WriteFile(archive, tgz, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := run(t, srv.URL, "", "deploy", archive, "--flat", "blog", "--save-only"); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := api.last("POST /api/flats/blog/versions").body; !bytes.Equal(got, tgz) {
		t.Fatal("archive was not sent as is")
	}

	// An archive the server would reject for size is refused before upload.
	api.handle("GET /api/status", 200, `{"ok": true, "upload_max_bytes": 10}`)
	big := filepath.Join(t.TempDir(), "big.zip")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	before := len(api.reqs)
	api.mu.Unlock()
	r := run(t, srv.URL, "", "deploy", big, "--flat", "blog")
	if r.code != ExitError || !strings.Contains(r.stderr, "upload limit") {
		t.Fatalf("big archive: exit %d %s", r.code, r.stderr)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, req := range api.reqs[before:] {
		if req.method == "POST" {
			t.Fatal("oversized archive was uploaded")
		}
	}
}

// Inside a git hook GIT_DIR points at the hook's repository; the build
// directory's own repository must still be detected.
func TestDetectGitIgnoresInheritedGitDir(t *testing.T) {
	if testing.Short() {
		t.Skip("runs git")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q")
	writeTree(t, repo, map[string]string{"dist/index.html": "v1"})
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-q", "-m", "first")
	head := gitCmd(t, repo, "rev-parse", "HEAD")

	other := t.TempDir()
	gitCmd(t, other, "init", "-q")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	gi, ok := detectGit(t.Context(), filepath.Join(repo, "dist"))
	if !ok || gi.SHA != head {
		t.Fatalf("got %+v ok=%v, want HEAD %s", gi, ok, head)
	}
}
