// Command devreview records and checks review evidence for the development
// protocol (docs/development-protocol.md, "Snapshot and evidence").
//
//	go run ./internal/devreview/cmd/devreview snapshot -base origin/main -o /evidence/snap.json
//	go run ./internal/devreview/cmd/devreview verify -manifest /evidence/snap.json
//	go run ./internal/devreview/cmd/devreview materialize -manifest /evidence/snap.json -o /evidence/copy
//	go run ./internal/devreview/cmd/devreview run -manifest ... -reviewer NAME -prompt FILE -runs DIR -author MODEL -copy DIR
//	go run ./internal/devreview/cmd/devreview abandon -runs DIR -id ID -reason "how you checked"
//	go run ./internal/devreview/cmd/devreview report -manifest ... -runs DIR -o /evidence/report.md
//
// Evidence paths must be outside the repository. See the package README.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gosuda/flats/internal/devreview"
)

// Exit codes.
const (
	exitOK         = 0
	exitFailed     = 1 // drift; the reviewer command failed, timed out or was cancelled; or its record could not be saved
	exitUsage      = 2 // usage error, refusal or other error before a command ran
	exitIncomplete = 3 // the command exited 0 but the evidence is incomplete
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	cmds := map[string]func([]string, io.Writer, io.Writer) int{
		"snapshot": snapshot, "verify": verify, "materialize": materialize, "run": runReviewer, "abandon": abandon, "report": report,
	}
	f, ok := cmds[args[0]]
	if !ok {
		usage(stderr)
		return exitUsage
	}
	return f(args[1:], stdout, stderr)
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage: devreview <command> [flags]

  snapshot     record the candidate in a manifest
  verify       report drift between the working tree (or a copy) and a manifest
  materialize  write a read-only copy of the candidate
  run          run a configured reviewer command and record the invocation
  abandon      settle an invocation whose termination could not be established
  report       write a task report skeleton from the recorded evidence

Run "devreview <command> -h" for its flags.`)
}

func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("devreview "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("C", ".", "a directory inside the repository")
	return fs, dir
}

func root(dir string, stderr io.Writer) (string, bool) {
	r, err := devreview.RepoRoot(dir)
	if err != nil {
		fmt.Fprintln(stderr, "devreview:", err)
		return "", false
	}
	return r, true
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "devreview:", err)
	return exitUsage
}

func snapshot(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlags("snapshot", stderr)
	base := fs.String("base", "", "base to compare against: the merge base of this commit and HEAD (default HEAD)")
	out := fs.String("o", "", "manifest file to create, outside the repository (required)")
	var include, exclude list
	fs.Var(&include, "include", "ignored or unchanged input that belongs to the candidate, relative to the repository root (repeatable)")
	fs.Var(&exclude, "exclude", "pattern for paths that are not candidate content, such as reports and logs (repeatable)")
	if fs.Parse(args) != nil || *out == "" || fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}
	r, ok := root(*dir, stderr)
	if !ok {
		return exitUsage
	}
	m, err := devreview.Snapshot(r, *base, devreview.Options{Include: include, Exclude: exclude})
	if err != nil {
		return fail(stderr, err)
	}
	if err := devreview.WriteJSON(r, *out, m); err != nil {
		return fail(stderr, err)
	}
	changed := 0
	for _, e := range m.Files {
		if e.Changed() {
			changed++
		}
	}
	fmt.Fprintf(stdout, "snapshot %s\nHEAD %s, merge base %s\n%d changed path(s) of %d bound, %d excluded\n", m.Digest, m.Head.Commit, m.Base.MergeBase, changed, len(m.Files), len(m.Excluded))
	return exitOK
}

func verify(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlags("verify", stderr)
	manifest := fs.String("manifest", "", "manifest file (required)")
	copyDir := fs.String("copy", "", "check this materialized copy instead of the working tree")
	if fs.Parse(args) != nil || *manifest == "" || fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}
	m, err := devreview.ReadManifest(*manifest)
	if err != nil {
		return fail(stderr, err)
	}
	if *copyDir != "" {
		if err := devreview.CheckCopy(*copyDir, m); err != nil {
			fmt.Fprintln(stdout, "DRIFT", err)
			return exitFailed
		}
		fmt.Fprintln(stdout, "OK copy matches snapshot", m.Digest)
		return exitOK
	}
	r, ok := root(*dir, stderr)
	if !ok {
		return exitUsage
	}
	d, err := devreview.Verify(r, m)
	if err != nil {
		return fail(stderr, err)
	}
	if !d.None() {
		fmt.Fprintln(stdout, "DRIFT", d)
		return exitFailed
	}
	fmt.Fprintln(stdout, "OK no drift from snapshot", m.Digest)
	return exitOK
}

func materialize(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlags("materialize", stderr)
	manifest := fs.String("manifest", "", "manifest file (required)")
	out := fs.String("o", "", "directory to create, outside the repository (required)")
	if fs.Parse(args) != nil || *manifest == "" || *out == "" || fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}
	r, ok := root(*dir, stderr)
	if !ok {
		return exitUsage
	}
	m, err := devreview.ReadManifest(*manifest)
	if err != nil {
		return fail(stderr, err)
	}
	p, err := devreview.Materialize(r, m, *out)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "materialized %s at %s\nremove it with: chmod -R u+w %s && git worktree remove --force %s\n", m.Digest, p, p, p)
	return exitOK
}

func runReviewer(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlags("run", stderr)
	var o devreview.RunOptions
	var authors list
	fs.StringVar(&o.ManifestPath, "manifest", "", "manifest file (required)")
	fs.StringVar(&o.Reviewer, "reviewer", "", "reviewer name in the configuration (required)")
	fs.StringVar(&o.PromptPath, "prompt", "", "prompt file passed on standard input (required)")
	fs.StringVar(&o.RunsDir, "runs", "", "the task's evidence directory, outside the repository (required)")
	fs.Var(&authors, "author", "material author model ID (repeatable, at least one)")
	fs.StringVar(&o.Copy, "copy", "", "materialized copy to review")
	fs.StringVar(&o.WritesPrevented, "writes-prevented", "", "for a live-tree review: how concurrent writes are prevented")
	fs.StringVar(&o.Stage, "stage", "", "stage label recorded with the invocation")
	fs.StringVar(&o.ConfigPath, "config", "", "reviewer configuration (default $"+devreview.ConfigEnv+", then the user config directory)")
	fs.IntVar(&o.Budget, "budget", 0, "invocation budget of the evidence directory (required the first time)")
	fs.StringVar(&o.BudgetReason, "budget-reason", "", "reason for changing a recorded budget")
	if fs.Parse(args) != nil || o.ManifestPath == "" || o.Reviewer == "" || o.PromptPath == "" || o.RunsDir == "" || fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}
	o.Authors = authors
	if o.ConfigPath == "" {
		o.ConfigPath = os.Getenv(devreview.ConfigEnv)
	}
	if o.ConfigPath == "" {
		p, err := devreview.DefaultConfigPath()
		if err != nil {
			return fail(stderr, err)
		}
		o.ConfigPath = p
	}
	r, ok := root(*dir, stderr)
	if !ok {
		return exitUsage
	}
	o.Root = r
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rec, err := devreview.Run(ctx, o)
	if err != nil {
		var refused *devreview.RefusedError
		if errors.As(err, &refused) {
			fmt.Fprintln(stderr, "devreview:", refused)
			return exitUsage
		}
		if rec == nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stderr, "devreview: invocation %s ran, but saving its record failed: %v\n", rec.ID, err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "invocation %s: %s, outcome %s\n", rec.ID, rec.Status, rec.Outcome)
	if rec.Identity != nil {
		fmt.Fprintf(stdout, "identity %s, eligibility %s\n", rec.Identity.Status, rec.Eligibility)
	}
	if rec.After != nil {
		fmt.Fprintf(stdout, "surface after review: %s\n", rec.After.Detail)
	}
	switch rec.Outcome {
	case devreview.OutcomeComplete:
		return exitOK
	case devreview.OutcomeIncomplete:
		return exitIncomplete
	default:
		return exitFailed
	}
}

func abandon(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlags("abandon", stderr)
	runs := fs.String("runs", "", "the task's evidence directory (required)")
	id := fs.String("id", "", "invocation ID (required)")
	reason := fs.String("reason", "", "how you established that its command no longer runs (required)")
	if fs.Parse(args) != nil || *runs == "" || *id == "" || fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}
	r, ok := root(*dir, stderr)
	if !ok {
		return exitUsage
	}
	rec, err := devreview.Abandon(r, *runs, *id, *reason)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "invocation %s settled as %s, outcome %s\n", rec.ID, rec.Status, rec.Outcome)
	return exitOK
}

func report(args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlags("report", stderr)
	var o devreview.ReportOptions
	var authors list
	fs.StringVar(&o.ManifestPath, "manifest", "", "manifest file (required)")
	fs.StringVar(&o.RunsDir, "runs", "", "the task's evidence directory")
	fs.StringVar(&o.Output, "o", "", "report file to create, outside the repository (required)")
	fs.StringVar(&o.Stage, "stage", "", "stage label")
	fs.StringVar(&o.Title, "title", "", "report title")
	fs.Var(&authors, "author", "material author model ID (repeatable; default: the authors recorded with the invocations)")
	if fs.Parse(args) != nil || o.ManifestPath == "" || o.Output == "" || fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}
	r, ok := root(*dir, stderr)
	if !ok {
		return exitUsage
	}
	o.Root, o.Authors = r, authors
	p, err := devreview.WriteReport(o)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "wrote", p)
	return exitOK
}
