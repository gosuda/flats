//go:build unix

package devreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

type runFixture struct {
	root, evidence, manifest, copy, prompt, config string
}

// newRunFixture builds a repository with a candidate change, its manifest,
// a materialized copy and a prompt, all evidence outside the repository.
func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	f := &runFixture{root: repo(t, map[string]string{"a.txt": "a\n"}), evidence: outside(t)}
	write(t, f.root, map[string]string{"a.txt": "b\n"})
	m := snap(t, f.root, "", Options{})
	f.manifest = filepath.Join(f.evidence, "snap.json")
	if err := WriteJSON(f.root, f.manifest, m); err != nil {
		t.Fatal(err)
	}
	f.copy = filepath.Join(f.evidence, "copy")
	cleanupCopy(t, f.root, f.copy)
	if _, err := Materialize(f.root, m, f.copy); err != nil {
		t.Fatal(err)
	}
	f.prompt = filepath.Join(f.evidence, "prompt.txt")
	os.WriteFile(f.prompt, []byte("review this\n"), 0o600)
	return f
}

// reviewer writes a shell script and a configuration naming it.
func (f *runFixture) reviewer(t *testing.T, script string, extra map[string]any) {
	t.Helper()
	sh := filepath.Join(f.evidence, "reviewer.sh")
	if err := os.WriteFile(sh, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := map[string]any{"command": []string{sh}, "requested_model": "model-r", "timeout": "20s"}
	for k, v := range extra {
		r[k] = v
	}
	b, _ := json.Marshal(map[string]any{"reviewers": map[string]any{"r": r}})
	f.config = filepath.Join(f.evidence, "reviewers.json")
	os.Remove(f.config)
	if err := os.WriteFile(f.config, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *runFixture) opts() RunOptions {
	return RunOptions{
		Root: f.root, ManifestPath: f.manifest, ConfigPath: f.config, Reviewer: "r", PromptPath: f.prompt,
		RunsDir: filepath.Join(f.evidence, "runs"), Authors: []string{"model-a"}, Copy: f.copy, Budget: 10, Stage: "implementation",
	}
}

const identifies = `cat >/dev/null
printf '{"effective_model":"model-r","session":"s-1","source":"test metadata"}' > "$FLATS_REVIEW_IDENTITY_FILE"
echo "VERDICT: ACCEPT"`

func TestRunComplete(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.reviewer(t, `test "$FLATS_REVIEW_ROOT" = "$PWD" || exit 9
test -f "$FLATS_REVIEW_MANIFEST" || exit 8
grep -Eq '"status": "(starting|running)"' "$(dirname "$FLATS_REVIEW_IDENTITY_FILE")/run.json" || exit 6
grep -q "review this" || exit 7
echo "stderr sentinel" >&2
`+identifies, nil)
	rec, err := Run(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != OutcomeComplete || rec.Status != StatusExited || *rec.ExitCode != 0 {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Surface.Kind != SurfaceCopy || rec.Surface.Path != f.copy || !rec.Before.OK || !rec.After.OK {
		t.Errorf("surface %+v before %+v after %+v", rec.Surface, rec.Before, rec.After)
	}
	if rec.Identity.Status != "known" || rec.Identity.EffectiveModel != "model-r" || rec.Identity.Session != "s-1" || rec.Eligibility != "eligible" {
		t.Errorf("identity %+v eligibility %s", rec.Identity, rec.Eligibility)
	}
	out, _ := os.ReadFile(filepath.Join(f.opts().RunsDir, rec.ID, rec.Stdout))
	if !strings.Contains(string(out), "VERDICT: ACCEPT") {
		t.Errorf("stdout receipt = %q", out)
	}
	errOut, _ := os.ReadFile(filepath.Join(f.opts().RunsDir, rec.ID, rec.Stderr))
	if string(errOut) != "stderr sentinel\n" || strings.Contains(string(out), "sentinel") {
		t.Errorf("stderr receipt = %q, stdout %q", errOut, out)
	}
	var onDisk Record
	b, _ := os.ReadFile(filepath.Join(f.opts().RunsDir, rec.ID, "run.json"))
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatalf("run.json = %s, %v", b, err)
	}
	started, err1 := time.Parse(time.RFC3339, onDisk.Started)
	ended, err2 := time.Parse(time.RFC3339, onDisk.Ended)
	if onDisk.Outcome != OutcomeComplete || onDisk.RequestedModel != "model-r" || !slices.Equal(onDisk.Authors, []string{"model-a"}) ||
		onDisk.Stage != "implementation" || onDisk.Snapshot == "" || onDisk.PromptSHA256 == "" || onDisk.PID == 0 ||
		onDisk.Identity == nil || onDisk.Identity.EffectiveModel != "model-r" || onDisk.Eligibility != "eligible" ||
		onDisk.Surface != rec.Surface || !onDisk.Before.OK || onDisk.After == nil || !onDisk.After.OK ||
		err1 != nil || err2 != nil || ended.Before(started) || onDisk.ExitCode == nil || *onDisk.ExitCode != 0 {
		t.Errorf("persisted record = %s", b)
	}

	// A second invocation in the same session is noted.
	rec2, err := Run(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if len(rec2.SessionReused) != 1 || rec2.SessionReused[0] != rec.ID {
		t.Errorf("session reuse = %v", rec2.SessionReused)
	}
}

func TestRunIncompleteEvidence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, script string
		extra        map[string]any
		authors      []string
		identity     string
		eligibility  string
	}{
		{"no metadata", "cat >/dev/null", nil, nil, "unknown", "unknown"},
		{"fixed binding without session", "cat >/dev/null", map[string]any{"fixed_binding": "provider docs"}, nil, "unknown", "eligible"},
		{"author model", identifies, nil, []string{"model-r"}, "known", "ineligible"},
		{"bad metadata", `cat >/dev/null; echo '{"model":1}' > "$FLATS_REVIEW_IDENTITY_FILE"`, nil, nil, "invalid", "unknown"},
		// Identity claimed on standard output, as a model's answer about
		// itself would be, is not metadata.
		{"self report only", `cat >/dev/null; echo '{"effective_model":"model-r","session":"s-1"}'; echo "I am model-r"`, nil, nil, "unknown", "unknown"},
		// The requested model differs from the authors, but the effective
		// model reported by execution metadata is an author's.
		{"effective model is an author", `cat >/dev/null; printf '{"effective_model":"model-a","session":"s-2","source":"m"}' > "$FLATS_REVIEW_IDENTITY_FILE"`, nil, nil, "known", "ineligible"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRunFixture(t)
			f.reviewer(t, c.script, c.extra)
			o := f.opts()
			if c.authors != nil {
				o.Authors = c.authors
			}
			rec, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Outcome != OutcomeIncomplete || rec.Identity.Status != c.identity || rec.Eligibility != c.eligibility {
				t.Errorf("outcome %s identity %+v eligibility %s", rec.Outcome, rec.Identity, rec.Eligibility)
			}
		})
	}
	t.Run("effective model decides, not the requested one", func(t *testing.T) {
		f := newRunFixture(t)
		f.reviewer(t, `cat >/dev/null; printf '{"effective_model":"model-x","session":"s-3","source":"m"}' > "$FLATS_REVIEW_IDENTITY_FILE"`, nil)
		o := f.opts()
		o.Authors = []string{"model-r"} // the requested model is an author's
		rec, err := Run(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Eligibility != "eligible" || rec.Identity.EffectiveModel != "model-x" || rec.RequestedModel != "model-r" ||
			!strings.Contains(strings.Join(rec.Notes, " "), "differs from requested") {
			t.Errorf("eligibility %s identity %+v notes %v", rec.Eligibility, rec.Identity, rec.Notes)
		}
	})
	t.Run("fixed binding with session", func(t *testing.T) {
		f := newRunFixture(t)
		f.reviewer(t, `cat >/dev/null; printf '{"session":"s-9","source":"header"}' > "$FLATS_REVIEW_IDENTITY_FILE"`,
			map[string]any{"fixed_binding": "provider docs"})
		rec, err := Run(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if rec.Outcome != OutcomeComplete || rec.Identity.ModelFrom != "fixed-binding" || rec.Identity.EffectiveModel != "model-r" {
			t.Errorf("outcome %s identity %+v", rec.Outcome, rec.Identity)
		}
	})
	t.Run("surface changed during review", func(t *testing.T) {
		f := newRunFixture(t)
		f.reviewer(t, `cat >/dev/null; echo x > "$PWD/extra.txt"; `+identifies, nil)
		rec, err := Run(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if rec.Outcome != OutcomeIncomplete || rec.After.OK {
			t.Errorf("outcome %s after %+v", rec.Outcome, rec.After)
		}
	})
}

func TestRunFailures(t *testing.T) {
	t.Parallel()
	t.Run("nonzero exit", func(t *testing.T) {
		f := newRunFixture(t)
		f.reviewer(t, "cat >/dev/null; exit 4", nil)
		rec, err := Run(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if rec.Outcome != OutcomeFailed || *rec.ExitCode != 4 {
			t.Errorf("record %+v", rec)
		}
	})
	t.Run("timeout kills the process group", func(t *testing.T) {
		f := newRunFixture(t)
		marker := filepath.Join(f.evidence, "child-pid")
		f.reviewer(t, `cat >/dev/null; sleep 30 & echo $! > `+marker+`; wait`, map[string]any{"timeout": "4s"})
		rec, err := Run(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != StatusTimeout || rec.Outcome != OutcomeFailed {
			t.Errorf("record %+v", rec)
		}
		if groupAlive(rec.PID) {
			t.Error("the reviewer's process group survived the timeout")
		}
		assertGone(t, marker)
	})
	t.Run("cancelled", func(t *testing.T) {
		f := newRunFixture(t)
		marker := filepath.Join(f.evidence, "child-pid")
		f.reviewer(t, `cat >/dev/null; sleep 30 & echo $! > `+marker+`; wait`, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { // cancel once the reviewer's child exists
			for i := 0; i < 400; i++ {
				if b, _ := os.ReadFile(marker); len(b) > 0 {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			cancel()
		}()
		rec, err := Run(ctx, f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != StatusCancelled || rec.Outcome != OutcomeFailed {
			t.Errorf("status %s outcome %s", rec.Status, rec.Outcome)
		}
		if groupAlive(rec.PID) {
			t.Error("the reviewer's process group survived cancellation")
		}
		assertGone(t, marker)
	})
	t.Run("missing program", func(t *testing.T) {
		f := newRunFixture(t)
		f.reviewer(t, "", nil)
		cfg := map[string]any{"reviewers": map[string]any{"r": map[string]any{"command": []string{filepath.Join(f.evidence, "absent")}, "requested_model": "m"}}}
		b, _ := json.Marshal(cfg)
		os.WriteFile(f.config, b, 0o600)
		rec, err := Run(context.Background(), f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != StatusFailedToStart {
			t.Errorf("status %s", rec.Status)
		}
	})
}

// assertGone reads the PID a reviewer wrote to marker and requires that
// process to be gone (allowing a moment for it to be reaped).
func assertGone(t *testing.T, marker string) {
	t.Helper()
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("child PID marker: %v", err)
	}
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err != nil || pid <= 0 {
		t.Fatalf("child PID marker = %q", b)
	}
	for i := 0; i < 40; i++ {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("child process %d is still running", pid)
}

func refused(t *testing.T, err error, want string) {
	t.Helper()
	var r *RefusedError
	if !errors.As(err, &r) || !strings.Contains(r.Reason, want) {
		t.Errorf("err = %v, want a refusal mentioning %q", err, want)
	}
}

func TestRunRefusals(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.reviewer(t, identifies, nil)

	o := f.opts()
	o.Authors = nil
	_, err := Run(context.Background(), o)
	refused(t, err, "author")

	o = f.opts()
	o.Copy = ""
	_, err = Run(context.Background(), o)
	refused(t, err, "concurrent writes")

	o = f.opts()
	o.Budget = 0
	_, err = Run(context.Background(), o)
	refused(t, err, "no invocation budget")

	// Drift of the reviewed surface blocks the start.
	extra := filepath.Join(f.copy, "extra.txt")
	os.WriteFile(extra, []byte("x\n"), 0o644)
	_, err = Run(context.Background(), f.opts())
	refused(t, err, "does not match")
	os.Remove(extra)

	// A live-tree review with a statement runs against the working tree.
	o = f.opts()
	o.Copy, o.WritesPrevented = "", "single executor, no other writers"
	rec, err := Run(context.Background(), o)
	if err != nil || rec.Surface.Kind != SurfaceLive || rec.Surface.Path != f.root || rec.Outcome != OutcomeComplete {
		t.Fatalf("live run: %+v, %v", rec, err)
	}

	records, _ := LoadRecords(o.RunsDir)
	if len(records) != 1 {
		t.Fatalf("refused starts left invocation records: %v", records)
	}
	// The persisted record keeps the executor's statement exactly.
	want := Surface{Kind: SurfaceLive, Path: f.root, WritesPrevented: "single executor, no other writers"}
	if records[0].Surface != want || records[0].Surface != rec.Surface {
		t.Errorf("persisted surface %+v, want %+v", records[0].Surface, want)
	}
}

func TestRunConfigBoundary(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.reviewer(t, identifies, nil)
	os.Chmod(f.config, 0o666)
	_, err := Run(context.Background(), f.opts())
	if err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Errorf("err = %v", err)
	}
	os.Chmod(f.config, 0o600)

	inRepo := filepath.Join(f.root, "reviewers.json")
	b, _ := os.ReadFile(f.config)
	os.WriteFile(inRepo, b, 0o600)
	o := f.opts()
	o.ConfigPath = inRepo
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrInsideRepo) {
		t.Errorf("config inside the repository: err = %v", err)
	}
	link := filepath.Join(f.evidence, "linked.json")
	os.Symlink(inRepo, link)
	o.ConfigPath = link
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrInsideRepo) {
		t.Errorf("config linked into the repository: err = %v", err)
	}
	o = f.opts()
	o.RunsDir = filepath.Join(f.root, "runs")
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrInsideRepo) {
		t.Errorf("evidence directory inside the repository: err = %v", err)
	}
}

func TestRunBudget(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.reviewer(t, identifies, nil)
	o := f.opts()
	o.Budget = 1
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o.Budget = 0 // the recorded budget applies
	_, err := Run(context.Background(), o)
	refused(t, err, "exhausted")

	// Another reviewer name does not reset the budget.
	cfg := map[string]any{"reviewers": map[string]any{"r2": map[string]any{"command": []string{"/bin/true"}, "requested_model": "model-y"}}}
	cb, _ := json.Marshal(cfg)
	other := filepath.Join(f.evidence, "reviewers2.json")
	os.WriteFile(other, cb, 0o600)
	o2 := o
	o2.Reviewer, o2.ConfigPath = "r2", other
	_, err = Run(context.Background(), o2)
	refused(t, err, "exhausted")

	// Nor does a new snapshot.
	write(t, f.root, map[string]string{"a.txt": "c\n"})
	m := snap(t, f.root, "", Options{})
	o.ManifestPath = filepath.Join(f.evidence, "snap2.json")
	WriteJSON(f.root, o.ManifestPath, m)
	o.Copy, o.WritesPrevented = "", "single executor"
	_, err = Run(context.Background(), o)
	refused(t, err, "exhausted")

	o.Budget = 2
	_, err = Run(context.Background(), o)
	refused(t, err, "needs a reason")
	o.BudgetReason = "one more review round agreed"
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	b, _ := ReadBudget(o.RunsDir)
	if b.MaxInvocations != 2 || len(b.History) != 2 || b.History[1].Reason != o.BudgetReason {
		t.Errorf("budget = %+v", b)
	}
}

func TestRunReconcile(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.reviewer(t, identifies, nil)
	runs := filepath.Join(f.evidence, "runs")
	stale := func(id string, pid int) string {
		dir := filepath.Join(runs, id)
		os.MkdirAll(dir, 0o700)
		os.WriteFile(filepath.Join(dir, "running.lock"), nil, 0o600)
		writeRecord(dir, &Record{Format: RecordFormat, ID: id, Status: StatusRunning, PID: pid})
		return dir
	}

	// A held lock means the invocation may still run, whatever its PID.
	dir := stale("20000101T000000Z-held", 0)
	unlock, err := lockPath(filepath.Join(dir, "running.lock"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), f.opts())
	refused(t, err, "running lock")
	unlock()

	// A released lock with a live process group is uncertain.
	sleeper := exec.Command("sleep", "30")
	setProcessGroup(sleeper)
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { killGroup(sleeper.Process.Pid); sleeper.Wait() }()
	os.RemoveAll(dir)
	stale("20000101T000001Z-group", sleeper.Process.Pid)
	_, err = Run(context.Background(), f.opts())
	refused(t, err, "uncertain")
	// So is a record that says it ended while its group still exists.
	ended := stale("20000101T000002Z-ended", sleeper.Process.Pid)
	r, _ := LoadRecords(runs)
	r[len(r)-1].Status = StatusExited
	writeRecord(ended, r[len(r)-1])
	os.RemoveAll(filepath.Join(runs, "20000101T000001Z-group"))
	_, err = Run(context.Background(), f.opts())
	refused(t, err, "uncertain")
	killGroup(sleeper.Process.Pid)
	sleeper.Wait()

	// Once the group is gone, a record left running is settled.
	stale("20000101T000003Z-dead", sleeper.Process.Pid)
	rec, err := Run(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	records, _ := LoadRecords(runs)
	if records[0].Status != StatusExited || records[1].Status != StatusInterrupted || records[1].Outcome != OutcomeFailed || rec.Outcome != OutcomeComplete {
		t.Errorf("records: %+v %+v; new %+v", records[0], records[1], rec)
	}
}

// A reviewer that outlives its wrapper keeps the inherited lock, so a new
// start is refused until that process is gone.
func TestRunLockSurvivesWrapper(t *testing.T) {
	t.Parallel()
	if os.Getenv("DEVREVIEW_HELPER") == "1" {
		o := RunOptions{}
		json.Unmarshal([]byte(os.Getenv("DEVREVIEW_OPTS")), &o)
		Run(context.Background(), o)
		os.Exit(0)
	}
	f := newRunFixture(t)
	marker := filepath.Join(f.evidence, "started")
	f.reviewer(t, `cat >/dev/null; touch `+marker+`; sleep 30`, nil)
	b, _ := json.Marshal(f.opts())
	wrapper := exec.Command(os.Args[0], "-test.run=^TestRunLockSurvivesWrapper$")
	wrapper.Env = append(os.Environ(), "DEVREVIEW_HELPER=1", "DEVREVIEW_OPTS="+string(b))
	if err := wrapper.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	wrapper.Process.Kill()
	wrapper.Wait()
	records, _ := LoadRecords(f.opts().RunsDir)
	if len(records) != 1 {
		t.Fatalf("records: %v", records)
	}
	defer killGroup(records[0].PID)
	_, err := Run(context.Background(), f.opts())
	refused(t, err, fmt.Sprintf("invocation %s still holds its running lock", records[0].ID))
}

// An invocation directory whose record says it ended still blocks a start
// while anything holds its lock, and so does one left starting.
func TestRunRefusesWhileAnyLockIsHeld(t *testing.T) {
	t.Parallel()
	for _, status := range []string{StatusExited, StatusStarting, ""} {
		t.Run("status "+status, func(t *testing.T) {
			f := newRunFixture(t)
			f.reviewer(t, identifies, nil)
			dir := filepath.Join(f.evidence, "runs", "20000101T000000Z-x")
			os.MkdirAll(dir, 0o700)
			os.WriteFile(filepath.Join(dir, "running.lock"), nil, 0o600)
			if status != "" {
				writeRecord(dir, &Record{Format: RecordFormat, ID: "20000101T000000Z-x", Status: status})
			}
			unlock, err := lockPath(filepath.Join(dir, "running.lock"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = Run(context.Background(), f.opts())
			refused(t, err, "still holds its running lock")
			if _, err := Abandon(f.root, filepath.Join(f.evidence, "runs"), "20000101T000000Z-x", "checked"); err == nil {
				t.Error("abandoned an invocation whose lock is held")
			}
			unlock()
			if status == StatusStarting {
				// No process ID was recorded: termination is unknown until
				// the executor settles it.
				_, err = Run(context.Background(), f.opts())
				refused(t, err, "recorded no process ID")
				if _, err := Abandon(f.root, filepath.Join(f.evidence, "runs"), "20000101T000000Z-x", ""); err == nil {
					t.Error("abandoned without a reason")
				}
				r, err := Abandon(f.root, filepath.Join(f.evidence, "runs"), "20000101T000000Z-x", "no reviewer process found")
				if err != nil || r.Status != StatusInterrupted || r.Abandoned == nil {
					t.Fatalf("abandon: %+v, %v", r, err)
				}
			}
			if _, err := Run(context.Background(), f.opts()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A descendant that leaves the process group but keeps the inherited lock
// makes the evidence incomplete and blocks the next start.
func TestRunDetectsEscapedDescendant(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("needs perl to call setsid")
	}
	f := newRunFixture(t)
	pidFile := filepath.Join(f.evidence, "escaped")
	f.reviewer(t, `cat >/dev/null
perl -e 'use POSIX; exit if fork; POSIX::setsid(); open(F, ">", $ARGV[0]); print F $$; close F; sleep 30' `+pidFile+` &
while [ ! -s `+pidFile+` ]; do sleep 0.05; done
`+identifies, nil)
	rec, err := Run(context.Background(), f.opts())
	if b, _ := os.ReadFile(pidFile); len(b) > 0 {
		var pid int
		fmt.Sscan(string(b), &pid)
		defer killGroup(pid)
	}
	if err != nil {
		t.Fatal(err)
	}
	if rec.Outcome != OutcomeIncomplete || !strings.Contains(strings.Join(rec.Notes, " "), "still holds the running lock") {
		t.Errorf("outcome %s notes %v", rec.Outcome, rec.Notes)
	}
	_, err = Run(context.Background(), f.opts())
	refused(t, err, "still holds its running lock")
}

func TestAbandonStaysOutsideTheRepository(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	inside := filepath.Join(f.root, "runs")
	os.MkdirAll(filepath.Join(inside, "x"), 0o700)
	link := filepath.Join(f.evidence, "linked-runs")
	os.Symlink(inside, link)
	for _, runs := range []string{inside, link} {
		if _, err := Abandon(f.root, runs, "x", "checked"); !errors.Is(err, ErrInsideRepo) {
			t.Errorf("Abandon(%s) err = %v, want ErrInsideRepo", runs, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(inside, ".start.lock")); !os.IsNotExist(err) {
		t.Error("abandon created a file inside the repository")
	}
}

// A lock file planted as a symbolic link into the repository is refused
// before anything is created there.
func TestLockSymlinkIntoRepository(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.reviewer(t, identifies, nil)
	runs := filepath.Join(f.evidence, "runs")
	os.MkdirAll(filepath.Join(runs, "x"), 0o700)
	target := filepath.Join(f.root, "created-by-lock")
	os.Symlink(target, filepath.Join(runs, ".start.lock"))
	if _, err := Run(context.Background(), f.opts()); err == nil {
		t.Error("run accepted a symlinked start lock")
	}
	if _, err := Abandon(f.root, runs, "x", "checked"); err == nil {
		t.Error("abandon accepted a symlinked start lock")
	}
	os.Remove(filepath.Join(runs, ".start.lock"))
	os.Symlink(target, filepath.Join(runs, "x", "running.lock"))
	if _, err := Run(context.Background(), f.opts()); err == nil {
		t.Error("run accepted a symlinked running lock")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Error("a lock symlink created a file inside the repository")
	}
}

// Two starts in the same evidence directory at once launch one command;
// the other is refused and nothing else is recorded.
func TestRunSerializesStarts(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	launches := filepath.Join(f.evidence, "launches")
	f.reviewer(t, `cat >/dev/null; echo x >> `+launches+`; sleep 3
`+identifies, nil)
	type result struct {
		rec *Record
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			rec, err := Run(context.Background(), f.opts())
			results <- result{rec, err}
		}()
	}
	close(start)
	var ran, refusals int
	for i := 0; i < 2; i++ {
		r := <-results
		var ref *RefusedError
		switch {
		case r.err == nil && r.rec.Outcome == OutcomeComplete:
			ran++
		case errors.As(r.err, &ref) && strings.Contains(ref.Reason, "still holds its running lock"):
			refusals++
		default:
			t.Errorf("unexpected result %+v, %v", r.rec, r.err)
		}
	}
	b, _ := os.ReadFile(launches)
	records, _ := LoadRecords(f.opts().RunsDir)
	if ran != 1 || refusals != 1 || string(b) != "x\n" || len(records) != 1 {
		t.Errorf("ran %d refused %d launches %q records %d", ran, refusals, b, len(records))
	}
}
