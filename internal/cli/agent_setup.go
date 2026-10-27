package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/gosuda/flats/internal/agentsetup"
)

// agentSetup is `flats agent-setup docs`: it makes Flats the document tool of
// the chosen agents, after a preview and a confirmation.
func (a *app) agentSetup(args []string) error {
	fs := a.flags("agent-setup")
	agentFlag := fs.String("agent", "", "`agent` to set up: claude, codex, cursor or all")
	scopeFlag := fs.String("scope", "user", "where the files live: user or project")
	dryRun := fs.Bool("dry-run", false, "print the plan and write nothing")
	yes := fs.Bool("yes", false, "apply without asking")
	undo := fs.Bool("undo", false, "reverse a setup")
	pos, err := a.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if pos[0] != "docs" {
		return usagef("unknown topic %q (use docs)", pos[0])
	}
	if a.jsonOut {
		return usagef("agent-setup prints text only; --json is not supported")
	}
	scope := agentsetup.Scope(*scopeFlag)
	if scope != agentsetup.User && scope != agentsetup.Project {
		return usagef("--scope must be user or project, not %q", *scopeFlag)
	}
	home, cwd, err := a.setupDirs()
	if err != nil {
		return err
	}
	agents, err := a.setupAgents(*agentFlag, scope, *undo, home)
	if err != nil {
		return err
	}
	plan, err := agentsetup.NewPlan(agentsetup.Options{Agents: agents, Scope: scope, Undo: *undo, Home: home, Getenv: a.env.Getenv, Cwd: cwd})
	if errors.Is(err, agentsetup.ErrProjectIsHome) {
		return fmt.Errorf("project scope: %w; run the command inside a repository or use --scope user", err)
	}
	if err != nil {
		return err
	}
	backups, err := a.backupDir()
	if err != nil {
		return err
	}
	a.printPlan(plan, agents, scope, *undo, cwd, backups)

	if refused := plan.Refused(); len(refused) > 0 {
		fmt.Fprintln(a.out, "\nNothing was written: some targets cannot be changed.")
		return exitCode(ExitError)
	}
	if !plan.Changes() {
		if *undo {
			fmt.Fprintln(a.out, "\nNothing to undo.")
		} else {
			fmt.Fprintln(a.out, "\nNothing to change.")
		}
		return nil
	}
	if *dryRun {
		fmt.Fprintln(a.out, "\nDry run: nothing was written.")
		return nil
	}
	if !*yes {
		if !a.isTerminal() {
			return usagef("no terminal to ask on, so nothing was written; rerun with --yes to apply or --dry-run to only see the plan")
		}
		ok, err := a.confirm("\nApply these changes?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.out, "Nothing changed.")
			return nil
		}
	}
	return a.applyPlan(plan, backups)
}

// setupDirs returns the home directory and the directory project scope starts
// from.
func (a *app) setupDirs() (home, cwd string, err error) {
	if home = a.env.Home; home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return "", "", err
		}
	}
	if cwd = a.env.Cwd; cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return "", "", err
		}
	}
	return home, cwd, nil
}

// setupAgents resolves --agent: all agents for an undo, the agents found in
// the home directory for a user-scope setup, and a required name for a
// project-scope setup.
func (a *app) setupAgents(name string, scope agentsetup.Scope, undo bool, home string) ([]agentsetup.Agent, error) {
	switch name {
	case "all":
		return agentsetup.AllAgents, nil
	case string(agentsetup.Claude), string(agentsetup.Codex), string(agentsetup.Cursor):
		return []agentsetup.Agent{agentsetup.Agent(name)}, nil
	case "":
	default:
		return nil, usagef("--agent must be claude, codex, cursor or all, not %q", name)
	}
	switch {
	case undo:
		return agentsetup.AllAgents, nil
	case scope == agentsetup.Project:
		return nil, usagef("--agent is required at project scope: claude, codex, cursor or all")
	}
	found := agentsetup.Detect(home, a.env.Getenv)
	if len(found) == 0 {
		return nil, usagef("no agent found in %s; name one with --agent claude|codex|cursor|all", home)
	}
	return found, nil
}

// backupDir is agent-setup-backups next to connection.json.
func (a *app) backupDir() (string, error) {
	p, err := a.connectionPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "agent-setup-backups"), nil
}

var agentLabels = map[agentsetup.Agent]string{agentsetup.Claude: "Claude Code", agentsetup.Codex: "Codex", agentsetup.Cursor: "Cursor"}

func (a *app) printPlan(plan *agentsetup.Plan, agents []agentsetup.Agent, scope agentsetup.Scope, undo bool, cwd, backups string) {
	verb := "Set up"
	if undo {
		verb = "Undo"
	}
	var names []string
	for _, ag := range agents {
		names = append(names, agentLabels[ag])
	}
	fmt.Fprintf(a.out, "%s document routing to Flats for %s at %s scope.\n", verb, strings.Join(names, ", "), scope)
	if scope == agentsetup.Project {
		fmt.Fprintf(a.out, "Project directory: %s\n", plan.Project)
	}
	for _, it := range plan.Items {
		fmt.Fprintln(a.out)
		a.printItem(it)
	}
	fmt.Fprintf(a.out, "\nBackups of changed files go to %s\n", backups)
	if notes := planNotes(plan, agents, scope, undo, cwd); len(notes) > 0 {
		fmt.Fprintln(a.out, "\nNotes:")
		for _, n := range notes {
			fmt.Fprintf(a.out, "  - %s\n", n)
		}
	}
}

func (a *app) printItem(it *agentsetup.Item) {
	switch {
	case it.Err != nil:
		fmt.Fprintf(a.out, "%s  (refused)\n  %v\n", it.Path, it.Err)
		return
	case it.Kind == agentsetup.KindPrint:
		fmt.Fprintf(a.out, "%s  (nothing is written; paste this)\n", it.Path)
		for _, l := range strings.Split(strings.TrimRight(it.Text, "\n"), "\n") {
			fmt.Fprintf(a.out, "    %s\n", l)
		}
		return
	case it.Action == agentsetup.Unchanged:
		fmt.Fprintf(a.out, "%s  (unchanged)\n", it.Path)
	default:
		fmt.Fprintf(a.out, "%s  (%s)\n", it.Path, it.Action)
	}
	if it.Link {
		fmt.Fprintf(a.out, "  symbolic link to %s\n", it.Real)
	}
	for _, n := range it.Notes {
		fmt.Fprintf(a.out, "  %s\n", n)
	}
	for _, e := range it.Present {
		fmt.Fprintf(a.out, "  already there: %s; --undo will remove it too\n", e)
	}
	for _, d := range it.Diff {
		fmt.Fprintln(a.out, d.Render())
	}
}

// planNotes are the sentences printed after the plan.
func planNotes(plan *agentsetup.Plan, agents []agentsetup.Agent, scope agentsetup.Scope, undo bool, cwd string) []string {
	var notes []string
	if scope == agentsetup.Project && plan.NoRepo {
		notes = append(notes, fmt.Sprintf("No Git repository was found above %s, so the project is the current directory.", cwd))
	}
	has := func(ag agentsetup.Agent) bool {
		for _, x := range agents {
			if x == ag {
				return true
			}
		}
		return false
	}
	if undo {
		if scope == agentsetup.User && has(agentsetup.Claude) {
			notes = append(notes, "Undo does not reconnect Claude Docs if you disconnected it at claude.ai/customize/connectors.")
		}
		if scope == agentsetup.User && has(agentsetup.Cursor) {
			notes = append(notes, "Undo does not remove text you pasted into Cursor's User Rules.")
		}
		return notes
	}
	notes = append(notes, "The agent needs Flats connected (see `flats mcp-config`). Until then it tells you so instead of using another document tool.")
	if has(agentsetup.Claude) {
		if scope == agentsetup.User {
			notes = append(notes, "The Claude desktop app does not read the connector entry. If Claude Docs is listed at claude.ai/customize/connectors, disconnect it there.")
		} else {
			notes = append(notes, "Claude Docs stays available at project scope. Use --scope user to block it.")
		}
	}
	return notes
}

func (a *app) applyPlan(plan *agentsetup.Plan, backups string) error {
	fmt.Fprintln(a.out)
	opts := agentsetup.ApplyOptions{BackupDir: backups}
	failed := 0
	for _, it := range plan.Items {
		if it.Kind == agentsetup.KindPrint || it.Action == agentsetup.Unchanged {
			continue
		}
		if err := agentsetup.Apply(it, opts); err != nil {
			failed++
			fmt.Fprintf(a.out, "failed: %s: %v\n", it.Path, err)
			continue
		}
		fmt.Fprintf(a.out, "%s: %s\n", it.Path, doneVerb(it.Action))
	}
	if failed > 0 {
		fmt.Fprintf(a.out, "\n%d of the files could not be changed. Run the command again to finish; every step can be repeated.\n", failed)
		return exitCode(ExitError)
	}
	fmt.Fprintln(a.out, "\nDone.")
	return nil
}

func doneVerb(a agentsetup.Action) string {
	switch a {
	case agentsetup.Create:
		return "created"
	case agentsetup.Delete:
		return "deleted"
	case agentsetup.Remove:
		return "removed"
	case agentsetup.Replace:
		return "replaced"
	}
	return "updated"
}

// isTerminal reports whether input comes from a terminal.
func (a *app) isTerminal() bool {
	if a.env.IsTerminal != nil {
		return a.env.IsTerminal(a.env.Stdin)
	}
	f, ok := a.env.Stdin.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// confirm asks a yes or no question. Only y or yes is a yes. The read runs
// beside the context, because Ctrl-C does not interrupt a blocked read.
func (a *app) confirm(question string) (bool, error) {
	fmt.Fprintf(a.out, "%s [y/N] ", question)
	type reply struct {
		line string
		err  error
	}
	ch := make(chan reply, 1)
	go func() {
		line, err := bufio.NewReader(a.env.Stdin).ReadString('\n')
		ch <- reply{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil && !errors.Is(r.err, io.EOF) {
			return false, r.err
		}
		fmt.Fprintln(a.out)
		answer := strings.ToLower(strings.TrimSpace(r.line))
		return answer == "y" || answer == "yes", nil
	case <-a.ctx.Done():
		fmt.Fprintln(a.out)
		return false, a.ctx.Err()
	}
}
