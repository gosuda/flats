package core

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

const sheetTickTimeout = 30 * time.Second
const sheetImportBatch = 100
const sheetManifest = "sheet.json"

type sheetImport struct {
	revisions []int
	byOp      map[string]int
}

type sheetBackoff struct {
	next  time.Time
	delay time.Duration
}

// sheetCalculating reports the refusal of a sheet that loads or
// recalculates without an edit and takes no work of its writer meanwhile.
// A caller answers it by trying again.
func sheetCalculating(err error) bool {
	var problem *sheet.WriteError
	return errors.As(err, &problem) && problem.Code == "unavailable" && problem.Reason == "calculating"
}

// importSheetLocked records the sheet's outbox as revisions. It commits the
// revisions in flats.db before it lets the instance drop their snapshots.
//
// A sheet that calculates without an edit, for a time only its content
// bounds, takes no acknowledgement. The import does not wait for it. The
// revisions are recorded all the same, the sheet keeps their snapshots,
// and the import of a later tick acknowledges them again, as after a crash
// between the two.
func (s *Service) importSheetLocked(ctx context.Context, slugName string, in *sheet.Instance) (sheetImport, error) {
	res := sheetImport{byOp: map[string]int{}}
	deferred := false
	acknowledge := func(through, stored int64) error {
		if deferred {
			return nil
		}
		err := in.Acknowledge(ctx, through, stored)
		if !sheetCalculating(err) {
			return sheetErr(err)
		}
		deferred = true
		s.sheetPending.Store(slugName, true)
		return nil
	}
	start := time.Now()
	st, err := s.st.ContentState(ctx, slugName)
	if err != nil {
		return res, err
	}
	if st.Type != contenttype.Sheet {
		return res, ErrNotSheet
	}
	outbox := in.StoreID()
	if outbox == "" {
		return res, fmt.Errorf("%w: empty sheet store identity", ErrUnavailable)
	}
	cursor := int64(0)
	if st.ImportStore == outbox {
		cursor = st.ImportSeq
	}
	if cursor > 0 {
		// A crash after the flats.db commit left entries the cursor covers.
		old, err := in.Outbox(ctx, 0, 1)
		if err != nil {
			return res, sheetErr(err)
		}
		if len(old) > 0 && old[0].Seq <= cursor {
			if err := acknowledge(cursor, 0); err != nil {
				return res, err
			}
			if !deferred {
				if err := in.Remeasure(ctx); err != nil {
					return res, sheetErr(err)
				}
			}
		}
	}
	backend := sheetBackend{s: s, slug: slugName}
	for {
		entries, err := in.Outbox(ctx, cursor, sheetImportBatch)
		if err != nil {
			return res, sheetErr(err)
		}
		if len(entries) == 0 {
			break
		}
		stored, err := s.storeSheetEntries(ctx, slugName, in, backend, entries, cursor)
		if err == nil {
			err = s.commitSheetEntries(ctx, slugName, outbox, entries, &res)
		}
		if err == nil {
			cursor = entries[len(entries)-1].Seq
			err = acknowledge(cursor, stored)
		}
		if (err != nil || deferred) && stored > 0 {
			// The blobs of this attempt stay in the store, and the sheet
			// was not told of them. The next attempt finds them there and
			// reports nothing stored, so the sheet measures them now.
			if merr := in.Remeasure(context.WithoutCancel(ctx)); merr != nil {
				s.logf("remeasure sheet %s: %v", slugName, merr)
			}
		}
		if err != nil {
			return res, err
		}
	}
	if len(res.revisions) > 0 {
		s.sheetStats.observeImport(time.Since(start))
		_ = s.st.Touch(ctx, slugName, s.now())
	}
	s.settleSheetHoldsLocked(ctx, slugName, in)
	s.sheetRetry.Delete(slugName)
	return res, nil
}

// storeSheetEntries checks a batch of outbox entries after cursor and puts
// their new snapshots into the blob store. It returns the bytes it added to
// the store, also when it fails after adding some.
func (s *Service) storeSheetEntries(ctx context.Context, slugName string, in *sheet.Instance, backend sheetBackend, entries []sheet.OutboxEntry, cursor int64) (stored int64, err error) {
	previous := cursor
	for _, entry := range entries {
		if entry.Seq <= previous {
			return stored, fmt.Errorf("%w: sheet outbox is not ordered", ErrUnavailable)
		}
		previous = entry.Seq
		if !validSheetDate(entry.CalcDate) {
			return stored, fmt.Errorf("%w: sheet outbox entry %d has no calculation date", ErrUnavailable, entry.Seq)
		}
		paths := map[string]bool{}
		hashes := map[string]bool{}
		for _, f := range entry.Files {
			if err := sheet.CheckFile(f); err != nil {
				return stored, fmt.Errorf("%w: %v", ErrUnavailable, err)
			}
			if paths[f.Path] {
				return stored, fmt.Errorf("%w: duplicate sheet path", ErrUnavailable)
			}
			paths[f.Path] = true
			hashes[f.Hash] = true
		}
		if !paths[sheetManifest] {
			return stored, fmt.Errorf("%w: missing sheet manifest", ErrUnavailable)
		}
		for _, hash := range entry.NewHashes {
			if !hashes[hash] {
				return stored, fmt.Errorf("%w: unreferenced sheet snapshot", ErrUnavailable)
			}
			data, err := in.SnapshotData(ctx, hash)
			if err != nil {
				return stored, sheetErr(err)
			}
			added, err := backend.PutBlob(ctx, hash, data)
			if err != nil {
				return stored, err
			}
			if added {
				stored += int64(len(data))
			}
		}
		for _, f := range entry.Files {
			if err := s.checkSheetBlob(slugName, f); err != nil {
				return stored, err
			}
		}
	}
	return stored, nil
}

// checkSheetBlob verifies that the blob of f is stored with its size. The
// blob store is content-addressed and PutBlob checks the hash of what it
// stores, so the bytes are not read again.
func (s *Service) checkSheetBlob(slugName string, f sheet.File) error {
	release := s.lease(slugName, []string{f.Hash})
	defer release()
	p, ok := blobPath(s.docsStoreDir(slugName), f.Hash)
	if !ok {
		return fmt.Errorf("%w: sheet file %s has an invalid hash", ErrUnavailable, f.Path)
	}
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("%w: sheet file %s: %v", ErrUnavailable, f.Path, err)
	}
	if info.Size() != f.Size {
		return fmt.Errorf("%w: sheet file %s does not match its blob", ErrUnavailable, f.Path)
	}
	return nil
}

func validSheetDate(date string) bool {
	_, err := time.Parse("2006-01-02", date)
	return err == nil
}

// sheetTabState is what a revision holds of one tab: its position in the
// manifest, its manifest entry and its files.
type sheetTabState struct {
	name  string
	pos   int
	entry []byte
	files map[string]string
}

// sheetTabStates reads the tabs of a revision from its manifest and file
// list. A revision without a manifest has no tabs. The manifest is read as
// the sheet reads a revision file: within the size its entry gives and the
// most the writer writes, under a lease, ending with ctx, and checked
// against its hash at its end.
func (s *Service) sheetTabStates(ctx context.Context, slugName string, files []store.RevisionFile) (map[uint64]*sheetTabState, error) {
	tabs := map[uint64]*sheetTabState{}
	backend := sheetBackend{s: s, slug: slugName}
	open := func(hash string) (io.ReadCloser, error) { return backend.OpenBlob(ctx, hash) }
	for _, f := range files {
		if f.Path != sheetManifest {
			continue
		}
		data, err := sheet.ReadRevisionFile(sheet.File{Path: f.Path, Hash: f.Hash, Size: f.Size}, open)
		if err != nil {
			return nil, err
		}
		var manifest struct {
			Tabs []json.RawMessage `json:"tabs"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("sheet manifest: %w", err)
		}
		for pos, raw := range manifest.Tabs {
			var tab struct {
				ID   uint64 `json:"id"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &tab); err != nil {
				return nil, fmt.Errorf("sheet manifest: %w", err)
			}
			tabs[tab.ID] = &sheetTabState{name: tab.Name, pos: pos, entry: raw, files: map[string]string{}}
		}
	}
	for _, f := range files {
		rest, ok := strings.CutPrefix(f.Path, "tabs/")
		if !ok {
			continue
		}
		number, _, _ := strings.Cut(rest, "/")
		id, err := strconv.ParseUint(number, 10, 64)
		if err != nil {
			continue
		}
		if tab := tabs[id]; tab != nil {
			tab.files[f.Path] = f.Hash
		}
	}
	return tabs, nil
}

// sheetStatesErr is the error of an import whose read of a manifest
// failed: the end of its request, or else a damaged revision.
func sheetStatesErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

// sheetTabChanges names the tabs that differ between two revisions, in tab
// id order: a tab only one of them has, and a tab whose name, settings,
// files or place in the tab order changed (spec rule 51).
func sheetTabChanges(before, after map[uint64]*sheetTabState) []store.SheetTabChange {
	out := []store.SheetTabChange{}
	moved := movedSheetTabs(before, after)
	for id, old := range before {
		tab, ok := after[id]
		switch {
		case !ok:
			out = append(out, store.SheetTabChange{Tab: id, Change: "removed", NameBefore: old.name})
		case moved[id] || !bytes.Equal(old.entry, tab.entry) || !sameSheetFiles(old.files, tab.files):
			out = append(out, store.SheetTabChange{Tab: id, Change: "changed", NameBefore: old.name, NameAfter: tab.name})
		}
	}
	for id, tab := range after {
		if _, ok := before[id]; !ok {
			out = append(out, store.SheetTabChange{Tab: id, Change: "added", NameAfter: tab.name})
		}
	}
	slices.SortFunc(out, func(a, b store.SheetTabChange) int { return cmp.Compare(a.Tab, b.Tab) })
	return out
}

// movedSheetTabs names the tabs whose place among the tabs both revisions
// have changed. The manifest entry of a tab does not hold its position, so
// a move shows only in the order of the manifest. Of the tabs both have,
// the longest run that kept its relative order stayed, and the others
// moved: dragging one tab past five reports that tab, not the five.
func movedSheetTabs(before, after map[uint64]*sheetTabState) map[uint64]bool {
	var ids []uint64
	for id := range before {
		if after[id] != nil {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b uint64) int { return cmp.Compare(before[a].pos, before[b].pos) })
	// run[i] is the longest run ending at ids[i] whose positions in after
	// rise, and prev[i] is the tab before ids[i] in it.
	run, prev := make([]int, len(ids)), make([]int, len(ids))
	last := -1
	for i, id := range ids {
		run[i], prev[i] = 1, -1
		for j := range i {
			if after[ids[j]].pos < after[id].pos && run[j]+1 > run[i] {
				run[i], prev[i] = run[j]+1, j
			}
		}
		if last < 0 || run[i] > run[last] {
			last = i
		}
	}
	moved := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		moved[id] = true
	}
	for i := last; i >= 0; i = prev[i] {
		delete(moved, ids[i])
	}
	return moved
}

func sameSheetFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for path, hash := range a {
		if b[path] != hash {
			return false
		}
	}
	return true
}

func (s *Service) commitSheetEntries(ctx context.Context, slugName, outbox string, entries []sheet.OutboxEntry, res *sheetImport) error {
	var prev []store.RevisionFile
	revs, err := s.st.ListRevisions(ctx, slugName, 0, 1)
	if err != nil {
		return err
	}
	if len(revs) > 0 {
		prev, err = s.sheetRevisionList(ctx, slugName, revs[0].Number)
		if err != nil {
			return err
		}
	}
	before, err := s.sheetTabStates(ctx, slugName, prev)
	if err != nil {
		return sheetStatesErr(ctx, err)
	}
	type prepared struct {
		files  []store.RevisionFile
		tabs   []store.SheetTabChange
		size   int64
		change store.Changes
	}
	preps := make([]prepared, len(entries))
	for i, e := range entries {
		p := prepared{}
		for _, f := range e.Files {
			p.files = append(p.files, store.RevisionFile{Path: f.Path, Hash: f.Hash, Size: f.Size})
			p.size += f.Size
		}
		after, err := s.sheetTabStates(ctx, slugName, p.files)
		if err != nil {
			return sheetStatesErr(ctx, err)
		}
		p.tabs = sheetTabChanges(before, after)
		p.change = diffFiles(prev, p.files)
		preps[i], prev, before = p, p.files, after
	}
	var imported []int
	byOp := map[string]int{}
	deleted := 0
	err = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		st, err := tx.State(ctx)
		if err != nil {
			return err
		}
		latest, err := tx.LatestRevision(ctx)
		if err != nil {
			return err
		}
		head := -1
		for i, e := range entries {
			if st.ImportStore == outbox && e.Seq <= st.ImportSeq {
				continue
			}
			latest++
			p := preps[i]
			kind := e.Kind
			if kind == "" {
				kind = "person"
			}
			// The sheet leaves the target of a rollback to the op id core chose.
			target := e.RollbackOf
			if kind == "rollback" && target == 0 {
				target = sheetRollbackTarget(e.OpID)
			}
			r := store.Revision{Number: latest, CreatedAt: time.Unix(e.CreatedAt, 0), Kind: kind, Authors: dedupe(e.Authors), Message: truncate(e.Message, 500), RollbackOf: target, Changes: p.change, Files: len(p.files), Size: p.size}
			if err := tx.InsertRevision(ctx, r, p.files); err != nil {
				return err
			}
			if err := tx.PutSheetRevTabs(ctx, latest, p.tabs); err != nil {
				return err
			}
			if err := tx.PutSheetImport(ctx, outbox, e.Seq, e.OpID, latest, e.CalcDate); err != nil {
				return err
			}
			if kind == "rollback" && e.OpID != "" {
				if err := tx.DeleteHold(ctx, e.OpID); err != nil {
					return err
				}
			}
			imported = append(imported, latest)
			if e.OpID != "" {
				byOp[e.OpID] = latest
			}
			st.ImportStore, st.ImportSeq, head = outbox, e.Seq, i
		}
		if head >= 0 {
			if err := tx.ReplaceHead(ctx, preps[head].files); err != nil {
				return err
			}
		}
		if err := tx.SetImport(ctx, st.ImportStore, st.ImportSeq); err != nil {
			return err
		}
		if len(imported) > 0 {
			deleted, err = s.retainTx(ctx, tx)
		}
		return err
	})
	if err != nil {
		return err
	}
	res.revisions = append(res.revisions, imported...)
	for op, n := range byOp {
		res.byOp[op] = n
	}
	if deleted > 0 {
		go s.sweepBlobs(context.WithoutCancel(ctx), slugName, contenttype.Sheet)
	}
	return nil
}

// sheetReadHoldPrefix names the hold of a call that reads or pins a
// revision. It never reaches the outbox, so a settlement drops it once its
// call is over.
const sheetReadHoldPrefix = "sheet-read:"

// holdSheetRevisionLocked holds revision n for op and then imports the
// sheet's outbox. The import runs retention, which would otherwise delete
// n when it is the oldest unpinned revision. A revision n not in flats.db
// yet may be one the sheet recorded and core has not imported: it is held
// after the import. A hold is not a pin and takes none of the pin quota.
//
// op is in sheetRollbacks from the start, so settlements keep its hold.
// release drops both. A rollback that commits hands its hold to its outbox
// entry instead and does not call it.
func (s *Service) holdSheetRevisionLocked(ctx context.Context, slugName string, in *sheet.Instance, op string, n int) (release func(), err error) {
	s.sheetRollbacks.Store(op, true)
	held := false
	release = func() {
		if held {
			dctx := context.WithoutCancel(ctx)
			// A hold left by a failure here is not in sheetRollbacks, so
			// the next settlement drops it.
			_ = s.st.ContentTx(dctx, slugName, func(tx *store.ContentTx) error { return tx.DeleteHold(dctx, op) })
		}
		s.sheetRollbacks.Delete(op)
	}
	hold := func() error {
		return s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
			if _, err := tx.Revision(ctx, n); err != nil {
				return err
			}
			return tx.AddHold(ctx, op, n)
		})
	}
	err = hold()
	held = err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		release()
		return nil, err
	}
	if _, ierr := s.importSheetLocked(ctx, slugName, in); ierr != nil {
		s.logf("sheet revisions of %s: %v", slugName, ierr)
	}
	if !held {
		if err := hold(); err != nil {
			release()
			return nil, err
		}
		held = true
	}
	return release, nil
}

// settleSheetHoldsLocked drops the holds of rollbacks that did not commit
// and of reads that are over. A hold whose call still runs is in
// sheetRollbacks and stays. Of the others, a hold whose rollback committed
// has an outbox entry that still awaits import.
func (s *Service) settleSheetHoldsLocked(ctx context.Context, slugName string, in *sheet.Instance) {
	holds, err := s.st.Holds(ctx, slugName)
	if err != nil || len(holds) == 0 {
		return
	}
	st, err := s.st.ContentState(ctx, slugName)
	if err != nil {
		return
	}
	cursor := int64(0)
	if st.ImportStore == in.StoreID() {
		cursor = st.ImportSeq
	}
	pending := map[string]bool{}
	for {
		entries, err := in.Outbox(ctx, cursor, sheetImportBatch)
		if err != nil {
			return
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			pending[e.OpID] = true
		}
		cursor = entries[len(entries)-1].Seq
	}
	for _, h := range holds {
		ours := strings.HasPrefix(h.Op, sheetRollbackPrefix) || strings.HasPrefix(h.Op, sheetReadHoldPrefix)
		if _, running := s.sheetRollbacks.Load(h.Op); !ours || pending[h.Op] || running {
			continue
		}
		_ = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.DeleteHold(ctx, h.Op) })
	}
}

// prepareSheetSweepLocked readies a running sheet for a blob sweep. For a
// loaded sheet it imports the outbox and returns the instance and the
// hashes sheet.db still names. A sheet that core unloaded has an empty
// outbox, and its file list is the head in flats.db: in is nil, and
// flats.db and the leases name everything the sweep must keep. Entries a
// reload adds meanwhile keep their new files as snapshots in sheet.db.
func (s *Service) prepareSheetSweepLocked(ctx context.Context, slugName string) (in *sheet.Instance, refs []string, ok bool) {
	h := s.sheetHandle(slugName)
	if h == nil {
		return nil, nil, false
	}
	in, drained := h.sweepState()
	if in == nil {
		return nil, nil, drained
	}
	if _, err := s.importSheetLocked(ctx, slugName, in); err != nil {
		return nil, nil, false
	}
	refs, err := in.References(ctx)
	if err != nil {
		return nil, nil, false
	}
	return in, refs, true
}

// sweepSheetIfFree sweeps the blobs of a sheet unless its flat lock is
// taken; the sheet is then swept in a later round. The periodic sweep uses
// it, because the loop that sweeps serves every flat.
func (s *Service) sweepSheetIfFree(ctx context.Context, slugName string) {
	m, _ := s.locks.LoadOrStore(slugName, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	if !mu.TryLock() {
		return
	}
	defer mu.Unlock()
	if s.checkStorageJournal(slugName) != nil {
		return
	}
	s.sweepBlobsLocked(ctx, slugName, contenttype.Sheet)
}

// catchUpSheet imports a sheet's outbox before a history read or change.
func (s *Service) catchUpSheet(ctx context.Context, slugName string) error {
	return s.catchUpSheetKeeping(ctx, slugName, 0)
}

// catchUpSheetKeeping is catchUpSheet for a call that reads or pins
// revision n next: n is held while the import runs, so the call does not
// delete it itself. A revision another call deletes meanwhile is gone.
func (s *Service) catchUpSheetKeeping(ctx context.Context, slugName string, n int) error {
	unlock, err := s.lockSheetCtx(ctx, slugName)
	if err != nil {
		return err
	}
	defer func() { unlock() }()
	// A sheet that was unloaded is loaded again: it may hold entries that a
	// failed import left behind.
	in, err := s.openLiveSheet(slugName)
	if err != nil || in == nil {
		return nil
	}
	// Until its load has opened and checked sheet.db, the sheet has no
	// store to import from. The call waits for that without the flat lock,
	// so that nothing that takes the lock waits for the load, and imports
	// from the same instance afterwards. A load that ended without, or an
	// instance that stopped, leaves nothing to import, and the call goes on
	// with what flats.db holds.
	now, stop := context.WithCancel(ctx)
	stop()
	for {
		// A context that has ended asks without waiting; an outcome that
		// is known comes before its error.
		checked := in.AwaitChecked(now)
		if checked == nil {
			break
		}
		if checked != now.Err() {
			return nil
		}
		unlock()
		waited := in.AwaitChecked(ctx)
		if ctx.Err() != nil {
			unlock = func() {}
			return ctx.Err()
		}
		var err error
		if unlock, err = s.lockSheetCtx(ctx, slugName); err != nil {
			unlock = func() {}
			return err
		}
		if s.liveSheet(slugName) != in {
			// A delete, a rename, a stop or an idle unload replaced it.
			if in, err = s.openLiveSheet(slugName); err != nil || in == nil {
				return nil
			}
			continue
		}
		if waited != nil {
			return nil
		}
	}
	if n < 1 {
		if _, err := s.importSheetLocked(ctx, slugName, in); err != nil {
			s.logf("sheet revisions of %s: %v", slugName, err)
		}
		return nil
	}
	// A revision that does not exist is the caller's answer to give.
	if release, err := s.holdSheetRevisionLocked(ctx, slugName, in, sheetReadHoldPrefix+newOpID(), n); err == nil {
		release()
	}
	return nil
}

// catchUpRevisionKeeping is catchUpRevisions for a call about revision n:
// a sheet holds n while it imports.
func (s *Service) catchUpRevisionKeeping(ctx context.Context, slugName string, n int) error {
	if st, err := s.st.ContentState(ctx, slugName); err == nil && st.Type == contenttype.Sheet {
		return s.catchUpSheetKeeping(ctx, slugName, n)
	}
	return s.catchUpRevisions(ctx, slugName)
}

// TickSheets imports the outbox of every sheet that recorded entries,
// skipping sheets whose lock is taken until the next tick. A sheet whose
// import keeps failing is tried less often. A sheet that calculates
// without an edit, for a time its content alone bounds, takes no
// acknowledgement until it ends. Its entries wait for a later tick, and
// the tick goes on to the other sheets and content types.
func (s *Service) TickSheets(ctx context.Context) {
	now := time.Now()
	s.sheetPending.Range(func(k, _ any) bool {
		slugName := k.(string)
		in := s.liveSheet(slugName)
		if in == nil {
			s.sheetPending.Delete(slugName)
			s.sheetRetry.Delete(slugName)
			return true
		}
		if in.Stats().Calculating {
			return true
		}
		var backoff sheetBackoff
		if v, ok := s.sheetRetry.Load(slugName); ok {
			backoff = v.(sheetBackoff)
			if now.Before(backoff.next) {
				return true
			}
		}
		m, _ := s.locks.LoadOrStore(slugName, &sync.Mutex{})
		mu := m.(*sync.Mutex)
		if !mu.TryLock() {
			return true
		}
		defer mu.Unlock()
		if s.checkStorageJournal(slugName) != nil {
			return true
		}
		in = s.liveSheet(slugName)
		if in == nil {
			return true
		}
		s.sheetPending.Delete(slugName)
		ictx, cancel := context.WithTimeout(ctx, sheetTickTimeout)
		_, err := s.importSheetLocked(ictx, slugName, in)
		cancel()
		if err != nil {
			s.sheetPending.Store(slugName, true)
			backoff.delay = min(max(2*backoff.delay, 5*time.Second), 5*time.Minute)
			backoff.next = time.Now().Add(backoff.delay)
			s.sheetRetry.Store(slugName, backoff)
			s.logf("sheet revisions of %s (next try in %s): %v", slugName, backoff.delay, err)
		}
		return true
	})
}

func (s *Service) remeasureSheets() {
	for _, h := range s.sheetHandles() {
		if in := h.loaded(); in != nil {
			if err := in.Remeasure(context.Background()); err != nil {
				s.logf("remeasure sheet: %v", err)
			}
		}
	}
}

func (s *Service) sheetHandles() []*sheetInstance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var handles []*sheetInstance
	for _, lf := range s.live {
		if d := lf.cur.Load(); d != nil {
			if h, ok := d.inst.(*sheetInstance); ok {
				handles = append(handles, h)
			}
		}
	}
	return handles
}
