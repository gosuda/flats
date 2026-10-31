package notebook

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRemapCopies(t *testing.T) {
	t.Parallel()
	page, child, external := copyTestID(PageKind, 1), copyTestID(PageKind, 2), copyTestID(PageKind, 3)
	parent, link, text, image := copyTestID(BlockKind, 1), copyTestID(BlockKind, 2), copyTestID(BlockKind, 3), copyTestID(BlockKind, 4)
	source := []itemRecord{
		copyTestRecord(child, Page{Title: "Child", Parent: &page, Position: "b"}),
		copyTestRecord(link, Block{Page: child, Parent: &parent, Position: "a", Content: Content{Kind: PageLink, Target: page}}),
		copyTestRecord(page, Page{Title: "Root", Position: "a"}),
		copyTestRecord(parent, Block{Page: child, Position: "a", Content: Content{Kind: Toggle, OpenDefault: true, Text: RichText{}}}),
		copyTestRecord(text, Block{Page: page, Position: "b", Content: Content{Kind: Paragraph, Text: RichText{
			{Text: "internal", Marks: []Mark{Bold}, Link: &Link{Page: child}},
			{Text: "external", Marks: []Mark{}, Link: &Link{Page: external}},
			{Text: "url", Marks: []Mark{}, Link: &Link{URL: "https://example.com"}},
		}}}),
		copyTestRecord(image, Block{Page: page, Position: "c", Content: Content{Kind: Image, Asset: fmt.Sprintf("%064d", 1), Alt: "image", Caption: RichText{{Text: "caption", Marks: []Mark{}, Link: &Link{Page: child}}}}}),
		copyTestRecord(copyTestID(BlockKind, 5), Block{Page: page, Position: "d", Content: Content{Kind: PageLink, Target: external}}),
	}
	ids := make(map[ID]ID)
	for index, record := range source {
		ids[record.Item.ID()] = copyTestID(record.Item.Kind(), uint64(index+100))
	}
	before, err := CanonicalJSON(copyTestItems(source))
	if err != nil {
		t.Fatal(err)
	}
	copies, err := remapCopies(source, ids, map[ID]copyPlacement{page: {Position: "z"}}, 42, nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[ID]Item{}
	for index, record := range copies {
		byID[source[index].Item.ID()] = record.Item
		if record.Item.Ref != (Ref{ID: ids[source[index].Item.ID()], Inc: 42}) || record.Item.Ver != 42 || len(record.Item.V) != 0 || record.Aggregate != 42 || !record.Alive || record.DeletedAt != 0 {
			t.Fatalf("copy lifetime = %#v", record)
		}
	}
	if got := byID[page].Data.(Page); got.Title != "Root" || got.Parent != nil || got.Position != "z" {
		t.Fatalf("root = %#v", got)
	}
	if got := byID[child].Data.(Page); got.Parent == nil || *got.Parent != ids[page] {
		t.Fatalf("child = %#v", got)
	}
	if got := byID[link].Data.(Block); got.Page != ids[child] || got.Parent == nil || *got.Parent != ids[parent] || got.Content.Target != ids[page] {
		t.Fatalf("page link block = %#v", got)
	}
	if got := byID[copyTestID(BlockKind, 5)].Data.(Block).Content.Target; got != external {
		t.Fatalf("external page link remapped to %s", got)
	}
	runs := byID[text].Data.(Block).Content.Text
	if runs[0].Link.Page != ids[child] || runs[1].Link.Page != external || runs[2].Link.URL != "https://example.com" {
		t.Fatalf("links = %#v", runs)
	}
	if got := byID[image].Data.(Block).Content; got.Caption[0].Link.Page != ids[child] || got.Asset != source[5].Item.Data.(Block).Content.Asset || got.Alt != "image" {
		t.Fatalf("image = %#v", got)
	}
	runs[0].Marks[0] = Italic
	runs[0].Link.Page = external
	*byID[child].Data.(Page).Parent = external
	after, err := CanonicalJSON(copyTestItems(source))
	if err != nil || string(after) != string(before) {
		t.Fatalf("remapping aliases source: %v", err)
	}
}

func TestRemapCopiesRecoveryIdentities(t *testing.T) {
	t.Parallel()
	page, liveChild, external := copyTestID(PageKind, 1), copyTestID(PageKind, 2), copyTestID(PageKind, 3)
	block := copyTestID(BlockKind, 1)
	source := []itemRecord{
		copyTestRecord(page, Page{Title: "Historical", Parent: &external, Position: "a"}),
		copyTestRecord(liveChild, Page{Title: "Collision", Parent: &page, Position: "a"}),
		copyTestRecord(block, Block{Page: liveChild, Position: "a", Content: Content{Kind: PageLink, Target: page}}),
	}
	ids := map[ID]ID{page: page, liveChild: copyTestID(PageKind, 100), block: block}
	copies, err := remapCopies(source, ids, map[ID]copyPlacement{page: {Position: "b"}}, 55, nil)
	if err != nil {
		t.Fatal(err)
	}
	if copies[0].Item.ID() != page || copies[0].Item.Ref.Inc != 55 || copies[0].Item.Data.(Page).Parent != nil || copies[1].Item.ID() != ids[liveChild] || copies[2].Item.Data.(Block).Page != ids[liveChild] || copies[2].Item.Data.(Block).Content.Target != page {
		t.Fatalf("recovery identities = %#v", copies)
	}
}

func TestRemapCopiesBlockDestination(t *testing.T) {
	t.Parallel()
	page, destination := copyTestID(PageKind, 1), copyTestID(PageKind, 2)
	root, child, destinationParent := copyTestID(BlockKind, 1), copyTestID(BlockKind, 2), copyTestID(BlockKind, 3)
	source := []itemRecord{
		copyTestRecord(child, Block{Page: page, Parent: &root, Position: "a", Content: Content{Kind: Paragraph, Text: RichText{}}}),
		copyTestRecord(root, Block{Page: page, Position: "a", Content: Content{Kind: Bullet, Text: RichText{}}}),
	}
	ids := map[ID]ID{root: copyTestID(BlockKind, 100), child: copyTestID(BlockKind, 101)}
	copies, err := remapCopies(source, ids, map[ID]copyPlacement{root: {Page: destination, Parent: &destinationParent, Position: "z"}}, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotChild, gotRoot := copies[0].Item.Data.(Block), copies[1].Item.Data.(Block)
	if gotChild.Page != destination || gotChild.Parent == nil || *gotChild.Parent != ids[root] || gotRoot.Page != destination || gotRoot.Parent == nil || *gotRoot.Parent != destinationParent || gotRoot.Position != "z" {
		t.Fatalf("destination: child %#v, root %#v", gotChild, gotRoot)
	}
}

func TestRemapCopiesRejectsIncompleteMap(t *testing.T) {
	t.Parallel()
	page, child := copyTestID(PageKind, 1), copyTestID(PageKind, 2)
	source := []itemRecord{copyTestRecord(page, Page{Position: "a"}), copyTestRecord(child, Page{Position: "b"})}
	for _, test := range []struct {
		name string
		ids  map[ID]ID
	}{
		{name: "missing", ids: map[ID]ID{page: copyTestID(PageKind, 100)}},
		{name: "collision", ids: map[ID]ID{page: copyTestID(PageKind, 100), child: copyTestID(PageKind, 100)}},
		{name: "wrong kind", ids: map[ID]ID{page: copyTestID(BlockKind, 100), child: copyTestID(PageKind, 101)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := remapCopies(source, test.ids, nil, 50, nil); err == nil {
				t.Fatal("accepted invalid identity map")
			}
		})
	}
}

func TestRemapCopiesDatabaseExtensionRequired(t *testing.T) {
	t.Parallel()
	block, database := copyTestID(BlockKind, 1), copyTestID(DatabaseKind, 1)
	source := []itemRecord{copyTestRecord(block, Block{Page: copyTestID(PageKind, 1), Position: "a", Content: Content{Kind: InlineDatabase, Database: database}})}
	ids := map[ID]ID{block: copyTestID(BlockKind, 100), database: copyTestID(DatabaseKind, 100)}
	if _, err := remapCopies(source, ids, nil, 50, nil); err == nil {
		t.Fatal("copied database owner without its database")
	}
	source = append(source, copyTestRecord(database, Database{Block: block, Title: "Tasks", PropertyOrder: []ID{}, ViewOrder: []ID{}}))
	if _, err := remapCopies(source, ids, nil, 50, nil); err == nil {
		t.Fatal("copied database without an extension")
	}
	copies, err := remapCopies(source, ids, nil, 50, copyTestDatabaseExtension{})
	if err != nil {
		t.Fatal(err)
	}
	if copies[0].Item.Data.(Block).Content.Database != ids[database] || copies[1].Item.Data.(Database).Block != ids[block] {
		t.Fatal("extension did not share complete identity map")
	}
	if _, err := remapCopies(source[:1], ids, nil, 50, copyTestDatabaseExtension{}); err != nil {
		t.Fatalf("reserved database identity: %v", err)
	}
	delete(ids, database)
	if _, err := remapCopies(source[:1], ids, nil, 50, copyTestDatabaseExtension{}); err == nil {
		t.Fatal("accepted database owner without reserved identity")
	}
}

type copyTestDatabaseExtension struct{}

func (copyTestDatabaseExtension) Remap(data ItemData, ids map[ID]ID) (ItemData, error) {
	database := data.(Database)
	database.Block = ids[database.Block]
	return database, nil
}

func TestDuplicatePage(t *testing.T) {
	t.Parallel()
	copyTestTransaction(t, func(e *edit) error {
		page, child, outside := copyTestID(PageKind, 1), copyTestID(PageKind, 2), copyTestID(PageKind, 3)
		link, text := copyTestID(BlockKind, 1), copyTestID(BlockKind, 2)
		source := []itemRecord{
			copyTestRecord(page, Page{Title: "Root", Position: "a"}),
			copyTestRecord(child, Page{Title: "Child", Parent: &page, Position: "a"}),
			copyTestRecord(outside, Page{Title: "Outside", Position: "b"}),
			copyTestRecord(link, Block{Page: page, Position: "a", Content: Content{Kind: PageLink, Target: child}}),
			copyTestRecord(text, Block{Page: child, Position: "a", Content: Content{Kind: Paragraph, Text: RichText{{Text: "internal", Marks: []Mark{}, Link: &Link{Page: page}}, {Text: "outside", Marks: []Mark{}, Link: &Link{Page: outside}}}}}),
		}
		copyTestSeed(t, e, source)
		if err := e.duplicatePage(&DuplicatePageOp{Page: Target{Ref: &source[0].Item.Ref}}); err != nil {
			return err
		}
		if len(e.result.Copies) != 4 || len(e.changes) != 4 {
			t.Fatalf("copy mapping %d, changes %d", len(e.result.Copies), len(e.changes))
		}
		mapped := copyTestMappings(t, e)
		root := copyTestLoad(t, e, mapped[page].ID).Item.Data.(Page)
		if root.Title != "Root" || root.Parent != nil || !("a" < root.Position && root.Position < "b") {
			t.Fatalf("copied page placement = %#v", root)
		}
		if got := copyTestLoad(t, e, mapped[child].ID).Item.Data.(Page); got.Parent == nil || *got.Parent != mapped[page].ID {
			t.Fatalf("copied child = %#v", got)
		}
		if got := copyTestLoad(t, e, mapped[link].ID).Item.Data.(Block); got.Page != mapped[page].ID || got.Content.Target != mapped[child].ID {
			t.Fatalf("copied link = %#v", got)
		}
		if got := copyTestLoad(t, e, mapped[text].ID).Item.Data.(Block); got.Page != mapped[child].ID || got.Content.Text[0].Link.Page != mapped[page].ID || got.Content.Text[1].Link.Page != outside {
			t.Fatalf("copied rich text = %#v", got)
		}
		for _, record := range source {
			if got := copyTestLoad(t, e, record.Item.ID()); !reflect.DeepEqual(got, record) {
				t.Fatalf("source changed: %s", record.Item.ID())
			}
		}
		return nil
	})
}

func TestDuplicateBlocksStableRoots(t *testing.T) {
	t.Parallel()
	copyTestTransaction(t, func(e *edit) error {
		page, destination := copyTestID(PageKind, 1), copyTestID(PageKind, 2)
		first, child, second, anchor, holder := copyTestID(BlockKind, 1), copyTestID(BlockKind, 2), copyTestID(BlockKind, 3), copyTestID(BlockKind, 4), copyTestID(BlockKind, 5)
		source := []itemRecord{
			copyTestRecord(page, Page{Position: "a"}), copyTestRecord(destination, Page{Position: "b"}),
			copyTestRecord(first, Block{Page: page, Position: "a", Content: Content{Kind: Toggle, Text: RichText{}}}),
			copyTestRecord(child, Block{Page: page, Parent: &first, Position: "a", Content: Content{Kind: Paragraph, Text: RichText{}}}),
			copyTestRecord(second, Block{Page: page, Parent: &holder, Position: "a", Content: Content{Kind: Paragraph, Text: RichText{}}}),
			copyTestRecord(anchor, Block{Page: destination, Position: "a", Content: Content{Kind: Bullet, Text: RichText{}}}),
			copyTestRecord(holder, Block{Page: page, Position: "b", Content: Content{Kind: Toggle, Text: RichText{}}}),
		}
		copyTestSeed(t, e, source)
		if err := e.duplicateBlocks(&DuplicateBlocksOp{Blocks: []Target{{Ref: &source[4].Item.Ref}, {Ref: &source[3].Item.Ref}, {Ref: &source[2].Item.Ref}}, After: Target{Ref: &source[5].Item.Ref}}); err != nil {
			return err
		}
		if len(e.result.Copies) != 3 {
			t.Fatalf("ancestor normalization copied %d blocks", len(e.result.Copies))
		}
		mapped := copyTestMappings(t, e)
		gotFirst, gotChild, gotSecond := copyTestLoad(t, e, mapped[first].ID).Item.Data.(Block), copyTestLoad(t, e, mapped[child].ID).Item.Data.(Block), copyTestLoad(t, e, mapped[second].ID).Item.Data.(Block)
		if gotFirst.Page != destination || gotSecond.Page != destination || gotChild.Page != destination || gotFirst.Parent != nil || gotSecond.Parent != nil || gotChild.Parent == nil || *gotChild.Parent != mapped[first].ID || !("a" < gotFirst.Position && gotFirst.Position < gotSecond.Position) {
			t.Fatalf("copied destinations/order: %#v %#v %#v", gotFirst, gotChild, gotSecond)
		}
		return nil
	})
}

func TestDuplicateBlocks501(t *testing.T) {
	t.Parallel()
	copyTestTransaction(t, func(e *edit) error {
		page := copyTestID(PageKind, 1)
		source := []itemRecord{copyTestRecord(page, Page{Position: "a"})}
		var targets []Target
		for index := range 501 {
			record := copyTestRecord(copyTestID(BlockKind, uint64(index+1)), Block{Page: page, Position: fmt.Sprintf("a%04d", index), Content: Content{Kind: Paragraph, Text: RichText{{Text: fmt.Sprint(index), Marks: []Mark{}}}}})
			source = append(source, record)
			ref := record.Item.Ref
			targets = append(targets, Target{Ref: &ref})
		}
		copyTestSeed(t, e, source)
		if err := e.duplicateBlocks(&DuplicateBlocksOp{Blocks: targets, After: targets[len(targets)-1]}); err != nil {
			return err
		}
		if len(e.result.Copies) != 501 {
			t.Fatalf("mapping %d; want 501 in one operation", len(e.result.Copies))
		}
		mapped := copyTestMappings(t, e)
		for _, ref := range mapped {
			change, ok := e.changes[ref.ID]
			if !ok || change.Before != nil || change.After.Item.Ref != ref {
				t.Fatalf("copy %s missing from creation journal", ref.ID)
			}
		}
		var previous string
		for _, original := range source[1:] {
			got := copyTestLoad(t, e, mapped[original.Item.ID()].ID).Item.Data.(Block)
			if !reflect.DeepEqual(got.Content, original.Item.Data.(Block).Content) || got.Page != page || got.Parent != nil || previous >= got.Position {
				t.Fatalf("copied block %s content/place/order changed", original.Item.ID())
			}
			previous = got.Position
		}
		var count int
		if err := e.tx.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM blocks JOIN items USING(id) WHERE items.alive=1`).Scan(&count); err != nil {
			return err
		}
		if count != 1002 {
			t.Fatalf("live block count = %d, want 1002", count)
		}
		return nil
	})
}

func TestDuplicateBlocksGoneAnchor(t *testing.T) {
	t.Parallel()
	copyTestTransaction(t, func(e *edit) error {
		page, block := copyTestID(PageKind, 1), copyTestID(BlockKind, 1)
		source := []itemRecord{copyTestRecord(page, Page{Position: "a"}), copyTestRecord(block, Block{Page: page, Position: "a", Content: Content{Kind: Divider}})}
		copyTestSeed(t, e, source)
		for _, inc := range []Incarnation{6, 8} {
			err := e.duplicateBlocks(&DuplicateBlocksOp{Blocks: []Target{{Ref: &source[1].Item.Ref}}, After: Target{Ref: &Ref{ID: block, Inc: inc}}})
			var failure *editFailure
			if !errors.As(err, &failure) || failure.Status != "ignored" || failure.Reason != "gone" || len(e.result.Copies) != 0 || len(e.changes) != 0 {
				t.Fatalf("stale anchor: %v, copies %d, changes %d", err, len(e.result.Copies), len(e.changes))
			}
		}
		return nil
	})
}

func TestDuplicateRefusesMissingDatabaseWithoutPartialCopies(t *testing.T) {
	t.Parallel()
	copyTestTransaction(t, func(e *edit) error {
		page, block, database := copyTestID(PageKind, 1), copyTestID(BlockKind, 1), copyTestID(DatabaseKind, 1)
		source := []itemRecord{
			copyTestRecord(page, Page{Position: "a"}),
			copyTestRecord(block, Block{Page: page, Position: "a", Content: Content{Kind: InlineDatabase, Database: database}}),
		}
		copyTestSeed(t, e, source)
		err := e.duplicatePage(&DuplicatePageOp{Page: Target{Ref: &source[0].Item.Ref}})
		var failure *editFailure
		if !errors.As(err, &failure) || failure.Status != "refused" || failure.Reason != "structure" || len(e.result.Copies) != 0 || len(e.changes) != 0 {
			t.Fatalf("database refusal: %v, copies %d, changes %d", err, len(e.result.Copies), len(e.changes))
		}
		var count int
		if err := e.tx.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM items`).Scan(&count); err != nil {
			return err
		}
		if count != len(source) {
			t.Fatalf("refused duplicate left %d items, want %d", count, len(source))
		}
		return nil
	})
}

func copyTestID(kind Kind, value uint64) ID {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[8:], value)
	return ID(kindPrefixes[kind] + idEncoding.EncodeToString(raw[:]))
}

func copyTestRecord(id ID, data ItemData) itemRecord {
	return itemRecord{Item: Item{Ref: Ref{ID: id, Inc: 7}, Ver: 7, V: map[GroupKey]Version{}, Data: data}, Alive: true, Aggregate: 7}
}

func copyTestItems(records []itemRecord) []Item {
	items := make([]Item, len(records))
	for index, record := range records {
		items[index] = record.Item
	}
	return items
}

func copyTestTransaction(t *testing.T, run func(*edit) error) {
	t.Helper()
	store, _, err := openStore(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Error(err)
		}
	})
	if err := store.transaction(t.Context(), func(tx *sql.Tx) error {
		e := &edit{ctx: t.Context(), tx: tx, seq: 42, session: "copy-test", refs: map[string]Ref{}, changes: map[ID]itemChange{}, index: 9}
		return run(e)
	}); err != nil {
		t.Fatal(err)
	}
}

func copyTestSeed(t *testing.T, e *edit, records []itemRecord) {
	t.Helper()
	for _, record := range records {
		if err := putRecord(e.ctx, e.tx, record); err != nil {
			t.Fatal(err)
		}
	}
}

func copyTestLoad(t *testing.T, e *edit, id ID) itemRecord {
	t.Helper()
	record, err := loadRecord(e.ctx, e.tx, id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func copyTestMappings(t *testing.T, e *edit) map[ID]Ref {
	t.Helper()
	mapped := map[ID]Ref{}
	for _, mapping := range e.result.Copies {
		if mapping.Index != e.index || mapping.Ref.ID == mapping.Source.ID || mapping.Ref.Inc != Incarnation(e.seq) || mapping.Ver != Version(e.seq) {
			t.Fatalf("invalid mapping %#v", mapping)
		}
		if _, duplicate := mapped[mapping.Source.ID]; duplicate {
			t.Fatalf("repeated mapping %s", mapping.Source.ID)
		}
		mapped[mapping.Source.ID] = mapping.Ref
	}
	return mapped
}
