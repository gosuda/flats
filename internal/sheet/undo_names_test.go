package sheet

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// closeRevision closes the open edit groups and imports the revision.
func closeRevision(e *env) int {
	e.t.Helper()
	e.clock.Add(time.Minute)
	e.tick()
	return e.b.importAll(e.t)
}

func wantValidTabNames(t *testing.T, e *env) {
	t.Helper()
	seen := map[string]bool{}
	for _, name := range e.tabNames() {
		if !validTabName(name) {
			t.Errorf("tab name %q (%d characters) breaks the name rule", name, utf8.RuneCountInString(name))
		}
		if key := strings.ToLower(name); seen[key] {
			t.Errorf("tab name %q is taken twice", name)
		} else {
			seen[key] = true
		}
	}
}

// A tab that comes back under a taken name gets a valid name, whatever
// the length of the name it had, and the revision that records it loads.
// The name has the most characters a tab name may have, so the name the
// tab comes back under cannot be that name with more added.
func TestRestoredTabNameStaysValid(t *testing.T) {
	long := strings.Repeat("n", maxTabName)
	setup := func(t *testing.T) (*env, *person, TabID) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"rename_tab","tab":"Sheet1","name":"` + long + `"},{"op":"add_tab","name":"Other"},
			{"op":"write","tab":"` + long + `","at":"A1","rows":[[7,"='` + long + `'!A1*2"]]}]}`)
		return e, e.person("Alice"), e.tabID(long)
	}
	t.Run("recovery of a tab that still exists", func(t *testing.T) {
		e, alice, tab := setup(t)
		rev := e.b.importAll(t)
		for range 3 {
			if _, err := alice.recover(rev, tab); err != nil {
				t.Fatal(err)
			}
		}
		wantValidTabNames(t, e)
		names := e.tabNames()
		if len(names) != 5 {
			t.Fatalf("tabs = %v", names)
		}
		// The copy's own formula follows its new name.
		e.want("'"+names[4]+"'!B1", "14")
		n := closeRevision(e)
		if n == rev {
			t.Fatal("the recovery recorded no revision")
		}
		if out := e.readRevision(n, ReadQuery{Tab: names[4], Range: "A1"}); firstCell(out) != float64(7) {
			t.Fatalf("revision %d reads %v", n, out)
		}
		e.restart()
		wantValidTabNames(t, e)
	})
	t.Run("undo of a deletion after the name was taken", func(t *testing.T) {
		e, alice, tab := setup(t)
		bob := e.person("Bob")
		alice.do(jsonf(`{"op":"delete_tab","tab":%d}`, tab))
		bob.do(`{"op":"add_tab","name":"` + strings.ToUpper(long) + `"}`)
		if u := undo(t, alice); u.Applied != 1 {
			t.Fatalf("undo = %+v", u)
		}
		wantValidTabNames(t, e)
		e.in.mu.RLock()
		name := e.in.wb.byID[tab].name
		e.in.mu.RUnlock()
		e.want("'"+name+"'!B1", "14")
		e.wantText("'"+name+"'!B1", "='"+name+"'!A1*2")
		n := closeRevision(e)
		if out := e.readRevision(n, ReadQuery{Tab: name, Range: "A1"}); firstCell(out) != float64(7) {
			t.Fatalf("revision %d reads %v", n, out)
		}
		e.restart()
		wantValidTabNames(t, e)
	})
}
