package agentsetup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func getenv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// fixture is a home directory, a project and a backup directory.
type fixture struct {
	home, project, backups string
	env                    map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{home: filepath.Join(root, "home"), project: filepath.Join(root, "work", "repo"), backups: filepath.Join(root, "backups"), env: map[string]string{}}
	for _, d := range []string{f.home, filepath.Join(f.project, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) plan(t *testing.T, scope Scope, undo bool, agents ...Agent) *Plan {
	t.Helper()
	p, err := NewPlan(Options{Agents: agents, Scope: scope, Undo: undo, Home: f.home, Getenv: getenv(f.env), Cwd: f.project})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fixture) apply(t *testing.T, p *Plan) {
	t.Helper()
	for _, it := range p.Items {
		if err := Apply(it, ApplyOptions{BackupDir: f.backups}); err != nil {
			t.Fatal(err)
		}
	}
}

func paths(p *Plan) []string {
	var out []string
	for _, it := range p.Items {
		out = append(out, it.Path)
	}
	return out
}

func TestUserDirsResolveRelativePaths(t *testing.T) {
	f := newFixture(t)
	f.env["CLAUDE_CONFIG_DIR"] = "cfg/claude"
	f.env["CODEX_HOME"] = "../codex-home/"
	d := UserDirs(f.home, f.project, getenv(f.env))
	if d.Claude != filepath.Join(f.project, "cfg", "claude") || d.Codex != filepath.Join(filepath.Dir(f.project), "codex-home") {
		t.Errorf("UserDirs = %+v", d)
	}
	f.env["CLAUDE_CONFIG_DIR"] = f.home + "/abs//dir/"
	if got := UserDirs(f.home, f.project, getenv(f.env)).Claude; got != filepath.Join(f.home, "abs", "dir") {
		t.Errorf("an absolute value is cleaned: %q", got)
	}
}

func TestUserDirsAndDetect(t *testing.T) {
	f := newFixture(t)
	if got := Detect(f.home, f.project, getenv(f.env)); len(got) != 0 {
		t.Errorf("Detect with no directories = %v", got)
	}
	for _, d := range []string{".claude", ".cursor"} {
		os.MkdirAll(filepath.Join(f.home, d), 0o755)
	}
	if got := Detect(f.home, f.project, getenv(f.env)); len(got) != 2 || got[0] != Claude || got[1] != Cursor {
		t.Errorf("Detect = %v, want claude and cursor", got)
	}
	// An environment override moves the directory Detect looks at.
	f.env["CLAUDE_CONFIG_DIR"] = filepath.Join(f.home, "elsewhere")
	f.env["CODEX_HOME"] = filepath.Join(f.home, "codex-home")
	os.MkdirAll(f.env["CODEX_HOME"], 0o755)
	if got := Detect(f.home, f.project, getenv(f.env)); len(got) != 2 || got[0] != Codex || got[1] != Cursor {
		t.Errorf("Detect with overrides = %v, want codex and cursor", got)
	}
	d := UserDirs(f.home, f.project, getenv(f.env))
	if d.Claude != f.env["CLAUDE_CONFIG_DIR"] || d.Codex != f.env["CODEX_HOME"] || d.Cursor != filepath.Join(f.home, ".cursor") {
		t.Errorf("UserDirs = %+v", d)
	}
}

func TestProjectDir(t *testing.T) {
	f := newFixture(t)
	sub := filepath.Join(f.project, "a", "b")
	os.MkdirAll(sub, 0o755)
	if dir, found := ProjectDir(sub); dir != f.project || !found {
		t.Errorf("ProjectDir(sub) = %q, %v", dir, found)
	}
	// A .git file, as in a worktree or submodule, also marks the root.
	nested := filepath.Join(f.project, "vendor", "lib")
	write(t, filepath.Join(nested, ".git"), "gitdir: x")
	if dir, found := ProjectDir(filepath.Join(nested, "pkg")); dir != nested || !found {
		// pkg does not exist, but the walk only needs the path.
		t.Errorf("nested ProjectDir = %q, %v", dir, found)
	}
	bare := t.TempDir()
	if dir, found := ProjectDir(bare); dir != bare || found {
		t.Errorf("ProjectDir without a repository = %q, %v", dir, found)
	}
}

func TestPlanUserScopeTargets(t *testing.T) {
	f := newFixture(t)
	p := f.plan(t, User, false, Claude, Codex, Cursor)
	want := []string{
		filepath.Join(f.home, ".claude", "CLAUDE.md"),
		filepath.Join(f.home, ".claude", "settings.json"),
		filepath.Join(f.home, ".codex", "AGENTS.md"),
		"Cursor Settings > Rules > User Rules",
	}
	if got := paths(p); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("targets = %v, want %v", got, want)
	}
	for _, it := range p.Items[:3] {
		if it.Action != Create || it.Err != nil || len(it.After) == 0 {
			t.Errorf("%s: action %v err %v", it.Path, it.Action, it.Err)
		}
	}
	if cur := p.Items[3]; cur.Kind != KindPrint || cur.Text != RoutingText {
		t.Errorf("cursor item = %+v", cur)
	}
	if !p.Changes() || len(p.Refused()) != 0 {
		t.Errorf("Changes %v, Refused %v", p.Changes(), p.Refused())
	}
}

func TestPlanEnvironmentOverridesMoveTheTargets(t *testing.T) {
	f := newFixture(t)
	f.env["CLAUDE_CONFIG_DIR"] = filepath.Join(f.home, "cc")
	f.env["CODEX_HOME"] = filepath.Join(f.home, "cx")
	p := f.plan(t, User, false, Claude, Codex)
	want := []string{filepath.Join(f.home, "cc", "CLAUDE.md"), filepath.Join(f.home, "cc", "settings.json"), filepath.Join(f.home, "cx", "AGENTS.md")}
	if got := paths(p); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

func TestPlanProjectScope(t *testing.T) {
	f := newFixture(t)
	f.env["CLAUDE_CONFIG_DIR"] = filepath.Join(f.home, "cc")
	sub := filepath.Join(f.project, "src")
	os.MkdirAll(sub, 0o755)
	p, err := NewPlan(Options{Agents: AllAgents, Scope: Project, Home: f.home, Getenv: getenv(f.env), Cwd: sub})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(f.project, "CLAUDE.md"),
		filepath.Join(f.project, "AGENTS.md"),
		filepath.Join(f.project, ".cursor", "rules", "flats-documents.mdc"),
	}
	if got := paths(p); strings.Join(got, "\n") != strings.Join(want, "\n") || p.Project != f.project || p.NoRepo {
		t.Errorf("targets = %v project %q norepo %v", got, p.Project, p.NoRepo)
	}
	for _, it := range p.Items {
		if it.Kind == KindSettings {
			t.Errorf("project scope plans a settings file: %s", it.Path)
		}
	}

	bare := t.TempDir()
	if p, err = NewPlan(Options{Agents: []Agent{Claude}, Scope: Project, Home: f.home, Getenv: getenv(f.env), Cwd: bare}); err != nil || !p.NoRepo || p.Project != bare {
		t.Errorf("without a repository: %v, project %q, norepo %v", err, p.Project, p.NoRepo)
	}
}

func TestPlanRefusesAProjectThatIsTheHomeDirectory(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(filepath.Join(f.home, ".git"), 0o755)
	sub := filepath.Join(f.home, "notes")
	os.MkdirAll(sub, 0o755)
	_, err := NewPlan(Options{Agents: []Agent{Claude}, Scope: Project, Home: f.home, Getenv: getenv(f.env), Cwd: sub})
	if !errors.Is(err, ErrProjectIsHome) {
		t.Errorf("err = %v, want ErrProjectIsHome", err)
	}
	if _, err = NewPlan(Options{Agents: []Agent{Claude}, Scope: User, Home: f.home, Getenv: getenv(f.env), Cwd: sub}); err != nil {
		t.Errorf("user scope: %v", err)
	}

	// The same directory under another spelling is still the home directory.
	alias := filepath.Join(filepath.Dir(f.home), "home-alias")
	if err := os.Symlink(f.home, alias); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"home spelled through a link":      {Home: alias, Cwd: sub},
		"working directory through a link": {Home: f.home, Cwd: filepath.Join(alias, "notes")},
	} {
		o.Agents, o.Scope, o.Getenv = []Agent{Claude}, Project, getenv(f.env)
		if _, err := NewPlan(o); !errors.Is(err, ErrProjectIsHome) {
			t.Errorf("%s: err = %v, want ErrProjectIsHome", name, err)
		}
	}
}

// A target under a directory link that points nowhere is refused while
// planning, so a setup that also has good targets writes none of them.
func TestPlanRefusesATargetUnderADanglingDirectoryLink(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink(filepath.Join(f.home, "missing"), filepath.Join(f.home, ".claude")); err != nil {
		t.Fatal(err)
	}
	p := f.plan(t, User, false, Claude, Codex)
	if len(p.Items) != 3 {
		t.Fatalf("items = %v", paths(p))
	}
	for _, it := range p.Items[:2] {
		if it.Err == nil || !strings.Contains(it.Err.Error(), "symbolic link that cannot be followed") {
			t.Errorf("%s: err = %v", it.Path, it.Err)
		}
	}
	if p.Items[2].Err != nil || len(p.Refused()) != 2 {
		t.Errorf("codex err %v, refused %d", p.Items[2].Err, len(p.Refused()))
	}
	if undo := f.plan(t, User, true, Claude); len(undo.Refused()) != 0 {
		t.Errorf("undo has nothing to look at and refuses nothing: %v", undo.Refused())
	}
}

// A file under a linked directory is backed up under the hash of its resolved
// path, as for a linked file.
func TestBackupNamesUseTheResolvedPath(t *testing.T) {
	f := newFixture(t)
	real := filepath.Join(f.home, "dotfiles", "claude")
	write(t, filepath.Join(real, "CLAUDE.md"), "# mine\n")
	if err := os.Symlink(real, filepath.Join(f.home, ".claude")); err != nil {
		t.Fatal(err)
	}
	p := f.plan(t, User, false, Claude)
	want, _ := filepath.EvalSymlinks(filepath.Join(real, "CLAUDE.md"))
	if it := p.Items[0]; it.Real != want {
		t.Fatalf("real = %q, want %q", it.Real, want)
	}
	f.apply(t, p)
	sum := sha256.Sum256([]byte(want))
	entries, _ := os.ReadDir(f.backups)
	if len(entries) != 1 || !strings.Contains(entries[0].Name(), "-"+hex.EncodeToString(sum[:4])+"-CLAUDE.md") {
		t.Errorf("backups = %v, want the hash of %s", entries, want)
	}
}

// codexDir returns where the Codex files of a scope live in the fixture.
func (f *fixture) codexDir(scope Scope) string {
	if scope == User {
		return filepath.Join(f.home, ".codex")
	}
	return f.project
}

// In the Codex home, Codex reads the first of AGENTS.override.md and AGENTS.md
// that has content. In a project directory it takes the first of the two that
// is a regular file and only then drops it if it is empty, so an empty
// override there hides AGENTS.md.
func TestPlanCodexFilePicksTheFileCodexReads(t *testing.T) {
	for _, scope := range []Scope{User, Project} {
		emptyOverride := "AGENTS.md"
		if scope == Project {
			emptyOverride = "AGENTS.override.md"
		}
		for name, tc := range map[string]struct {
			setup func(t *testing.T, override string)
			want  string
		}{
			"no override":              {func(t *testing.T, override string) {}, "AGENTS.md"},
			"override with content":    {func(t *testing.T, override string) { write(t, override, "override\n") }, "AGENTS.override.md"},
			"empty override":           {func(t *testing.T, override string) { write(t, override, "") }, emptyOverride},
			"whitespace-only override": {func(t *testing.T, override string) { write(t, override, " \n\t\n") }, emptyOverride},
			"override is a directory":  {func(t *testing.T, override string) { os.MkdirAll(override, 0o755) }, "AGENTS.md"},
			"override links to an empty file": {func(t *testing.T, override string) {
				real := filepath.Join(filepath.Dir(override), "real-override.md")
				write(t, real, "")
				if err := os.Symlink(real, override); err != nil {
					t.Fatal(err)
				}
			}, emptyOverride},
			"override links to a directory": {func(t *testing.T, override string) {
				real := filepath.Join(filepath.Dir(override), "real-override")
				os.MkdirAll(real, 0o755)
				if err := os.Symlink(real, override); err != nil {
					t.Fatal(err)
				}
			}, "AGENTS.md"},
			"override is a dangling link": {func(t *testing.T, override string) {
				os.MkdirAll(filepath.Dir(override), 0o755)
				if err := os.Symlink(filepath.Join(filepath.Dir(override), "nowhere"), override); err != nil {
					t.Fatal(err)
				}
			}, "AGENTS.md"},
		} {
			t.Run(string(scope)+"/"+name, func(t *testing.T) {
				f := newFixture(t)
				dir := f.codexDir(scope)
				override, plain := filepath.Join(dir, "AGENTS.override.md"), filepath.Join(dir, "AGENTS.md")
				write(t, plain, "my own global rules\n")
				tc.setup(t, override)
				overrideBefore := ""
				if b, err := os.ReadFile(override); err == nil {
					overrideBefore = string(b)
				}
				p := f.plan(t, scope, false, Codex)
				if got := paths(p); len(got) != 1 || filepath.Base(got[0]) != tc.want || p.Items[0].Err != nil {
					t.Fatalf("targets = %v (err %v), want %s", got, p.Items[0].Err, tc.want)
				}
				f.apply(t, p)
				if tc.want == "AGENTS.md" {
					if got := read(t, plain); !strings.HasPrefix(got, "my own global rules\n\n"+BeginMarker) {
						t.Errorf("AGENTS.md = %q", got)
					}
					if b, err := os.ReadFile(override); err == nil && string(b) != overrideBefore {
						t.Errorf("the override changed: %q", b)
					}
				} else if got := read(t, plain); got != "my own global rules\n" || !strings.Contains(read(t, override), BeginMarker) {
					t.Errorf("the block went to the wrong file: AGENTS.md %q", got)
				}
			})
		}
	}
}

// Undo looks at both files in either scope, so it finds a block in either,
// even when the override file appeared or got content after the setup.
func TestPlanCodexUndoLooksAtBothFiles(t *testing.T) {
	for _, scope := range []Scope{User, Project} {
		t.Run(string(scope), func(t *testing.T) {
			f := newFixture(t)
			dir := f.codexDir(scope)
			f.apply(t, f.plan(t, scope, false, Codex)) // the block goes into AGENTS.md
			write(t, filepath.Join(dir, "AGENTS.override.md"), "new override\n")
			undo := f.plan(t, scope, true, Codex)
			if len(undo.Items) != 2 {
				t.Fatalf("undo items = %v", paths(undo))
			}
			f.apply(t, undo)
			if got := read(t, filepath.Join(dir, "AGENTS.md")); strings.Contains(got, BeginMarker) {
				t.Errorf("the block is still in AGENTS.md: %q", got)
			}
			if got := read(t, filepath.Join(dir, "AGENTS.override.md")); got != "new override\n" {
				t.Errorf("undo changed the override: %q", got)
			}
		})
	}
}

func TestPlanCodexSizeLimit(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.home, ".codex", "AGENTS.md")
	blockBytes := len(joinLines(blockLines(), "\n"))
	// The text, a blank line and the block make exactly codexLimit bytes.
	fits := strings.Repeat("a", codexLimit-blockBytes-2) + "\n"
	write(t, path, fits)
	if it := f.plan(t, User, false, Codex).Items[0]; it.Err != nil || len(it.After) != codexLimit {
		t.Fatalf("a file that just fits: err %v, %d bytes", it.Err, len(it.After))
	}
	write(t, path, fits+"b")
	if it := f.plan(t, User, false, Codex).Items[0]; it.Err == nil || !strings.Contains(it.Err.Error(), "Codex") {
		t.Errorf("a file one byte over: err = %v", it.Err)
	}

	// A file that is over the limit but already current stays unchanged, and an
	// undo that shrinks it is never refused.
	set, _, _ := SetBlock([]byte(strings.Repeat("a", codexLimit) + "\n"))
	write(t, path, string(set))
	if it := f.plan(t, User, false, Codex).Items[0]; it.Err != nil || it.Action != Unchanged {
		t.Errorf("over the limit but current: err %v action %v", it.Err, it.Action)
	}
	if it := f.plan(t, User, true, Codex).Items[0]; it.Err != nil || it.Action != Remove {
		t.Errorf("undo of a large file: err %v action %v", it.Err, it.Action)
	}
}

func TestPlanRefusals(t *testing.T) {
	f := newFixture(t)
	claude := filepath.Join(f.home, ".claude")
	write(t, filepath.Join(claude, "CLAUDE.md"), BeginMarker+"\nno end\n")
	write(t, filepath.Join(claude, "settings.json"), `{"permissions": []}`)
	os.MkdirAll(filepath.Join(f.home, ".codex", "AGENTS.md"), 0o755) // a directory
	if err := os.Symlink(filepath.Join(f.home, "nowhere"), filepath.Join(f.project, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	user := f.plan(t, User, false, Claude, Codex)
	proj := f.plan(t, Project, false, Claude)
	for _, it := range append(user.Items, proj.Items...) {
		if it.Err == nil {
			t.Errorf("%s is not refused", it.Path)
		}
	}
	if len(user.Refused()) != 3 || user.Changes() {
		t.Errorf("Refused %d, Changes %v", len(user.Refused()), user.Changes())
	}
	if !errors.Is(user.Items[0].Err, ErrBlock) || !errors.Is(user.Items[1].Err, ErrSettings) {
		t.Errorf("errors = %v, %v", user.Items[0].Err, user.Items[1].Err)
	}
	if err := proj.Items[0].Err; err == nil || !strings.Contains(err.Error(), "symbolic link that cannot be followed") {
		t.Errorf("dangling link: %v", err)
	}
	if err := user.Items[2].Err; err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory target: %v", err)
	}
}

func TestSymbolicLinksAreFollowedAndKept(t *testing.T) {
	f := newFixture(t)
	dotfiles := filepath.Join(f.home, "dotfiles")
	write(t, filepath.Join(dotfiles, "CLAUDE.md"), "# mine\n")
	claude := filepath.Join(f.home, ".claude")
	os.MkdirAll(claude, 0o755)
	if err := os.Symlink(filepath.Join(dotfiles, "CLAUDE.md"), filepath.Join(claude, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	p := f.plan(t, User, false, Claude)
	it := p.Items[0]
	want, _ := filepath.EvalSymlinks(filepath.Join(dotfiles, "CLAUDE.md"))
	if !it.Link || it.Real != want {
		t.Fatalf("link %v real %q, want %q", it.Link, it.Real, want)
	}
	f.apply(t, p)
	fi, err := os.Lstat(filepath.Join(claude, "CLAUDE.md"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v %v", fi, err)
	}
	if !strings.Contains(read(t, filepath.Join(dotfiles, "CLAUDE.md")), BeginMarker) {
		t.Error("the link target was not edited")
	}
	f.apply(t, f.plan(t, User, true, Claude))
	if got := read(t, filepath.Join(dotfiles, "CLAUDE.md")); got != "# mine\n" {
		t.Errorf("undo left %q", got)
	}
	// An empty file behind a link stays after undo.
	write(t, filepath.Join(dotfiles, "empty.md"), "")
	os.Remove(filepath.Join(claude, "CLAUDE.md"))
	os.Symlink(filepath.Join(dotfiles, "empty.md"), filepath.Join(claude, "CLAUDE.md"))
	f.apply(t, f.plan(t, User, false, Claude))
	f.apply(t, f.plan(t, User, true, Claude))
	if !exists(filepath.Join(dotfiles, "empty.md")) || !exists(filepath.Join(claude, "CLAUDE.md")) {
		t.Error("undo deleted a file behind a link")
	}
}

func TestSetupThenUndoRestoresTheFiles(t *testing.T) {
	f := newFixture(t)
	claude := filepath.Join(f.home, ".claude")
	files := map[string]string{
		filepath.Join(claude, "CLAUDE.md"):           "# My rules\n\nBe brief.\n",
		filepath.Join(claude, "settings.json"):       "{\n  \"permissions\": {\n    \"deny\": [\n      \"Bash(rm:*)\"\n    ]\n  },\n  \"deniedMcpServers\": [\n    {\"serverName\": \"other\"}\n  ]\n}\n",
		filepath.Join(f.home, ".codex", "AGENTS.md"): "codex notes\n",
		filepath.Join(f.project, "CLAUDE.md"):        "# project\n",
		filepath.Join(f.project, "AGENTS.md"):        "agents\r\nlines\r\n",
	}
	for p, c := range files {
		write(t, p, c)
	}
	for _, scope := range []Scope{User, Project} {
		f.apply(t, f.plan(t, scope, false, AllAgents...))
	}
	for p, c := range files {
		if got := read(t, p); got == c || !strings.Contains(got, "claude.ai Claude Docs") && !strings.Contains(got, BeginMarker) {
			t.Errorf("%s was not changed:\n%s", p, got)
		}
	}
	if !exists(filepath.Join(f.project, ".cursor", "rules", "flats-documents.mdc")) {
		t.Error("the Cursor rule was not created")
	}
	for _, scope := range []Scope{User, Project} {
		f.apply(t, f.plan(t, scope, true, AllAgents...))
	}
	for p, c := range files {
		if got := read(t, p); got != c {
			t.Errorf("%s after undo:\n got %q\nwant %q", p, got, c)
		}
	}
	if exists(filepath.Join(f.project, ".cursor", "rules", "flats-documents.mdc")) {
		t.Error("the Cursor rule is still there")
	}
	if !exists(filepath.Join(f.project, ".cursor", "rules")) {
		t.Error("undo removed the directory around the rule file")
	}
	// Nothing is left to do, and a second undo changes nothing.
	for _, scope := range []Scope{User, Project} {
		if p := f.plan(t, scope, true, AllAgents...); p.Changes() {
			t.Errorf("a second undo would change files at %s scope", scope)
		}
	}
}

func TestUndoNeverDeletesInstructionFiles(t *testing.T) {
	f := newFixture(t)
	only, _, _ := SetBlock(nil) // a file that holds nothing but the block
	for _, path := range []string{
		filepath.Join(f.home, ".claude", "CLAUDE.md"),
		filepath.Join(f.project, "CLAUDE.md"),
	} {
		write(t, path, "") // an empty file that exists before the setup
	}
	for _, path := range []string{
		filepath.Join(f.home, ".codex", "AGENTS.override.md"),
		filepath.Join(f.project, "AGENTS.override.md"),
	} {
		write(t, path, string(only))
	}
	for _, scope := range []Scope{User, Project} {
		f.apply(t, f.plan(t, scope, false, Claude, Codex))
		f.apply(t, f.plan(t, scope, true, Claude, Codex))
	}
	for _, path := range []string{
		filepath.Join(f.home, ".claude", "CLAUDE.md"),
		filepath.Join(f.home, ".codex", "AGENTS.override.md"),
		filepath.Join(f.project, "CLAUDE.md"),
		filepath.Join(f.project, "AGENTS.override.md"),
	} {
		if !exists(path) || read(t, path) != "" {
			t.Errorf("%s: exists %v content %q, want an empty file", path, exists(path), read(t, path))
		}
	}
	// Files that setup created stay too, empty or as an empty-array object.
	g := newFixture(t)
	g.apply(t, g.plan(t, User, false, Claude))
	g.apply(t, g.plan(t, User, true, Claude))
	if got := read(t, filepath.Join(g.home, ".claude", "CLAUDE.md")); got != "" {
		t.Errorf("CLAUDE.md after undo = %q", got)
	}
	if got := read(t, filepath.Join(g.home, ".claude", "settings.json")); !strings.Contains(got, `"deniedMcpServers": [`) || strings.Contains(got, "Skill(") {
		t.Errorf("settings.json after undo = %q", got)
	}
}

func TestCursorRule(t *testing.T) {
	f := newFixture(t)
	rule := filepath.Join(f.project, ".cursor", "rules", "flats-documents.mdc")
	f.apply(t, f.plan(t, Project, false, Cursor))
	if got := read(t, rule); got != string(CursorRule()) {
		t.Fatalf("new rule = %q", got)
	}
	if p := f.plan(t, Project, false, Cursor); p.Changes() {
		t.Error("a second setup would change the rule")
	}
	// Other content: undo keeps the file with that content, and setup refuses it.
	write(t, rule, read(t, rule)+"\nMy own note.\n")
	f.apply(t, f.plan(t, Project, true, Cursor))
	left := read(t, rule)
	if strings.Contains(left, BeginMarker) || !strings.Contains(left, "My own note.") {
		t.Errorf("after undo with other content: %q", left)
	}
	if it := f.plan(t, Project, false, Cursor).Items[0]; it.Err == nil || !strings.Contains(it.Err.Error(), "by hand") {
		t.Errorf("setup of a markerless rule with other content: %v", it.Err)
	}
	// Front matter only: setup accepts it and appends the block.
	write(t, rule, cursorFrontMatter)
	p := f.plan(t, Project, false, Cursor)
	if it := p.Items[0]; it.Err != nil || it.Action != Add {
		t.Fatalf("front matter only: %v %v", it.Err, it.Action)
	}
	f.apply(t, p)
	if got := read(t, rule); got != string(CursorRule()) {
		t.Errorf("rule = %q", got)
	}
	// A stale block is replaced.
	write(t, rule, cursorFrontMatter+"\n"+BeginMarker+"\nold\n"+EndMarker+"\n")
	if it := f.plan(t, Project, false, Cursor).Items[0]; it.Action != Replace {
		t.Errorf("stale rule action = %v", it.Action)
	}
	// Undo deletes the file when only front matter is left, whatever it says.
	write(t, rule, "---\nalwaysApply: false\nglobs: *.md\n---\n\n"+BeginMarker+"\nold\n"+EndMarker+"\n")
	p = f.plan(t, Project, true, Cursor)
	if p.Items[0].Action != Delete {
		t.Fatalf("undo action = %v", p.Items[0].Action)
	}
	f.apply(t, p)
	if exists(rule) {
		t.Error("the rule file is still there")
	}
}

func TestCursorRuleThroughALinkKeepsTheLink(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(f.home, "shared", "flats-documents.mdc")
	write(t, target, string(CursorRule()))
	rule := filepath.Join(f.project, ".cursor", "rules", "flats-documents.mdc")
	os.MkdirAll(filepath.Dir(rule), 0o755)
	if err := os.Symlink(target, rule); err != nil {
		t.Fatal(err)
	}
	p := f.plan(t, Project, true, Cursor)
	it := p.Items[0]
	if it.Action != Remove || len(it.Notes) == 0 {
		t.Fatalf("action %v notes %v", it.Action, it.Notes)
	}
	f.apply(t, p)
	if !exists(rule) || !exists(target) || strings.Contains(read(t, target), BeginMarker) {
		t.Error("the link or its target was removed, or the block stayed")
	}
	// The front-matter-only file accepts a new setup.
	if it := f.plan(t, Project, false, Cursor).Items[0]; it.Err != nil || it.Action != Add {
		t.Errorf("setup after that: %v %v", it.Err, it.Action)
	}
}

func TestApplyBacksUpAndKeepsModes(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.home, ".claude", "CLAUDE.md")
	write(t, path, "# rules\n")
	os.Chmod(path, 0o640)
	p := f.plan(t, User, false, Claude)
	now := time.Date(2026, 10, 27, 12, 30, 45, 0, time.UTC)
	for _, it := range p.Items {
		if err := Apply(it, ApplyOptions{BackupDir: f.backups, Now: func() time.Time { return now }}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(f.backups)
	if err != nil || len(entries) != 1 {
		t.Fatalf("backups = %v, %v (settings.json did not exist, so only CLAUDE.md is backed up)", entries, err)
	}
	name := entries[0].Name()
	if !strings.HasPrefix(name, "20261027T123045Z-") || !strings.HasSuffix(name, "-CLAUDE.md") || len(name) != len("20261027T123045Z-")+8+len("-CLAUDE.md") {
		t.Errorf("backup name = %q", name)
	}
	if got := read(t, filepath.Join(f.backups, name)); got != "# rules\n" {
		t.Errorf("backup content = %q", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640 kept", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(f.home, ".claude", "settings.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("new settings mode = %v, want 0600", fi.Mode().Perm())
	}
	// A second backup of the same file within the same second does not collide.
	write(t, path, "# rules again\n")
	again := f.plan(t, User, false, Claude)
	if err := Apply(again.Items[0], ApplyOptions{BackupDir: f.backups, Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	if entries, _ = os.ReadDir(f.backups); len(entries) != 2 {
		t.Errorf("backups = %d, want 2", len(entries))
	}

	g := newFixture(t)
	proj := g.plan(t, Project, false, Claude)
	g.apply(t, proj)
	if fi, _ := os.Stat(filepath.Join(g.project, "CLAUDE.md")); fi.Mode().Perm() != 0o644 {
		t.Errorf("new instruction file mode = %v, want 0644", fi.Mode().Perm())
	}
	if entries, _ = os.ReadDir(g.backups); len(entries) != 0 {
		t.Errorf("a new file was backed up: %v", entries)
	}
}

func TestApplyChangesNothingWhenTheFileMoved(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.home, ".claude", "CLAUDE.md")
	write(t, path, "before\n")
	p := f.plan(t, User, false, Claude)
	write(t, path, "someone else wrote this\n")
	err := Apply(p.Items[0], ApplyOptions{BackupDir: f.backups})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("err = %v, want ErrChanged", err)
	}
	if got := read(t, path); got != "someone else wrote this\n" {
		t.Errorf("the file was overwritten: %q", got)
	}
	if left := tempFiles(t, filepath.Dir(path)); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}

	// A file created after the plan is not overwritten either.
	g := newFixture(t)
	made := filepath.Join(g.home, ".claude", "CLAUDE.md")
	q := g.plan(t, User, false, Claude)
	write(t, made, "appeared\n")
	if err := Apply(q.Items[0], ApplyOptions{BackupDir: g.backups}); !errors.Is(err, ErrChanged) {
		t.Errorf("err = %v, want ErrChanged", err)
	}
	if got := read(t, made); got != "appeared\n" {
		t.Errorf("content = %q", got)
	}
	// A deleted file counts as changed too.
	h := newFixture(t)
	gone := filepath.Join(h.home, ".claude", "CLAUDE.md")
	write(t, gone, "x\n")
	r := h.plan(t, User, false, Claude)
	os.Remove(gone)
	if err := Apply(r.Items[0], ApplyOptions{BackupDir: h.backups}); !errors.Is(err, ErrChanged) {
		t.Errorf("err = %v, want ErrChanged", err)
	}
}

func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".flats-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// A failure after the temporary file exists must not leave it behind. A
// directory where the file should be makes the recheck fail at that point.
func TestApplyLeavesNoTemporaryFileAfterTheTempFileExists(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.home, "d")
	target := filepath.Join(dir, "CLAUDE.md")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	it := &Item{Kind: KindBlock, Path: target, Real: target, Action: Create, After: []byte("x\n"), mode: 0o644, dirMode: 0o755}
	if err := Apply(it, ApplyOptions{BackupDir: f.backups}); !errors.Is(err, ErrChanged) {
		t.Fatalf("err = %v, want ErrChanged", err)
	}
	if left := tempFiles(t, dir); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestApplyStopsWhenTheBackupFails(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.home, ".claude")
	path := filepath.Join(dir, "CLAUDE.md")
	write(t, path, "x\n")
	p := f.plan(t, User, false, Claude)
	// The backup directory is a file, so the backup fails before any write.
	blocker := filepath.Join(f.home, "blocker")
	write(t, blocker, "")
	err := Apply(p.Items[0], ApplyOptions{BackupDir: filepath.Join(blocker, "sub")})
	if err == nil || read(t, path) != "x\n" {
		t.Errorf("err = %v, content %q", err, read(t, path))
	}
	if left := tempFiles(t, dir); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestApplyDoesNothingForUnchangedPrintAndRefusedItems(t *testing.T) {
	f := newFixture(t)
	for _, it := range []*Item{
		{Action: Unchanged, Real: filepath.Join(f.home, "a")},
		{Kind: KindPrint, Action: Add, Real: filepath.Join(f.home, "b")},
		{Err: errors.New("refused"), Action: Add, Real: filepath.Join(f.home, "c")},
	} {
		if err := Apply(it, ApplyOptions{BackupDir: f.backups}); err != nil {
			t.Error(err)
		}
	}
	if entries, _ := os.ReadDir(f.home); len(entries) != 0 {
		t.Errorf("files appeared: %v", entries)
	}
}

func TestPlanDiffShowsTheLinesChanged(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.home, ".claude", "CLAUDE.md"), "# rules\n")
	it := f.plan(t, User, false, Claude).Items[0]
	var added []string
	for _, d := range it.Diff {
		if d.Op == '+' {
			added = append(added, d.Text)
		}
	}
	if len(added) != len(blockLines())+1 || added[1] != BeginMarker || added[len(added)-1] != EndMarker {
		t.Errorf("added lines = %q", added)
	}
	var buf bytes.Buffer
	for _, d := range it.Diff {
		buf.WriteString(d.Render() + "\n")
	}
	if !strings.Contains(buf.String(), "  + "+BeginMarker) || !strings.Contains(buf.String(), "    # rules") {
		t.Errorf("rendered diff:\n%s", buf.String())
	}
}

func TestLineDiff(t *testing.T) {
	for name, tc := range map[string]struct {
		before, after string
		want          string
	}{
		"added at the end":  {"a\nb\n", "a\nb\nc\n", " b\n+c\n"},
		"removed in middle": {"a\nb\nc\nd\ne\n", "a\nb\nd\ne\n", " b\n-c\n d\n"},
		"two hunks":         {"1\n2\n3\n4\n5\n6\n7\n", "1\nx\n3\n4\n5\ny\n7\n", " 1\n-2\n+x\n 3\n@\n 5\n-6\n+y\n 7\n"},
		"identical":         {"a\n", "a\n", ""},
		"from nothing":      {"", "a\nb\n", "+a\n+b\n"},
		"to nothing":        {"a\nb\n", "", "-a\n-b\n"},
	} {
		t.Run(name, func(t *testing.T) {
			var sb strings.Builder
			for _, d := range LineDiff([]byte(tc.before), []byte(tc.after), 1) {
				if d.Op == '@' {
					sb.WriteString("@\n")
					continue
				}
				sb.WriteString(string(d.Op) + d.Text + "\n")
			}
			if sb.String() != tc.want {
				t.Errorf("diff =\n%s\nwant\n%s", sb.String(), tc.want)
			}
		})
	}
}
