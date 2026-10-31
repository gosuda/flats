package notebook

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

type undoTestStore struct {
	t   *testing.T
	s   *store
	seq Sequence
	now time.Time
}

func newUndoTestStore(t *testing.T) *undoTestStore {
	t.Helper()
	s, _, err := openStore(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	f := &undoTestStore{t: t, s: s, seq: 1, now: time.Unix(1000, 0)}
	t.Cleanup(func() {
		if err := f.s.close(); err != nil {
			t.Error(err)
		}
	})
	err = s.transaction(t.Context(), func(tx *sql.Tx) error {
		for _, session := range []string{"a", "b", "c"} {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO sessions(token_hash,editor,label,verified,max_n,last_at) VALUES(?,?,?,0,'0',0)`, session, session, session); err != nil {
				return err
			}
		}
		for i := byte(1); i <= 2; i++ {
			item := Item{Ref: Ref{ID: vocabID(PageKind, i), Inc: 1}, Ver: 1, V: map[GroupKey]Version{}, Data: Page{Title: fmt.Sprintf("page%d", i), Position: fmt.Sprint(i)}}
			if err := putItem(t.Context(), tx, item); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *undoTestStore) record(id ID) itemRecord {
	f.t.Helper()
	var r itemRecord
	err := f.s.transaction(f.t.Context(), func(tx *sql.Tx) error { var err error; r, err = loadRecord(f.t.Context(), tx, id); return err })
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *undoTestStore) change(session string, opts editOptions, fn func(*edit) error) {
	f.t.Helper()
	f.seq++
	f.now = f.now.Add(100 * time.Millisecond)
	opts.Session, opts.Seq, opts.Now, opts.Browser = session, f.seq, f.now, true
	err := f.s.transaction(f.t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: f.t.Context(), tx: tx, seq: f.seq, session: session, browser: true, changes: map[ID]itemChange{}, refs: map[string]Ref{}}
		if err := fn(e); err != nil {
			return err
		}
		return recordUndo(e, opts)
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *undoTestStore) title(session string, id ID, title string) {
	f.t.Helper()
	f.change(session, editOptions{}, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, id)
		if err != nil {
			return err
		}
		data := r.Item.Data.(Page)
		data.Title = title
		r.Item.Data = data
		return e.write(r, TitleGroup)
	})
}

func (f *undoTestStore) block(session string, n byte, parent *ID, kind ContentKind) Ref {
	f.t.Helper()
	id := vocabID(BlockKind, n)
	f.change(session, editOptions{}, func(e *edit) error {
		return e.write(itemRecord{Item: Item{Ref: Ref{ID: id, Inc: Incarnation(e.seq)}, Ver: Version(e.seq), V: map[GroupKey]Version{}, Data: Block{Page: vocabID(PageKind, 1), Parent: parent, Position: fmt.Sprint(n), Content: Content{Kind: kind, Text: RichText{}}}}, Alive: true})
	})
	return f.record(id).Item.Ref
}

func (f *undoTestStore) text(session string, ref Ref, text string, opts editOptions) {
	f.t.Helper()
	f.change(session, opts, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, ref.ID)
		if err != nil {
			return err
		}
		data := r.Item.Data.(Block)
		data.Content.Text = RichText{{Text: text, Marks: []Mark{}}}
		r.Item.Data = data
		return e.write(r, ContentGroup)
	})
}

func (f *undoTestStore) undo(session string, redo bool) undoSummary {
	f.t.Helper()
	f.seq++
	var summary undoSummary
	err := f.s.transaction(f.t.Context(), func(tx *sql.Tx) error {
		var err error
		summary, err = undoEdit(f.t.Context(), tx, editOptions{Session: session, Seq: f.seq, Now: f.now}, redo)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return summary
}

func (f *undoTestStore) count(session, stack string) int {
	f.t.Helper()
	var count int
	if err := f.s.db.QueryRowContext(f.t.Context(), `SELECT count(*) FROM undo WHERE session=? AND stack=?`, session, stack).Scan(&count); err != nil {
		f.t.Fatal(err)
	}
	return count
}

func TestUndoDifferentGroupsAndEqualWrite(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := vocabID(PageKind, 1)
	f.title("a", page, "mine")
	f.change("b", editOptions{}, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, page)
		if err != nil {
			return err
		}
		p := r.Item.Data.(Page)
		parent := vocabID(PageKind, 2)
		p.Parent = &parent
		r.Item.Data = p
		return e.write(r, PlaceGroup)
	})
	if got := f.undo("a", false); got.Applied != 1 || got.Skipped != 0 {
		t.Fatalf("undo=%+v", got)
	}
	p := f.record(page).Item.Data.(Page)
	if p.Title != "page1" || p.Parent == nil || *p.Parent != vocabID(PageKind, 2) {
		t.Fatalf("page=%+v", p)
	}
	f.title("a", page, "same")
	f.title("b", page, "same")
	got := f.undo("a", false)
	if got.LaterEdits != 1 || got.Applied != 0 || len(got.Notices) == 0 || got.Notices[0] != "Changes other people made later were not undone" {
		t.Fatalf("undo=%+v", got)
	}
	if f.record(page).Item.Data.(Page).Title != "same" {
		t.Fatal("equal remote write overwritten")
	}
}

func TestUndoSessionLineage(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprintf("remote=%t", remote), func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			page := vocabID(PageKind, 1)
			f.title("a", page, "A")
			if remote {
				f.title("b", page, "B")
				f.title("a", page, "A")
			} else {
				f.title("a", page, "C")
			}
			if got := f.undo("a", false); got.Applied != 1 {
				t.Fatalf("first=%+v", got)
			}
			got := f.undo("a", false)
			want := "page1"
			if remote {
				want = "B"
				if got.LaterEdits != 1 || got.Applied != 0 {
					t.Fatalf("second=%+v", got)
				}
			} else if got.Applied != 1 {
				t.Fatalf("second=%+v", got)
			}
			if title := f.record(page).Item.Data.(Page).Title; title != want {
				t.Fatalf("title=%q want %q", title, want)
			}
		})
	}
}

func TestUndoRedoOnlyAppliedGroups(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := vocabID(PageKind, 1)
	f.change("a", editOptions{}, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, page)
		if err != nil {
			return err
		}
		p := r.Item.Data.(Page)
		p.Title = "mine"
		parent := vocabID(PageKind, 2)
		p.Parent = &parent
		r.Item.Data = p
		return e.write(r, TitleGroup, PlaceGroup)
	})
	f.title("b", page, "theirs")
	got := f.undo("a", false)
	if got.Applied != 0 || got.LaterEdits != 1 {
		t.Fatalf("structural dependency was partially undone: %+v", got)
	}
	if f.count("a", "redo") != 0 {
		t.Fatal("skipped inverse entered redo")
	}
	ref := f.block("b", 1, nil, Paragraph)
	f.text("a", ref, "mine", editOptions{})
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("undo=%+v", got)
	}
	if got := f.undo("a", true); got.Applied != 1 {
		t.Fatalf("redo=%+v", got)
	}
	if got := f.record(ref.ID).Item.Data.(Block).Content.Text[0].Text; got != "mine" {
		t.Fatal(got)
	}
	f.undo("a", false)
	f.text("a", ref, "new", editOptions{})
	if got := f.undo("a", true); !got.Empty {
		t.Fatalf("new edit did not clear redo: %+v", got)
	}
}

func TestUndoCreationOutsideChildAndLaterEdit(t *testing.T) {
	for _, outside := range []bool{false, true} {
		t.Run(fmt.Sprintf("outside=%t", outside), func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			root := f.block("a", 1, nil, Toggle)
			if outside {
				f.block("b", 2, &root.ID, Paragraph)
			} else {
				f.text("b", root, "later", editOptions{})
			}
			if got := f.undo("a", false); got.LaterEdits != 1 || got.Applied != 0 {
				t.Fatalf("undo=%+v", got)
			}
			if !f.record(root.ID).Alive {
				t.Fatal("creation removed despite later changes")
			}
		})
	}
}

func TestUndoDeletionFreshIncarnationAndOwnStacks(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	ref := f.block("b", 1, nil, Paragraph)
	f.text("a", ref, "one", editOptions{})
	f.text("a", ref, "two", editOptions{})
	f.change("a", editOptions{}, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, ref.ID)
		if err != nil {
			return err
		}
		r.Alive = false
		r.DeletedAt = e.seq
		return e.write(r)
	})
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("revive=%+v", got)
	}
	revived := f.record(ref.ID)
	if !revived.Alive || revived.Item.Ref.ID != ref.ID || revived.Item.Ref.Inc == ref.Inc {
		t.Fatalf("revived=%+v", revived)
	}
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("own stack after revive=%+v", got)
	}
	if got := f.record(ref.ID).Item.Data.(Block).Content.Text[0].Text; got != "one" {
		t.Fatal(got)
	}
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("older own stack after revive=%+v", got)
	}
}

func TestUndoCreationRoundTrip(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	ref := f.block("a", 1, nil, Paragraph)
	if got := f.undo("a", false); got.Applied != 1 || f.record(ref.ID).Alive {
		t.Fatalf("undo=%+v", got)
	}
	if got := f.undo("a", true); got.Applied != 1 {
		t.Fatalf("redo=%+v", got)
	}
	revived := f.record(ref.ID)
	if !revived.Alive || revived.Item.Ref.ID != ref.ID || revived.Item.Ref.Inc == ref.Inc {
		t.Fatalf("revived=%+v", revived)
	}
}

func TestUndoRevivalDoesNotRelinkRemoteHistory(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	ref := f.block("c", 1, nil, Paragraph)
	f.text("a", ref, "A", editOptions{})
	f.text("b", ref, "B", editOptions{})
	f.change("a", editOptions{}, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, ref.ID)
		if err != nil {
			return err
		}
		r.Alive, r.DeletedAt = false, e.seq
		return e.write(r)
	})
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("revive=%+v", got)
	}
	if got := f.undo("a", false); got.LaterEdits != 1 || got.Applied != 0 {
		t.Fatalf("old A erased B: %+v", got)
	}
	if got := f.undo("b", false); got.Missing != 1 || got.Applied != 0 {
		t.Fatalf("remote history relinked: %+v", got)
	}
	if got := f.record(ref.ID).Item.Data.(Block).Content.Text[0].Text; got != "B" {
		t.Fatal(got)
	}
}

func TestUndoKindChangeMovementDependency(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	parent := f.block("b", 1, nil, Toggle)
	child := f.block("b", 2, &parent.ID, Paragraph)
	f.change("a", editOptions{}, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, parent.ID)
		if err != nil {
			return err
		}
		b := r.Item.Data.(Block)
		b.Content.Kind = Paragraph
		r.Item.Data = b
		if err := e.write(r, ContentGroup); err != nil {
			return err
		}
		r, err = loadRecord(e.ctx, e.tx, child.ID)
		if err != nil {
			return err
		}
		b = r.Item.Data.(Block)
		b.Parent = nil
		r.Item.Data = b
		return e.write(r, PlaceGroup)
	})
	f.change("b", editOptions{}, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, child.ID)
		if err != nil {
			return err
		}
		b := r.Item.Data.(Block)
		b.Page = vocabID(PageKind, 2)
		r.Item.Data = b
		return e.write(r, PlaceGroup)
	})
	if got := f.undo("a", false); got.LaterEdits != 1 || got.Applied != 0 {
		t.Fatalf("partial kind inverse=%+v", got)
	}
	if kind := f.record(parent.ID).Item.Data.(Block).Content.Kind; kind != Paragraph {
		t.Fatalf("kind=%s", kind)
	}
}

func TestUndoTypingIdleAndExplicitClosure(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			ref := f.block("b", 1, nil, Paragraph)
			opts := editOptions{ActionID: "run", TypingBlock: &ref}
			f.text("a", ref, "a", opts)
			if explicit {
				if err := f.s.transaction(t.Context(), func(tx *sql.Tx) error { return closeUndoAction(t.Context(), tx, "a", "run") }); err != nil {
					t.Fatal(err)
				}
			} else {
				f.now = f.now.Add(time.Second)
			}
			f.text("a", ref, "ab", opts)
			f.text("a", ref, "abc", opts)
			if n := f.count("a", "undo"); n != 3 {
				t.Fatalf("closed run reopened: %d", n)
			}
		})
	}
}

func TestUndoCurrentStructureCycleAndLastPage(t *testing.T) {
	for _, lastPage := range []bool{false, true} {
		t.Run(fmt.Sprintf("last_page=%t", lastPage), func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			one, two := vocabID(PageKind, 1), vocabID(PageKind, 2)
			move := func(session string, id ID, parent *ID) {
				f.change(session, editOptions{}, func(e *edit) error {
					r, err := loadRecord(e.ctx, e.tx, id)
					if err != nil {
						return err
					}
					p := r.Item.Data.(Page)
					p.Parent = parent
					r.Item.Data = p
					return e.write(r, PlaceGroup)
				})
			}
			if lastPage {
				three := vocabID(PageKind, 3)
				f.change("a", editOptions{}, func(e *edit) error {
					return e.write(itemRecord{Alive: true, Item: Item{Ref: Ref{ID: three, Inc: Incarnation(e.seq)}, Ver: Version(e.seq), V: map[GroupKey]Version{}, Data: Page{Position: "3"}}})
				})
				f.change("b", editOptions{}, func(e *edit) error {
					for _, id := range []ID{one, two} {
						r, err := loadRecord(e.ctx, e.tx, id)
						if err != nil {
							return err
						}
						r.Alive, r.DeletedAt = false, e.seq
						if err := e.write(r); err != nil {
							return err
						}
					}
					return nil
				})
				if got := f.undo("a", false); got.Structure != 1 || got.Applied != 0 || !f.record(three).Alive {
					t.Fatalf("last page removed: %+v", got)
				}
			} else {
				move("b", one, &two)
				move("a", one, nil)
				move("b", two, &one)
				if got := f.undo("a", false); got.Structure != 1 || got.Applied != 0 {
					t.Fatalf("cycle inverse=%+v", got)
				}
				if parent := f.record(one).Item.Data.(Page).Parent; parent != nil {
					t.Fatal("invalid placement was not rolled back")
				}
			}
		})
	}
}

func TestUndoDeletionPageParentFallback(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	one, child := vocabID(PageKind, 1), vocabID(PageKind, 3)
	f.change("b", editOptions{}, func(e *edit) error {
		return e.write(itemRecord{Alive: true, Item: Item{Ref: Ref{ID: child, Inc: Incarnation(e.seq)}, Ver: Version(e.seq), V: map[GroupKey]Version{}, Data: Page{Title: "child", Parent: &one, Position: "3"}}})
	})
	f.change("a", editOptions{}, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, child)
		if err != nil {
			return err
		}
		r.Alive, r.DeletedAt = false, e.seq
		return e.write(r)
	})
	f.change("b", editOptions{}, func(e *edit) error {
		r, err := loadRecord(e.ctx, e.tx, one)
		if err != nil {
			return err
		}
		r.Alive, r.DeletedAt = false, e.seq
		return e.write(r)
	})
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("undo=%+v", got)
	}
	if r := f.record(child); !r.Alive || r.Item.Data.(Page).Parent != nil {
		t.Fatalf("fallback=%+v", r)
	}
}

func TestUndoSQLFailureRollsBackEdit(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	page := vocabID(PageKind, 1)
	if _, err := f.s.db.ExecContext(t.Context(), `DROP TABLE undo`); err != nil {
		t.Fatal(err)
	}
	err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: t.Context(), tx: tx, seq: 2, session: "a", changes: map[ID]itemChange{}}
		r, err := loadRecord(e.ctx, tx, page)
		if err != nil {
			return err
		}
		p := r.Item.Data.(Page)
		p.Title = "not committed"
		r.Item.Data = p
		if err := e.write(r, TitleGroup); err != nil {
			return err
		}
		return recordUndo(e, editOptions{Session: "a", Seq: 2})
	})
	if err == nil {
		t.Fatal("missing undo table was ignored")
	}
	if title := f.record(page).Item.Data.(Page).Title; title != "page1" {
		t.Fatalf("failed action committed: %q", title)
	}
}

func TestUndoDeletionFallbackAndMissingPage(t *testing.T) {
	for _, missingPage := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_page=%t", missingPage), func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			parent := f.block("b", 1, nil, Toggle)
			child := f.block("b", 2, &parent.ID, Paragraph)
			f.change("a", editOptions{}, func(e *edit) error {
				r, err := loadRecord(e.ctx, e.tx, child.ID)
				if err != nil {
					return err
				}
				r.Alive = false
				r.DeletedAt = e.seq
				return e.write(r)
			})
			f.change("b", editOptions{}, func(e *edit) error {
				ids := []ID{parent.ID}
				if missingPage {
					ids = append(ids, vocabID(PageKind, 1))
				}
				for _, id := range ids {
					r, err := loadRecord(e.ctx, e.tx, id)
					if err != nil {
						return err
					}
					r.Alive = false
					r.DeletedAt = e.seq
					if err := e.write(r); err != nil {
						return err
					}
				}
				return nil
			})
			got := f.undo("a", false)
			r := f.record(child.ID)
			if missingPage {
				if got.Structure != 1 || r.Alive {
					t.Fatalf("invented page: %+v %+v", got, r)
				}
			} else if got.Applied != 1 || !r.Alive || r.Item.Data.(Block).Parent != nil {
				t.Fatalf("fallback failed: %+v %+v", got, r)
			}
		})
	}
}

func TestUndoTypingCoalescingClosureAndDurability(t *testing.T) {
	t.Parallel()
	f := newUndoTestStore(t)
	ref := f.block("b", 1, nil, Paragraph)
	opts := editOptions{ActionID: "typing", TypingBlock: &ref}
	f.text("a", ref, "a", opts)
	f.text("a", ref, "ab", opts)
	if n := f.count("a", "undo"); n != 1 {
		t.Fatalf("typing entries=%d", n)
	}
	f.text("b", ref, "remote", editOptions{})
	f.text("a", ref, "abc", opts)
	f.text("a", ref, "abcd", opts)
	if n := f.count("a", "undo"); n != 3 {
		t.Fatalf("closed action reopened: entries=%d", n)
	}
	path := f.s.path
	if err := f.s.close(); err != nil {
		t.Fatal(err)
	}
	s, _, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	if got := f.undo("a", false); got.Applied != 1 {
		t.Fatalf("reopened undo=%+v", got)
	}
	if got := f.record(ref.ID).Item.Data.(Block).Content.Text[0].Text; got != "abc" {
		t.Fatal(got)
	}
}

func TestUndoEntryLimitAndBudget(t *testing.T) {
	for _, budget := range []int64{0, 1} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			page := vocabID(PageKind, 1)
			for i := 0; i < MaxUndoEntries+2; i++ {
				f.change("a", editOptions{UndoBudget: budget}, func(e *edit) error {
					r, err := loadRecord(e.ctx, e.tx, page)
					if err != nil {
						return err
					}
					p := r.Item.Data.(Page)
					p.Title = fmt.Sprint(e.seq)
					r.Item.Data = p
					return e.write(r, TitleGroup)
				})
			}
			n := f.count("a", "undo")
			if n > MaxUndoEntries || (budget == 1 && n != 0) {
				t.Fatalf("entries=%d", n)
			}
			if budget == 1 {
				got := f.undo("a", false)
				if !got.Empty || !got.Evicted {
					t.Fatalf("eviction notice=%+v", got)
				}
			}
		})
	}
}

func TestUndoGuardSnapshotAcrossIndependentGroups(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprintf("remote=%t", remote), func(t *testing.T) {
			t.Parallel()
			f := newUndoTestStore(t)
			page := vocabID(PageKind, 1)
			f.change("a", editOptions{}, func(e *edit) error {
				r, err := loadRecord(e.ctx, e.tx, page)
				if err != nil {
					return err
				}
				p := r.Item.Data.(Page)
				p.Title = "one"
				parent := vocabID(PageKind, 2)
				p.Parent = &parent
				r.Item.Data = p
				return e.write(r, TitleGroup, PlaceGroup)
			})
			err := f.s.transaction(t.Context(), func(tx *sql.Tx) error {
				rows, err := readUndoRows(t.Context(), tx, "a")
				if err != nil {
					return err
				}
				row := rows[0]
				part := row.Entry.Components[0].Parts[0]
				title, place := part, part
				title.Groups, place.Groups = []GroupKey{TitleGroup}, []GroupKey{PlaceGroup}
				row.Entry.Components = []undoComponent{{Parts: []undoPart{title}}, {Parts: []undoPart{place}}}
				return saveUndoEntry(t.Context(), tx, "a", "undo", row.Order, row.Entry)
			})
			if err != nil {
				t.Fatal(err)
			}
			if remote {
				f.title("b", page, "remote")
			}
			got := f.undo("a", false)
			if (!remote && got.Applied != 2) || (remote && (got.Applied != 1 || got.LaterEdits != 1)) {
				t.Fatalf("undo=%+v", got)
			}
			p := f.record(page).Item.Data.(Page)
			if p.Parent != nil {
				t.Fatalf("place was not restored: %+v", p)
			}
			if remote && p.Title != "remote" {
				t.Fatal("remote title overwritten")
			}
			redone := f.undo("a", true)
			if redone.Applied != got.Applied || redone.Skipped != 0 {
				t.Fatalf("redo=%+v", redone)
			}
			p = f.record(page).Item.Data.(Page)
			if p.Parent == nil || (remote && p.Title != "remote") {
				t.Fatalf("redo applied wrong groups: %+v", p)
			}
			var seq string
			if err := f.s.db.QueryRowContext(t.Context(), `SELECT seq FROM meta`).Scan(&seq); err != nil {
				t.Fatal(err)
			}
			if seq != "0" {
				t.Fatalf("undo allocated sequence: %s", seq)
			}
		})
	}
}
