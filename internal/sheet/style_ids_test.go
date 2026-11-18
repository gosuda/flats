package sheet

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// styleIDs is what the style table gives out: the ids above next and the
// free runs below it.
type styleIDs struct {
	next, freeIDs uint64
	free          []styleRun
}

func (e *env) styleIDs() styleIDs {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	s := &e.in.wb.styles
	return styleIDs{s.next, s.freeIDs, slices.Clone(s.free)}
}

func sameIDs(a, b styleIDs) bool {
	return a.next == b.next && a.freeIDs == b.freeIDs && slices.Equal(a.free, b.free)
}

// sentinels are cells whose style states a test must find as they were.
type sentinels map[string]styleState

func (e *env) sentinels(addrs ...string) sentinels {
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	out := sentinels{}
	for _, addr := range addrs {
		t, row, col := e.at(addr)
		c := t.cell(row, col)
		if c == nil || c.style == 0 {
			e.t.Fatalf("%s has no style", addr)
		}
		out[addr] = e.in.wb.styles.state(c.style)
	}
	return out
}

// check fails when a sentinel cell's format or stamps changed, or the
// table holds the reserved id or two ids of one state.
func (s sentinels) check(e *env, when string) {
	e.t.Helper()
	e.in.mu.RLock()
	defer e.in.mu.RUnlock()
	st := &e.in.wb.styles
	for addr, want := range s {
		t, row, col := e.at(addr)
		if c := t.cell(row, col); c == nil || st.state(c.style) != want {
			e.t.Fatalf("%s: the style of %s changed", when, addr)
		}
	}
	if _, ok := st.byID[0]; ok || len(st.byID) != len(st.ids) {
		e.t.Fatalf("%s: the table holds %d states by id and %d by state, id 0 among them: %v", when, len(st.byID), len(st.ids), ok)
	}
	for state, id := range st.ids {
		if se := st.byID[id]; se == nil || se.styleState != state {
			e.t.Fatalf("%s: id %d is not the id of its state", when, id)
		}
	}
}

// leaveIDs makes the table, at the start of the next action, give out
// only the last n ids, with none free below them: as after billions of
// states that came and went. The hook is set, and takes itself out, on
// the writer, which also reads it at the steps of a tick.
func leaveIDs(e *env, n uint64) {
	in := e.in
	err := in.execute(context.Background(), &work{run: func() error {
		in.hook = func(step string) {
			if step != "begin" {
				return
			}
			in.hook = nil
			s := &in.wb.styles
			s.next, s.free, s.freeIDs = maxStyleID+1-n, nil, 0
		}
		return nil
	}})
	if err != nil {
		e.t.Fatal(err)
	}
}

// newFormats returns agent operations that give n cells of column col
// formats of their own, the k-th of them symbolFormat(from+k).
func newFormats(col string, n, from int) string {
	ops := make([]string, n)
	for i := range n {
		ops[i] = jsonf(`{"op":"format","tab":"Sheet1","ranges":["%s%d"],"format":%s}`, col, i+1, symbolFormat(from+i))
	}
	return strings.Join(ops, ",")
}

// Style ids are given out again after the states that held them left, so
// that formatting does not use them up; at no time does a state get id 0
// or the id of another state. Each case starts with the table's last few
// ids left and none below them free. A change that needs more is refused
// as a whole and leaves the sheet, the table and sheet.db as they were. A
// change that fits takes the last ids, and the sheet loads with them. The
// load, and the tick, find the ids that states gave back, and the changes
// after them take those. The bold and the currency cells keep their
// formats and stamps throughout.
func TestStyleIDsAreGivenOutAgain(t *testing.T) {
	setup := func(t *testing.T) (*env, sentinels) {
		e := start(t)
		e.mustAgent(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true}},{"op":"format","tab":"Sheet1","ranges":["A2"],"format":` + symbolFormat(0) + `}]}`)
		return e, e.sentinels("Sheet1!A1", "Sheet1!A2")
	}
	refused := func(t *testing.T, e *env, err error, ids styleIDs, seq uint64, before string) {
		t.Helper()
		if werr := refusal(err); werr.Code != "unavailable" || werr.Reason != "ids_exhausted" {
			t.Fatalf("the change was not refused for its style ids: %v", err)
		}
		if e.seq() != seq || e.dump() != before {
			t.Fatal("a refused change changed the sheet")
		}
		if got := e.styleIDs(); !sameIDs(got, ids) {
			t.Fatalf("a refused change left the ids at %+v, before %+v", got, ids)
		}
		e.in.mu.RLock()
		full := e.in.wb.styles.full
		e.in.mu.RUnlock()
		if full {
			t.Fatal("the table is still marked full after the revert")
		}
	}
	t.Run("an agent call that needs more ids than are left", func(t *testing.T) {
		e, kept := setup(t)
		seq, before := e.seq(), e.dump()
		leaveIDs(e, 2)
		_, err := e.agent(`{"ops":[` + newFormats("B", 3, 1) + `]}`)
		refused(t, e, err, styleIDs{next: maxStyleID - 1}, seq, before)
		kept.check(e, "after the refusal")
		e.wantStored("after the refusal")
		// The same call takes ids once the tick found the free ones.
		e.tick()
		if left := e.styleIDs(); left.freeIDs+maxStyleID+1-left.next < styleRefillLeft {
			t.Fatalf("after the tick %+v ids are left", left)
		}
		e.mustAgent(`{"ops":[` + newFormats("B", 3, 1) + `]}`)
		kept.check(e, "after the tick")
		e.wantStored("after the tick")
	})
	t.Run("a person's action that needs more ids than are left", func(t *testing.T) {
		e, kept := setup(t)
		alice := e.person("Alice")
		seq, before := e.seq(), e.dump()
		leaveIDs(e, 2)
		_, err := alice.try(e.seq(), e.formatOp("Sheet1!B1", `{"italic":true}`), e.formatOp("Sheet1!C1", `{"underline":true}`), e.formatOp("Sheet1!D1", `{"strikethrough":true}`))
		refused(t, e, err, styleIDs{next: maxStyleID - 1}, seq, before)
		kept.check(e, "after the refusal")
		e.wantStored("after the refusal")
	})
	t.Run("changes that fit take the last ids and the sheet loads with them", func(t *testing.T) {
		e, kept := setup(t)
		leaveIDs(e, 2)
		e.mustAgent(`{"ops":[` + newFormats("B", 2, 1) + `]}`)
		if got := e.styleIDs(); got.next != maxStyleID+1 || len(got.free) != 0 {
			t.Fatalf("after the last ids the table gives out %+v", got)
		}
		taken := e.sentinels("Sheet1!B1", "Sheet1!B2")
		for k, v := range taken {
			kept[k] = v
		}
		kept.check(e, "after the last ids")
		e.wantStored("after the last ids")
		// The load finds every id below the highest free.
		e.restart()
		got := e.styleIDs()
		if got.next != maxStyleID+1 || got.freeIDs != maxStyleID-4 || len(got.free) != 1 {
			t.Fatalf("after the load the table gives out %+v", got)
		}
		kept.check(e, "after the load")
		// A refused call gives back the free ids it took, in their runs.
		seq, before := e.seq(), e.dump()
		_, err := e.agent(`{"ops":[` + newFormats("C", 3, 3) + `,{"op":"delete_tab","tab":"Nowhere"}]}`)
		if err == nil {
			t.Fatal("the call was applied")
		}
		if e.seq() != seq || e.dump() != before || !sameIDs(e.styleIDs(), got) {
			t.Fatalf("the refused call left the ids at %+v, before %+v", e.styleIDs(), got)
		}
		e.mustAgent(`{"ops":[` + newFormats("C", 3, 3) + `]}`)
		kept.check(e, "after the free ids")
		e.wantStored("after the free ids")
		e.restart()
		kept.check(e, "after the second load")
		e.want("Sheet1!A1", "")
	})
}

// A load refuses a sheet.db whose styles hold the reserved id, an id past
// the last or one state under two ids that cells use: cells would get the
// default format or two ids that the table takes for one.
func TestLoadChecksTheStyleIDs(t *testing.T) {
	for name, damage := range map[string][]string{
		"the reserved id":     {`INSERT INTO styles(id,format,stamps,refs) SELECT 0,'{"italic":true}',stamps,0 FROM styles WHERE id=1`},
		"an id past the last": {`INSERT INTO styles(id,format,stamps,refs) SELECT 4294967296,'{"italic":true}',stamps,0 FROM styles WHERE id=1`},
		"a state twice": {`INSERT INTO styles(id,format,stamps,refs) SELECT 7,format,stamps,1 FROM styles WHERE id=1`,
			`INSERT INTO cells(tab,row,col,kind,num,str,style,seq,text_seq) VALUES(1,2,1,0,NULL,NULL,7,1,0)`},
	} {
		t.Run(name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"format","tab":"Sheet1","ranges":["A1"],"format":{"bold":true}}]}`)
			for _, d := range damage {
				if _, err := e.in.db.Exec(d); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadWorkbook(context.Background(), e.in.rdb); err == nil {
				t.Fatal("the load took the styles")
			}
		})
	}
}
