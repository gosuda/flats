package sheet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestScratchRootIsAskedAgain covers an instance whose host could not name
// a scratch directory when the first action needed one. The action is
// refused as unavailable and nothing of it stays. Every later attempt asks
// the host again, so the same action, sent again with its number, applies
// once after the host's storage is repaired, and its undo reads the
// payload back. A directory that is removed while the instance runs is
// made again in the place the host names then. No scratch file goes to
// the system's temporary directory while the host names no place.
func TestScratchRootIsAskedAgain(t *testing.T) {
	system := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, system)
	}
	var root string
	asked := 0
	broken := errors.New("no space left on device")
	e := start(t, func(o *Options) {
		o.ScratchRoot = func() (string, error) {
			asked++
			if root == "" {
				return "", broken
			}
			return root, nil
		}
	})
	e.in.mu.Lock()
	e.in.undoPool().entryMax = -1 // every payload goes to a scratch file
	e.in.mu.Unlock()
	alice := e.person("Alice")
	base, op := e.seq(), e.setOp("A1", "text")
	alice.n++
	for attempt := 1; attempt <= 3; attempt++ {
		_, err := alice.send(alice.n, base, op)
		if werr := refusal(err); err == nil || werr.Code != "unavailable" || werr.Reason != "undo_scratch" || !werr.Retry || !errors.Is(werr.cause, broken) {
			t.Fatalf("attempt %d without a scratch directory = %v, want unavailable for the host's reason", attempt, err)
		}
		if asked != attempt {
			t.Fatalf("the host was asked %d times in %d attempts", asked, attempt)
		}
		if e.seq() != base {
			t.Fatalf("a refused action moved the sequence to %d", e.seq())
		}
		e.want("A1", "")
	}
	if stray, _ := filepath.Glob(filepath.Join(system, "flats-sheet-undo-*")); len(stray) != 0 {
		t.Fatalf("scratch files in the system's temporary directory: %v", stray)
	}

	root = t.TempDir()
	ack, err := alice.send(alice.n, base, op)
	if err != nil || status(ack) != "applied" || e.seq() != base+1 {
		t.Fatalf("the action after the repair = %s, %v at sequence %d", status(ack), err, e.seq())
	}
	e.want("A1", "text")
	first := e.in.scratch.dir
	if filepath.Dir(first) != root {
		t.Fatalf("the scratch directory is %s, want one under %s", first, root)
	}
	// Its receipt answers the same number again.
	if again, err := alice.send(alice.n, base, op); err != nil || status(again) != "applied" || e.seq() != base+1 {
		t.Fatalf("the action sent again = %s, %v at sequence %d", status(again), err, e.seq())
	}

	// The directory goes while the instance runs, and the host names
	// another place.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	root = t.TempDir()
	if ack := alice.set("B1", "more"); status(ack) != "applied" {
		t.Fatalf("an action after the directory was removed: %s", status(ack))
	}
	if second := e.in.scratch.dir; second == first || filepath.Dir(second) != root {
		t.Fatalf("the scratch directory is %s after %s was removed, want one under %s", second, first, root)
	}
	if u := undo(t, alice); u.Applied != 1 {
		t.Fatalf("undo of the action that was written after the removal = %+v", u)
	}
	e.want("B1", "")
	// The payload of the first action went with its directory: its undo is
	// skipped, and the cell keeps its text.
	if u := undo(t, alice); u.Applied != 0 || u.Reason != "lost" {
		t.Fatalf("undo of the action whose file was removed = %+v, want it skipped as lost", u)
	}
	e.want("A1", "text")
	if stray, _ := filepath.Glob(filepath.Join(system, "flats-sheet-undo-*")); len(stray) != 0 {
		t.Fatalf("scratch files in the system's temporary directory: %v", stray)
	}
}
