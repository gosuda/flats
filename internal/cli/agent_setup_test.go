package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/agentsetup"
)

// setupFx is a home directory, a project and a config directory for one run of
// `flats agent-setup`.
type setupFx struct {
	t                    *testing.T
	home, project, confg string
	env                  map[string]string
}

func newSetupFx(t *testing.T) *setupFx {
	t.Helper()
	root := t.TempDir()
	f := &setupFx{t: t, home: filepath.Join(root, "home"), project: filepath.Join(root, "repo"), confg: filepath.Join(root, "config"), env: map[string]string{}}
	for _, d := range []string{filepath.Join(f.home, ".claude"), filepath.Join(f.project, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// run runs the CLI with no terminal and the given standard input.
func (f *setupFx) run(stdin string, args ...string) result {
	return f.runWith(Env{Stdin: strings.NewReader(stdin)}, args...)
}

func (f *setupFx) runWith(env Env, args ...string) result {
	f.t.Helper()
	var out, errb bytes.Buffer
	env.Stdout, env.Stderr = &out, &errb
	env.Home, env.ConfigDir, env.Cwd = f.home, f.confg, f.project
	env.Getenv = func(k string) string { return f.env[k] }
	code := Run(context.Background(), append([]string{"agent-setup", "docs"}, args...), env)
	return result{code, out.String(), errb.String()}
}

// typed runs the CLI as if standard input were a terminal that holds line.
func (f *setupFx) typed(line string, args ...string) result {
	return f.runWith(Env{Stdin: strings.NewReader(line), IsTerminal: func(io.Reader) bool { return true }}, args...)
}

func (f *setupFx) write(rel, content string) string {
	f.t.Helper()
	p := filepath.Join(f.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func (f *setupFx) backups() []string {
	entries, _ := os.ReadDir(filepath.Join(f.confg, "flats-client", "agent-setup-backups"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestAgentSetupUsageErrors(t *testing.T) {
	f := newSetupFx(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"unknown scope":          {[]string{"--scope", "team"}, "--scope must be user or project"},
		"unknown agent":          {[]string{"--agent", "gemini"}, "--agent must be claude, codex, cursor or all"},
		"project needs an agent": {[]string{"--scope", "project"}, "--agent is required at project scope"},
		"json":                   {[]string{"--json"}, "text only"},
	} {
		r := f.run("", tc.args...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%s: %+v", name, r)
		}
	}
	for name, args := range map[string][]string{"no topic": {"agent-setup"}, "wrong topic": {"agent-setup", "mail"}} {
		var out, errb bytes.Buffer
		code := Run(context.Background(), args, Env{Stdout: &out, Stderr: &errb, Home: f.home, ConfigDir: f.confg, Getenv: func(string) string { return "" }})
		if code != ExitUsage {
			t.Errorf("%s: code %d\n%s", name, code, errb.String())
		}
	}
	// No agent directory in the home directory.
	empty := newSetupFx(t)
	os.Remove(filepath.Join(empty.home, ".claude"))
	if r := empty.run(""); r.code != ExitUsage || !strings.Contains(r.stderr, "no agent found") {
		t.Errorf("no agent found: %+v", r)
	}
	if fileExists(filepath.Join(f.home, ".claude", "CLAUDE.md")) {
		t.Error("a usage error wrote a file")
	}
}

func TestAgentSetupDryRunWritesNothing(t *testing.T) {
	f := newSetupFx(t)
	f.write(".claude/CLAUDE.md", "# rules\n")
	r := f.run("", "--dry-run", "--yes")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Dry run: nothing was written.") {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{
		"Set up document routing to Flats for Claude Code at user scope.",
		filepath.Join(f.home, ".claude", "CLAUDE.md") + "  (add)",
		filepath.Join(f.home, ".claude", "settings.json") + "  (create)",
		"  + " + agentsetup.BeginMarker,
		`+   "deniedMcpServers": [`,
		"Backups of changed files go to " + filepath.Join(f.confg, "flats-client", "agent-setup-backups"),
		"The agent needs Flats connected (see `flats mcp-config`).",
		"The Claude desktop app does not read the connector entry.",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("plan lacks %q:\n%s", want, r.stdout)
		}
	}
	if got := readFile(t, filepath.Join(f.home, ".claude", "CLAUDE.md")); got != "# rules\n" {
		t.Errorf("CLAUDE.md changed: %q", got)
	}
	if fileExists(filepath.Join(f.home, ".claude", "settings.json")) || len(f.backups()) != 0 {
		t.Error("dry run wrote a file or a backup")
	}
}

func TestAgentSetupNeedsConfirmation(t *testing.T) {
	f := newSetupFx(t)
	r := f.run("y\n") // not a terminal: the input is not read
	if r.code != ExitUsage || !strings.Contains(r.stderr, "rerun with --yes") || !strings.Contains(r.stdout, "(create)") {
		t.Fatalf("%+v", r)
	}
	if fileExists(filepath.Join(f.home, ".claude", "CLAUDE.md")) {
		t.Error("wrote a file without confirmation")
	}
	for name, answer := range map[string]string{"empty": "\n", "no": "n\n", "other": "maybe\n", "end of input": ""} {
		r := f.typed(answer)
		if r.code != ExitOK || !strings.Contains(r.stdout, "Apply these changes? [y/N]") || !strings.Contains(r.stdout, "Nothing changed.") {
			t.Errorf("%s: %+v", name, r)
		}
		if fileExists(filepath.Join(f.home, ".claude", "CLAUDE.md")) {
			t.Fatalf("%s: wrote a file after a decline", name)
		}
	}
	for _, answer := range []string{"y\n", "YES\n", " yes \n"} {
		g := newSetupFx(t)
		if r := g.typed(answer); r.code != ExitOK || !strings.Contains(r.stdout, "Done.") {
			t.Errorf("%q: %+v", answer, r)
		}
		if !fileExists(filepath.Join(g.home, ".claude", "CLAUDE.md")) {
			t.Errorf("%q: nothing was written", answer)
		}
	}
	// --yes skips the question on a terminal, which never reads its input.
	h := newSetupFx(t)
	if r := h.typed("n\n", "--yes"); r.code != ExitOK || strings.Contains(r.stdout, "[y/N]") || !fileExists(filepath.Join(h.home, ".claude", "CLAUDE.md")) {
		t.Errorf("--yes on a terminal: %+v", r)
	}
}

func TestAgentSetupAppliesAndIsIdempotent(t *testing.T) {
	f := newSetupFx(t)
	md := f.write(".claude/CLAUDE.md", "# rules\n")
	r := f.run("", "--yes")
	if r.code != ExitOK || !strings.Contains(r.stdout, "CLAUDE.md: updated") || !strings.Contains(r.stdout, "settings.json: created") || !strings.HasSuffix(r.stdout, "Done.\n") {
		t.Fatalf("%+v", r)
	}
	if got := readFile(t, md); !strings.HasPrefix(got, "# rules\n\n"+agentsetup.BeginMarker+"\n") || !strings.HasSuffix(got, agentsetup.EndMarker+"\n") {
		t.Errorf("CLAUDE.md = %q", got)
	}
	settings := readFile(t, filepath.Join(f.home, ".claude", "settings.json"))
	for _, want := range []string{`"serverName": "claude.ai Claude Docs"`, `"Skill(anthropic-skills:docs)"`, `"mcp__claude_ai_Claude_Docs__*"`} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings lack %s:\n%s", want, settings)
		}
	}
	if b := f.backups(); len(b) != 1 || !strings.HasSuffix(b[0], "-CLAUDE.md") {
		t.Errorf("backups = %v, want one of CLAUDE.md", b)
	}
	again := f.run("", "--yes")
	if again.code != ExitOK || !strings.Contains(again.stdout, "Nothing to change.") || len(f.backups()) != 1 {
		t.Errorf("second run: %+v backups %v", again, f.backups())
	}
	if got := readFile(t, md); strings.Count(got, agentsetup.BeginMarker) != 1 {
		t.Errorf("CLAUDE.md holds %d blocks after two runs", strings.Count(got, agentsetup.BeginMarker))
	}
}

func TestAgentSetupUndo(t *testing.T) {
	f := newSetupFx(t)
	md := f.write(".claude/CLAUDE.md", "# rules\n")
	// An entry that was there before the setup is reported, and undo removes it.
	f.write(".claude/settings.json", "{\n  \"permissions\": {\n    \"deny\": [\n      \"Skill(anthropic-skills:docs)\"\n    ]\n  }\n}\n")
	r := f.run("", "--dry-run")
	if !strings.Contains(r.stdout, `already there: permissions.deny: "Skill(anthropic-skills:docs)"; --undo will remove it too`) {
		t.Errorf("setup plan:\n%s", r.stdout)
	}
	f.run("", "--yes")
	undo := f.run("", "--undo", "--dry-run")
	for _, want := range []string{"Undo document routing to Flats", "(remove)", "- " + agentsetup.BeginMarker, "Undo does not reconnect Claude Docs"} {
		if !strings.Contains(undo.stdout, want) {
			t.Errorf("undo plan lacks %q:\n%s", want, undo.stdout)
		}
	}
	for _, setupOnly := range []string{"The agent needs Flats connected", "The Claude desktop app does not read"} {
		if strings.Contains(undo.stdout, setupOnly) {
			t.Errorf("undo plan carries the setup note %q", setupOnly)
		}
	}
	if r := f.run("", "--undo"); r.code != ExitUsage {
		t.Errorf("undo without a terminal or --yes: %+v", r)
	}
	if r := f.run("", "--undo", "--yes"); r.code != ExitOK || !strings.Contains(r.stdout, "removed") {
		t.Fatalf("undo: %+v", r)
	}
	if got := readFile(t, md); got != "# rules\n" {
		t.Errorf("CLAUDE.md after undo = %q", got)
	}
	if got := readFile(t, filepath.Join(f.home, ".claude", "settings.json")); strings.Contains(got, "Skill(") || strings.Contains(got, "claude.ai Claude Docs") {
		t.Errorf("settings after undo:\n%s", got)
	}
	if r := f.run("", "--undo", "--yes"); r.code != ExitOK || !strings.Contains(r.stdout, "Nothing to undo.") {
		t.Errorf("second undo: %+v", r)
	}
}

func TestAgentSetupRefusalWritesNothing(t *testing.T) {
	f := newSetupFx(t)
	f.write(".claude/CLAUDE.md", "# rules\n")
	f.write(".claude/settings.json", "{ not json")
	for _, args := range [][]string{{"--yes"}, {"--dry-run"}, {"--undo", "--yes"}} {
		r := f.run("", args...)
		if r.code != ExitError || !strings.Contains(r.stdout, "(refused)") || !strings.Contains(r.stdout, "Nothing was written") {
			t.Errorf("%v: %+v", args, r)
		}
	}
	if got := readFile(t, filepath.Join(f.home, ".claude", "CLAUDE.md")); got != "# rules\n" || len(f.backups()) != 0 {
		t.Errorf("a refusal changed CLAUDE.md or made backups: %q %v", got, f.backups())
	}
}

func TestAgentSetupDetectsAgentsAndHonorsEnvironment(t *testing.T) {
	f := newSetupFx(t)
	os.MkdirAll(filepath.Join(f.home, "codex-home"), 0o755)
	f.env["CODEX_HOME"] = filepath.Join(f.home, "codex-home")
	f.env["CLAUDE_CONFIG_DIR"] = filepath.Join(f.home, "no-claude-here")
	r := f.run("", "--yes")
	if r.code != ExitOK || !strings.Contains(r.stdout, "for Codex at user scope") || strings.Contains(r.stdout, "Claude Code") {
		t.Fatalf("%+v", r)
	}
	if !fileExists(filepath.Join(f.home, "codex-home", "AGENTS.md")) || fileExists(filepath.Join(f.home, ".claude", "CLAUDE.md")) {
		t.Error("the block went to the wrong place")
	}
}

func TestAgentSetupResolvesRelativeAgentDirectories(t *testing.T) {
	// If the resolution regressed, a relative path would land in the process
	// working directory. Make that a temporary one, not the source tree.
	t.Chdir(t.TempDir())
	f := newSetupFx(t)
	f.env["CLAUDE_CONFIG_DIR"] = "rel-claude"
	r := f.run("", "--agent", "claude", "--yes")
	want := filepath.Join(f.project, "rel-claude", "CLAUDE.md")
	if r.code != ExitOK || !strings.Contains(r.stdout, want+"  (create)") || !fileExists(want) {
		t.Errorf("%+v", r)
	}
	if b := f.backups(); len(b) != 0 {
		t.Errorf("a new file was backed up: %v", b)
	}
}

func TestAgentSetupProjectScope(t *testing.T) {
	f := newSetupFx(t)
	sub := filepath.Join(f.project, "src")
	os.MkdirAll(sub, 0o755)
	f.project = sub // run from a subdirectory; the .git entry is one level up
	r := f.run("", "--scope", "project", "--agent", "all", "--yes")
	root := filepath.Dir(sub)
	if r.code != ExitOK || !strings.Contains(r.stdout, "Project directory: "+root) {
		t.Fatalf("%+v", r)
	}
	for _, p := range []string{"CLAUDE.md", "AGENTS.md", filepath.Join(".cursor", "rules", "flats-documents.mdc")} {
		if !fileExists(filepath.Join(root, p)) {
			t.Errorf("%s was not created at the project root", p)
		}
	}
	if fileExists(filepath.Join(sub, "CLAUDE.md")) || fileExists(filepath.Join(f.home, ".claude", "settings.json")) {
		t.Error("project scope wrote in the subdirectory or a settings file")
	}
	if !strings.Contains(r.stdout, "Claude Docs stays available at project scope") {
		t.Errorf("no project-scope note:\n%s", r.stdout)
	}
	if u := f.run("", "--scope", "project", "--undo", "--yes"); u.code != ExitOK || fileExists(filepath.Join(root, ".cursor", "rules", "flats-documents.mdc")) {
		t.Errorf("project undo: %+v", u)
	}

	// Without a repository the note says so.
	g := newSetupFx(t)
	bare := t.TempDir()
	g.project = bare
	if r := g.run("", "--scope", "project", "--agent", "claude", "--dry-run"); !strings.Contains(r.stdout, "No Git repository was found above "+bare) {
		t.Errorf("no-repository note:\n%s", r.stdout)
	}

	// A project that is the home directory is refused.
	h := newSetupFx(t)
	os.MkdirAll(filepath.Join(h.home, ".git"), 0o755)
	h.project = h.home
	if r := h.run("", "--scope", "project", "--agent", "claude", "--yes"); r.code != ExitError || !strings.Contains(r.stderr, "the home directory") {
		t.Errorf("home as project: %+v", r)
	}
}

func TestAgentSetupCursorUserScopePrintsTheText(t *testing.T) {
	f := newSetupFx(t)
	r := f.run("", "--agent", "cursor", "--yes")
	if r.code != ExitOK || !strings.Contains(r.stdout, "nothing is written; paste this") || !strings.Contains(r.stdout, "    ## Documents") || !strings.Contains(r.stdout, "Nothing to change.") {
		t.Errorf("%+v", r)
	}
	u := f.run("", "--agent", "cursor", "--undo", "--yes")
	if u.code != ExitOK || !strings.Contains(u.stdout, "Undo does not remove text you pasted into Cursor's User Rules.") {
		t.Errorf("undo: %+v", u)
	}
}

func TestAgentSetupReportsAFileThatChangedAndGoesOn(t *testing.T) {
	f := newSetupFx(t)
	os.MkdirAll(filepath.Join(f.home, ".codex"), 0o755)
	md := f.write(".claude/CLAUDE.md", "before\n")
	// The reader changes CLAUDE.md after the plan, as the question is asked.
	in := &changeOnRead{path: md, then: "someone else\n", reply: "y\n"}
	r := f.runWith(Env{Stdin: in, IsTerminal: func(io.Reader) bool { return true }})
	if r.code != ExitError || !strings.Contains(r.stdout, "failed: "+md) || !strings.Contains(r.stdout, "changed while the command ran") {
		t.Fatalf("%+v", r)
	}
	if got := readFile(t, md); got != "someone else\n" {
		t.Errorf("CLAUDE.md = %q", got)
	}
	// The other files were still written.
	if !fileExists(filepath.Join(f.home, ".claude", "settings.json")) || !fileExists(filepath.Join(f.home, ".codex", "AGENTS.md")) {
		t.Errorf("the other files were not written:\n%s", r.stdout)
	}
	// Undo over this mix of written and unwritten targets leaves the changed
	// file alone and takes the setup out of the others.
	if u := f.run("", "--undo", "--yes"); u.code != ExitOK {
		t.Fatalf("undo after a partial failure: %+v", u)
	}
	if got := readFile(t, md); got != "someone else\n" {
		t.Errorf("undo touched the file that was never set up: %q", got)
	}
	if got := readFile(t, filepath.Join(f.home, ".codex", "AGENTS.md")); got != "" {
		t.Errorf("AGENTS.md after undo = %q", got)
	}
	if got := readFile(t, filepath.Join(f.home, ".claude", "settings.json")); strings.Contains(got, "Skill(") || strings.Contains(got, "claude.ai Claude Docs") {
		t.Errorf("settings after undo:\n%s", got)
	}
	// A rerun of the setup finishes the rest.
	if again := f.run("", "--yes"); again.code != ExitOK || !strings.Contains(again.stdout, "CLAUDE.md: updated") {
		t.Errorf("rerun: %+v", again)
	}
}

// syncBuffer is a buffer that one goroutine writes while another reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type changeOnRead struct {
	path, then, reply string
	done              bool
}

func (c *changeOnRead) Read(p []byte) (int, error) {
	if !c.done {
		c.done = true
		if err := os.WriteFile(c.path, []byte(c.then), 0o644); err != nil {
			return 0, err
		}
		return copy(p, c.reply), nil
	}
	return 0, io.EOF
}

func TestAgentSetupReportsWriteFailuresPerFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	f := newSetupFx(t)
	os.MkdirAll(filepath.Join(f.home, ".codex"), 0o755)
	f.write(".claude/CLAUDE.md", "x\n")
	if err := os.Chmod(filepath.Join(f.home, ".claude"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(f.home, ".claude"), 0o755) })
	r := f.run("", "--yes")
	if r.code != ExitError || strings.Count(r.stdout, "failed: ") != 2 || !strings.Contains(r.stdout, "AGENTS.md: created") || !strings.Contains(r.stdout, "Run the command again") {
		t.Errorf("%+v", r)
	}
}

func TestAgentSetupInterruptedPrompt(t *testing.T) {
	f := newSetupFx(t)
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	var out, errb syncBuffer
	env := Env{Stdin: pr, Stdout: &out, Stderr: &errb, Home: f.home, ConfigDir: f.confg, Cwd: f.project,
		Getenv: func(string) string { return "" }, IsTerminal: func(io.Reader) bool { return true }}
	done := make(chan int, 1)
	go func() { done <- Run(ctx, []string{"agent-setup", "docs"}, env) }()
	for !strings.Contains(out.String(), "[y/N]") { // the plan is out and the question is asked
		select {
		case code := <-done:
			t.Fatalf("exited early with %d", code)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if code := <-done; code != ExitError {
		t.Errorf("code = %d, want %d after Ctrl-C", code, ExitError)
	}
	if fileExists(filepath.Join(f.home, ".claude", "CLAUDE.md")) {
		t.Error("wrote a file after the prompt was interrupted")
	}
	if got := errb.String(); got != "flats: interrupted; nothing was written\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestAgentSetupHelp(t *testing.T) {
	r := run(t, "", "", "help", "agent-setup")
	if r.code != ExitOK || !strings.HasPrefix(r.stdout, "usage: flats agent-setup docs [--agent claude|codex|cursor|all] [--scope user|project] [--dry-run] [--yes] [--undo]\n\n") {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"It shows every change first and asks before it writes.", "--undo reverses the setup", "see `flats mcp-config`"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help lacks %q:\n%s", want, r.stdout)
		}
	}
	list := run(t, "", "", "help")
	if !strings.Contains(list.stdout, "\n  agent-setup make Flats your agents' document tool (opt-in, with a preview)\n") {
		t.Errorf("command list:\n%s", list.stdout)
	}
}
