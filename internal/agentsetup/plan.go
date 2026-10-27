package agentsetup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Agent names a coding agent this package can set up.
type Agent string

const (
	Claude Agent = "claude"
	Codex  Agent = "codex"
	Cursor Agent = "cursor"
)

// AllAgents lists the agents in the order a plan reports them.
var AllAgents = []Agent{Claude, Codex, Cursor}

// Scope is where the files live: the user's agent directories or a project.
type Scope string

const (
	User    Scope = "user"
	Project Scope = "project"
)

// codexLimit is the number of bytes of AGENTS.md that Codex reads by default.
const codexLimit = 32768

// ErrProjectIsHome is returned when the project directory is the home directory.
var ErrProjectIsHome = errors.New("the project directory is the home directory")

// Kind says what is edited in a target.
type Kind int

const (
	KindBlock    Kind = iota // a Flats block in an instruction file
	KindSettings             // Claude Code settings entries
	KindRule                 // the Cursor rule file this package owns
	KindPrint                // text for the user to paste; nothing is written
)

// Options selects what Plan looks at.
type Options struct {
	Agents []Agent
	Scope  Scope
	Undo   bool
	Home   string
	Getenv func(string) string
	Cwd    string
}

// Item is one target of a plan.
type Item struct {
	Agent  Agent
	Kind   Kind
	Path   string // the path the agent reads
	Real   string // Path with symbolic links resolved
	Link   bool   // Path is itself a symbolic link
	Exists bool
	Action Action
	Before []byte
	After  []byte
	Diff   []DiffLine
	// Entries lists settings entries added or removed; Present those a setup
	// found already in the file.
	Entries []string
	Present []string
	Text    string // KindPrint: what to paste
	Notes   []string
	Err     error // a refusal: the target cannot be changed

	mode    fs.FileMode // permission bits for a file Apply creates, or keeps
	dirMode fs.FileMode // for a directory Apply creates
}

// Plan is the computed set of changes, with nothing written.
type Plan struct {
	Items   []*Item
	Project string // the project directory, for project scope
	NoRepo  bool   // no .git was found above the current directory
}

// Refused returns the items that cannot be changed.
func (p *Plan) Refused() []*Item {
	var out []*Item
	for _, it := range p.Items {
		if it.Err != nil {
			out = append(out, it)
		}
	}
	return out
}

// Changes reports whether any item would change a file.
func (p *Plan) Changes() bool {
	for _, it := range p.Items {
		if it.Err == nil && it.Action != Unchanged {
			return true
		}
	}
	return false
}

// Dirs holds the directories of the agents at user scope.
type Dirs struct{ Claude, Codex, Cursor string }

// UserDirs resolves the agents' user directories: $CLAUDE_CONFIG_DIR or
// ~/.claude, $CODEX_HOME or ~/.codex, and ~/.cursor. A relative value is taken
// from cwd, so the plan and the backups name the same absolute paths wherever
// the command runs.
func UserDirs(home, cwd string, getenv func(string) string) Dirs {
	d := Dirs{Claude: filepath.Join(home, ".claude"), Codex: filepath.Join(home, ".codex"), Cursor: filepath.Join(home, ".cursor")}
	abs := func(v string) string {
		if filepath.IsAbs(v) {
			return filepath.Clean(v)
		}
		return filepath.Join(cwd, v)
	}
	if v := getenv("CLAUDE_CONFIG_DIR"); v != "" {
		d.Claude = abs(v)
	}
	if v := getenv("CODEX_HOME"); v != "" {
		d.Codex = abs(v)
	}
	return d
}

// Detect lists the agents whose user directory exists.
func Detect(home, cwd string, getenv func(string) string) []Agent {
	d := UserDirs(home, cwd, getenv)
	var out []Agent
	for _, c := range []struct {
		a   Agent
		dir string
	}{{Claude, d.Claude}, {Codex, d.Codex}, {Cursor, d.Cursor}} {
		if fi, err := os.Stat(c.dir); err == nil && fi.IsDir() {
			out = append(out, c.a)
		}
	}
	return out
}

// ProjectDir returns the nearest directory from start upward that holds a
// .git entry. With none it returns start and found false.
func ProjectDir(start string) (dir string, found bool) {
	for d := start; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return start, false
		}
		d = parent
	}
}

// NewPlan computes the changes for o without writing.
func NewPlan(o Options) (*Plan, error) {
	p := &Plan{}
	dirs := UserDirs(o.Home, o.Cwd, o.Getenv)
	project := ""
	if o.Scope == Project {
		var found bool
		project, found = ProjectDir(o.Cwd)
		p.Project, p.NoRepo = project, !found
		if filepath.Clean(project) == filepath.Clean(o.Home) {
			return nil, ErrProjectIsHome
		}
	}
	for _, a := range AllAgents {
		if !containsAgent(o.Agents, a) {
			continue
		}
		var items []*Item
		switch {
		case a == Claude && o.Scope == User:
			items = []*Item{
				blockItem(o, Claude, filepath.Join(dirs.Claude, "CLAUDE.md"), 0o700),
				settingsItem(o, filepath.Join(dirs.Claude, "settings.json")),
			}
		case a == Claude:
			items = []*Item{blockItem(o, Claude, filepath.Join(project, "CLAUDE.md"), 0o755)}
		case a == Codex && o.Scope == User:
			items = codexUserItems(o, dirs.Codex)
		case a == Codex:
			items = []*Item{codexItem(o, filepath.Join(project, "AGENTS.md"), 0o755)}
		case a == Cursor && o.Scope == Project:
			items = []*Item{ruleItem(o, filepath.Join(project, ".cursor", "rules", "flats-documents.mdc"))}
		case a == Cursor && !o.Undo:
			items = []*Item{{Agent: Cursor, Kind: KindPrint, Path: "Cursor Settings > Rules > User Rules", Text: RoutingText}}
		}
		for _, it := range items {
			if it != nil {
				p.Items = append(p.Items, it)
			}
		}
	}
	return p, nil
}

func containsAgent(as []Agent, a Agent) bool {
	for _, x := range as {
		if x == a {
			return true
		}
	}
	return false
}

// codexUser picks the user-level file Codex reads: AGENTS.override.md if it
// exists, else AGENTS.md. Undo looks at both.
func codexUserItems(o Options, dir string) []*Item {
	override := filepath.Join(dir, "AGENTS.override.md")
	plain := filepath.Join(dir, "AGENTS.md")
	if o.Undo {
		return []*Item{codexItem(o, override, 0o700), codexItem(o, plain, 0o700)}
	}
	if _, err := os.Lstat(override); err == nil {
		return []*Item{codexItem(o, override, 0o700)}
	}
	return []*Item{codexItem(o, plain, 0o700)}
}

func codexItem(o Options, path string, dirMode fs.FileMode) *Item {
	it := blockItem(o, Codex, path, dirMode)
	if it != nil && it.Err == nil && !o.Undo && it.Action != Unchanged && len(it.After) > codexLimit {
		it.Err = fmt.Errorf("the file would be %d bytes, more than the %d bytes Codex reads of AGENTS.md by default, so the block could be cut off", len(it.After), codexLimit)
	}
	return it
}

// target reads the state of path. For undo, a missing file returns nil: there
// is nothing to look at.
func target(agent Agent, kind Kind, path string, o Options, mode, dirMode fs.FileMode) *Item {
	it := &Item{Agent: agent, Kind: kind, Path: path, Real: path, mode: mode, dirMode: dirMode}
	li, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if o.Undo {
			return nil
		}
		return it
	}
	if err != nil {
		it.Err = err
		return it
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		it.Link = true
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			it.Err = fmt.Errorf("%s is a symbolic link that cannot be followed: %w", path, err)
			return it
		}
		it.Real = real
	}
	fi, err := os.Stat(it.Real)
	if err != nil {
		it.Err = err
		return it
	}
	if !fi.Mode().IsRegular() {
		it.Err = fmt.Errorf("%s is not a regular file", it.Real)
		return it
	}
	it.Exists, it.mode = true, fi.Mode().Perm()
	if it.Before, err = os.ReadFile(it.Real); err != nil {
		it.Err = err
	}
	return it
}

func (it *Item) finish(after []byte, action Action) {
	it.After, it.Action = after, action
	if action == Add && !it.Exists {
		it.Action = Create
	}
	if action == Replace || action == Add || action == Create || action == Remove || action == Delete {
		it.Diff = LineDiff(it.Before, after, 1)
	}
}

// block plans a Flats block in an instruction file.
func blockItem(o Options, agent Agent, path string, dirMode fs.FileMode) *Item {
	it := target(agent, KindBlock, path, o, 0o644, dirMode)
	if it == nil || it.Err != nil {
		return it
	}
	var (
		out    []byte
		action Action
		err    error
	)
	if o.Undo {
		out, action, err = DropBlock(it.Before)
	} else {
		out, action, err = SetBlock(it.Before)
	}
	if err != nil {
		it.Err = err
		return it
	}
	it.finish(out, action)
	return it
}

// settings plans the Claude Code settings entries.
func settingsItem(o Options, path string) *Item {
	it := target(Claude, KindSettings, path, o, 0o600, 0o700)
	if it == nil || it.Err != nil {
		return it
	}
	var (
		ch  SettingsChange
		err error
	)
	if o.Undo {
		ch, err = DropSettings(it.Before)
	} else {
		ch, err = SetSettings(it.Before)
	}
	if err != nil {
		it.Err = err
		return it
	}
	it.Entries, it.Present = ch.Entries, ch.Present
	it.finish(ch.Out, ch.Action)
	return it
}

// rule plans the Cursor rule file, which this package owns.
func ruleItem(o Options, path string) *Item {
	it := target(Cursor, KindRule, path, o, 0o644, 0o755)
	if it == nil || it.Err != nil {
		return it
	}
	if o.Undo {
		out, action, err := DropBlock(it.Before)
		if err != nil {
			it.Err = err
			return it
		}
		if action == Remove && OnlyFrontMatter(out) {
			if it.Link {
				it.Notes = append(it.Notes, "The rule file is a symbolic link, so only the block is removed and the file stays.")
			} else {
				it.finish(nil, Delete)
				return it
			}
		}
		it.finish(out, action)
		return it
	}
	if !it.Exists {
		it.finish(CursorRule(), Create)
		return it
	}
	has, err := HasBlock(it.Before)
	if err != nil {
		it.Err = err
		return it
	}
	if !has && !OnlyFrontMatter(it.Before) {
		it.Err = fmt.Errorf("%s exists without the Flats block and holds other content; add the block by hand or move the file", path)
		return it
	}
	out, action, err := SetBlock(it.Before)
	if err != nil {
		it.Err = err
		return it
	}
	it.finish(out, action)
	return it
}
