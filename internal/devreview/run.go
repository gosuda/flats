package devreview

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// RecordFormat identifies the invocation record layout.
const RecordFormat = "flats-review-run/1"

// Invocation statuses.
const (
	StatusStarting      = "starting"
	StatusRunning       = "running"
	StatusExited        = "exited"
	StatusSignaled      = "signaled"
	StatusTimeout       = "timeout"
	StatusCancelled     = "cancelled"
	StatusFailedToStart = "failed-to-start"
	StatusInterrupted   = "interrupted"
)

// Outcomes of an invocation's evidence. Complete means the evidence the
// protocol needs was recorded, not that the review accepted anything.
const (
	OutcomeComplete   = "complete"
	OutcomeIncomplete = "incomplete"
	OutcomeFailed     = "failed"
)

// Review surfaces.
const (
	SurfaceCopy = "materialized-copy"
	SurfaceLive = "live-tree"
)

// RunOptions describe one reviewer invocation.
type RunOptions struct {
	Root         string   // repository work tree
	ManifestPath string   // snapshot manifest
	ConfigPath   string   // reviewer configuration
	Reviewer     string   // reviewer name in the configuration
	PromptPath   string   // prompt passed on standard input
	RunsDir      string   // the task's evidence directory
	Authors      []string // material author model IDs
	Stage        string   // design, implementation, testing or another label

	// Copy is a materialized copy to review. When empty the live working
	// tree is reviewed and WritesPrevented must state how the executor
	// prevented concurrent writes.
	Copy            string
	WritesPrevented string

	// Budget sets the evidence directory's invocation budget. Zero keeps
	// the recorded budget; changing it needs BudgetReason.
	Budget       int
	BudgetReason string
}

// Record is the evidence of one invocation, kept as run.json in its own
// directory under the evidence directory.
type Record struct {
	Format         string          `json:"format"`
	ID             string          `json:"id"`
	Stage          string          `json:"stage,omitempty"`
	Reviewer       string          `json:"reviewer"`
	Config         string          `json:"config"`
	Command        []string        `json:"command"`
	RequestedModel string          `json:"requested_model"`
	FixedBinding   string          `json:"fixed_binding,omitempty"`
	Authors        []string        `json:"authors"`
	Surface        Surface         `json:"surface"`
	Manifest       string          `json:"manifest"`
	Snapshot       string          `json:"snapshot"`
	Before         Check           `json:"before"`
	After          *Check          `json:"after,omitempty"`
	PromptSHA256   string          `json:"prompt_sha256"`
	Started        string          `json:"started,omitempty"`
	Ended          string          `json:"ended,omitempty"`
	PID            int             `json:"pid,omitempty"`
	Status         string          `json:"status"`
	ExitCode       *int            `json:"exit_code,omitempty"`
	Identity       *IdentityResult `json:"identity,omitempty"`
	Eligibility    string          `json:"eligibility,omitempty"`
	SessionReused  []string        `json:"session_reused_from,omitempty"`
	Outcome        string          `json:"outcome,omitempty"`
	Stdout         string          `json:"stdout"`
	Stderr         string          `json:"stderr"`
	Notes          []string        `json:"notes,omitempty"`
}

// Surface is what the reviewer command was given to review.
type Surface struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	// WritesPrevented is the executor's statement for a live-tree review.
	WritesPrevented string `json:"writes_prevented,omitempty"`
}

// Check is the result of checking the surface against the snapshot.
type Check struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// IdentityResult is the reviewer identity evidence.
type IdentityResult struct {
	Status         string `json:"status"` // known, unknown or invalid
	EffectiveModel string `json:"effective_model,omitempty"`
	ModelFrom      string `json:"model_from,omitempty"` // metadata or fixed-binding
	Session        string `json:"session,omitempty"`
	Source         string `json:"source,omitempty"`
}

// Budget is the evidence directory's invocation budget, kept in budget.json.
type Budget struct {
	MaxInvocations int            `json:"max_invocations"`
	History        []BudgetChange `json:"history"`
}

// BudgetChange records one budget setting.
type BudgetChange struct {
	MaxInvocations int    `json:"max_invocations"`
	Reason         string `json:"reason,omitempty"`
	At             string `json:"at"`
}

// RefusedError reports that run did not start a command.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "refused to start: " + e.Reason }

func refuse(format string, args ...any) error {
	return &RefusedError{Reason: fmt.Sprintf(format, args...)}
}

var errTimeout = errors.New("reviewer timeout")

// Run executes the configured reviewer once and returns its record. A
// non-nil error means no command ran; it is a *RefusedError when a
// protocol condition blocked the start.
func Run(ctx context.Context, o RunOptions) (*Record, error) {
	if err := runSupported(); err != nil {
		return nil, err
	}
	runs, err := OutsideRepo(o.Root, o.RunsDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(runs, 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockPath(filepath.Join(runs, ".start.lock"))
	if err != nil {
		return nil, err
	}
	defer func() { unlock() }()

	rev, cfg, err := LoadReviewer(o.Root, o.ConfigPath, o.Reviewer)
	if err != nil {
		return nil, err
	}
	authors := cleanList(o.Authors)
	if len(authors) == 0 {
		return nil, refuse("no material author model ID given; eligibility cannot be checked")
	}
	manifestPath, err := filepath.Abs(o.ManifestPath)
	if err != nil {
		return nil, err
	}
	m, err := ReadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	surface, check, err := surfaceFor(o, m)
	if err != nil {
		return nil, err
	}
	before := check()
	if !before.OK {
		return nil, refuse("the review surface does not match the snapshot: %s", before.Detail)
	}
	prompt, err := os.ReadFile(o.PromptPath)
	if err != nil {
		return nil, err
	}
	records, err := reconcile(runs)
	if err != nil {
		return nil, err
	}
	if err := spendBudget(runs, len(records), o.Budget, o.BudgetReason); err != nil {
		return nil, err
	}

	id := newID()
	dir := filepath.Join(runs, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	promptFile := filepath.Join(dir, "prompt.txt")
	if err := writeNew(promptFile, prompt); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(prompt)
	rec := &Record{
		Format: RecordFormat, ID: id, Stage: o.Stage, Reviewer: o.Reviewer, Config: cfg,
		Command: rev.Command, RequestedModel: rev.RequestedModel, FixedBinding: rev.FixedBinding,
		Authors: authors, Surface: surface, Manifest: manifestPath, Snapshot: m.Digest,
		Before: before, PromptSHA256: hex.EncodeToString(sum[:]),
		Stdout: "stdout.txt", Stderr: "stderr.txt",
	}
	identityFile := filepath.Join(dir, "identity.json")

	lock, err := os.OpenFile(filepath.Join(dir, "running.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := lockFile(lock, false); err != nil {
		return nil, err
	}
	stdin, err := os.Open(promptFile)
	if err != nil {
		return nil, err
	}
	defer stdin.Close()
	stdout, err := os.OpenFile(filepath.Join(dir, rec.Stdout), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(dir, rec.Stderr), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer stderr.Close()

	runCtx, cancel := context.WithTimeoutCause(ctx, rev.timeout(), errTimeout)
	defer cancel()
	cmd := exec.Command(rev.Command[0], rev.Command[1:]...)
	cmd.Dir = surface.Path
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.ExtraFiles = []*os.File{lock} // the process tree inherits the running lock
	cmd.Env = append(os.Environ(),
		"FLATS_REVIEW_ROOT="+surface.Path,
		"FLATS_REVIEW_MANIFEST="+manifestPath,
		"FLATS_REVIEW_PROMPT_FILE="+promptFile,
		"FLATS_REVIEW_IDENTITY_FILE="+identityFile,
		"FLATS_REVIEW_INVOCATION="+id,
	)
	setProcessGroup(cmd)
	// Publish the record before the command exists, so a crash at any later
	// point leaves a record whose lock a later start checks.
	rec.Status = StatusStarting
	if err := writeRecord(dir, rec); err != nil {
		return nil, err
	}
	rec.Started = now()
	if err := cmd.Start(); err != nil {
		rec.Status, rec.Outcome, rec.Ended = StatusFailedToStart, OutcomeFailed, now()
		rec.Notes = append(rec.Notes, err.Error())
		return rec, writeRecord(dir, rec)
	}
	rec.PID, rec.Status = cmd.Process.Pid, StatusRunning
	if err := writeRecord(dir, rec); err != nil {
		killGroup(rec.PID)
		_ = cmd.Wait()
		return nil, err
	}
	unlock()
	unlock = func() {}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		terminateGroup(rec.PID)
		select {
		case waitErr = <-done:
		case <-time.After(10 * time.Second):
			killGroup(rec.PID)
			waitErr = <-done
		}
	}
	if groupAlive(rec.PID) {
		killGroup(rec.PID)
		rec.Notes = append(rec.Notes, "processes left in the reviewer's process group were killed after it ended")
	}
	lock.Close()
	survivor, lockErr := lockHeld(filepath.Join(dir, "running.lock"))
	if lockErr != nil || survivor {
		rec.Notes = append(rec.Notes, "a process outside the reviewer's process group still holds the running lock; later starts are refused until it exits")
	}
	rec.Ended = now()
	switch {
	case errors.Is(context.Cause(runCtx), errTimeout):
		rec.Status = StatusTimeout
	case ctx.Err() != nil:
		rec.Status = StatusCancelled
	case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0:
		rec.Status = StatusExited
		code := cmd.ProcessState.ExitCode()
		rec.ExitCode = &code
	default:
		rec.Status = StatusSignaled
		if waitErr != nil {
			rec.Notes = append(rec.Notes, waitErr.Error())
		}
	}
	after := check()
	rec.After = &after
	rec.Identity = identityResult(identityFile, rev)
	rec.Eligibility = eligibility(rec.Identity, authors)
	if rec.Identity.Session != "" {
		for _, r := range records {
			if r.Identity != nil && r.Identity.Session == rec.Identity.Session {
				rec.SessionReused = append(rec.SessionReused, r.ID)
			}
		}
	}
	if m := rec.Identity.EffectiveModel; m != "" && m != rev.RequestedModel {
		rec.Notes = append(rec.Notes, fmt.Sprintf("effective model %s differs from requested %s", m, rev.RequestedModel))
	}
	rec.Outcome = outcome(rec)
	if (survivor || lockErr != nil) && rec.Outcome == OutcomeComplete {
		rec.Outcome = OutcomeIncomplete
	}
	return rec, writeRecord(dir, rec)
}

// surfaceFor resolves the review surface and returns a function that checks
// it against the snapshot.
func surfaceFor(o RunOptions, m *Manifest) (Surface, func() Check, error) {
	if o.Copy != "" {
		p, err := OutsideRepo(o.Root, o.Copy)
		if err != nil {
			return Surface{}, nil, err
		}
		return Surface{Kind: SurfaceCopy, Path: p}, func() Check {
			if err := CheckCopy(p, m); err != nil {
				return Check{Detail: err.Error()}
			}
			return Check{OK: true, Detail: "copy matches the snapshot"}
		}, nil
	}
	if strings.TrimSpace(o.WritesPrevented) == "" {
		return Surface{}, nil, refuse("a live-tree review needs a statement of how concurrent writes are prevented; review a materialized copy instead, or state it")
	}
	return Surface{Kind: SurfaceLive, Path: o.Root, WritesPrevented: o.WritesPrevented}, func() Check {
		d, err := Verify(o.Root, m)
		if err != nil {
			return Check{Detail: err.Error()}
		}
		return Check{OK: d.None(), Detail: d.String()}
	}, nil
}

func identityResult(file string, rev Reviewer) *IdentityResult {
	res := &IdentityResult{Status: "unknown"}
	id, err := readIdentity(file)
	if err != nil {
		res.Status, res.Source = "invalid", err.Error()
		return res
	}
	if id != nil {
		res.EffectiveModel, res.Session, res.Source = id.EffectiveModel, id.Session, id.Source
		if id.EffectiveModel != "" {
			res.ModelFrom = "metadata"
		}
	}
	if res.EffectiveModel == "" && rev.FixedBinding != "" {
		res.EffectiveModel, res.ModelFrom = rev.RequestedModel, "fixed-binding"
	}
	if res.EffectiveModel != "" && res.Session != "" {
		res.Status = "known"
	}
	return res
}

func eligibility(id *IdentityResult, authors []string) string {
	switch {
	case id.EffectiveModel == "":
		return "unknown"
	case slices.Contains(authors, id.EffectiveModel):
		return "ineligible"
	default:
		return "eligible"
	}
}

func outcome(r *Record) string {
	if r.Status != StatusExited || r.ExitCode == nil || *r.ExitCode != 0 {
		return OutcomeFailed
	}
	if r.Identity.Status != "known" || r.Eligibility != "eligible" || r.After == nil || !r.After.OK {
		return OutcomeIncomplete
	}
	return OutcomeComplete
}

// reconcile loads every record in runs and settles those still marked
// starting or running whose process tree is gone. It refuses while any
// invocation's lock is held, whatever its recorded status.
func reconcile(runs string) ([]*Record, error) {
	records, err := LoadRecords(runs)
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		dir := filepath.Join(runs, r.ID)
		held, err := lockHeld(filepath.Join(dir, "running.lock"))
		if err != nil {
			return nil, err
		}
		if held {
			return nil, refuse("invocation %s still holds its running lock", r.ID)
		}
		if r.Status != StatusRunning && r.Status != StatusStarting {
			continue
		}
		if r.PID > 0 && groupAlive(r.PID) {
			return nil, refuse("invocation %s released its lock but its process group %d still exists; its termination is uncertain", r.ID, r.PID)
		}
		r.Status, r.Outcome = StatusInterrupted, OutcomeFailed
		r.Notes = append(r.Notes, "reconciled "+now()+": running lock released and process group gone without a final record")
		if r.PID == 0 {
			r.Notes = append(r.Notes, "no process ID was recorded; the lock is the only evidence that no reviewer process remains")
		}
		if err := writeRecord(dir, r); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// LoadRecords reads every invocation record under runs, oldest first.
func LoadRecords(runs string) ([]*Record, error) {
	dirs, err := os.ReadDir(runs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Record
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(runs, d.Name(), "run.json"))
		if errors.Is(err, os.ErrNotExist) {
			// The record is written before the command starts, so a
			// directory without one never started a command; it still
			// counts against the budget.
			out = append(out, &Record{ID: d.Name(), Status: StatusFailedToStart, Outcome: OutcomeFailed})
			continue
		}
		if err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Name(), err)
		}
		out = append(out, &r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// spendBudget applies a budget setting and refuses when used invocations
// already reach it.
func spendBudget(runs string, used, set int, reason string) error {
	file := filepath.Join(runs, "budget.json")
	b, err := ReadBudget(runs)
	if err != nil {
		return err
	}
	switch {
	case set < 0:
		return refuse("invocation budget must be positive")
	case b == nil && set == 0:
		return refuse("no invocation budget is recorded for %s; set one", runs)
	case b == nil:
		b = &Budget{MaxInvocations: set, History: []BudgetChange{{MaxInvocations: set, Reason: reason, At: now()}}}
	case set != 0 && set != b.MaxInvocations:
		if strings.TrimSpace(reason) == "" {
			return refuse("changing the recorded invocation budget from %d to %d needs a reason", b.MaxInvocations, set)
		}
		b.MaxInvocations = set
		b.History = append(b.History, BudgetChange{MaxInvocations: set, Reason: reason, At: now()})
	}
	if err := writeAtomic(file, b); err != nil {
		return err
	}
	if used >= b.MaxInvocations {
		return refuse("invocation budget exhausted: %d of %d used", used, b.MaxInvocations)
	}
	return nil
}

// ReadBudget returns the recorded budget of an evidence directory, or nil.
func ReadBudget(runs string) (*Budget, error) {
	data, err := os.ReadFile(filepath.Join(runs, "budget.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Budget
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("budget.json: %w", err)
	}
	return &b, nil
}

func writeRecord(dir string, r *Record) error { return writeAtomic(filepath.Join(dir, "run.json"), r) }

// writeAtomic replaces file with v as JSON through a temporary file in the
// same directory.
func writeAtomic(file string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), file)
}

func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
