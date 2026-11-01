package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/notebook"
	"github.com/gosuda/flats/internal/store"
)

const notebookTickTimeout = 30 * time.Second
const notebookImportBatch = 100

type notebookImport struct {
	revisions []int
	bySeq     map[int64]int
}
type notebookBackoff struct {
	next  time.Time
	delay time.Duration
}

// importNotebookLocked commits metadata before releasing the instance's snapshots.
func (s *Service) importNotebookLocked(ctx context.Context, slugName string, in *notebook.Instance) (notebookImport, error) {
	res := notebookImport{bySeq: map[int64]int{}}
	start := time.Now()
	st, err := s.st.ContentState(ctx, slugName)
	if err != nil {
		return res, err
	}
	if st.Type != contenttype.Notebook {
		return res, ErrNotNotebook
	}
	outbox := in.StoreID()
	if outbox == "" {
		return res, fmt.Errorf("%w: empty notebook store identity", ErrUnavailable)
	}
	cursor := int64(0)
	if st.ImportStore == outbox {
		cursor = st.ImportSeq
	}
	if cursor > 0 {
		old, err := in.Outbox(ctx, 0, 1)
		if err != nil {
			return res, notebookErr(err)
		}
		if len(old) > 0 && old[0].Seq <= cursor {
			if err := in.Acknowledge(ctx, cursor, 0); err != nil {
				return res, notebookErr(err)
			}
			if err := in.Remeasure(ctx); err != nil {
				return res, notebookErr(err)
			}
		}
	}
	backend := notebookBackend{s: s, slug: slugName}
	for {
		entries, err := in.Outbox(ctx, cursor, notebookImportBatch)
		if err != nil {
			return res, notebookErr(err)
		}
		if len(entries) == 0 {
			break
		}
		var stored int64
		previous := cursor
		for _, entry := range entries {
			if entry.Seq <= previous {
				return res, fmt.Errorf("%w: notebook outbox is not ordered", ErrUnavailable)
			}
			previous = entry.Seq
			files := map[string]bool{}
			hashes := map[string]bool{}
			for _, f := range entry.Files {
				if err := validateNotebookFile(f); err != nil {
					return res, fmt.Errorf("%w: %v", ErrUnavailable, err)
				}
				if files[f.Path] {
					return res, fmt.Errorf("%w: duplicate notebook path", ErrUnavailable)
				}
				files[f.Path] = true
				hashes[f.Hash] = true
			}
			if !files["notebook.json"] {
				return res, fmt.Errorf("%w: missing notebook header", ErrUnavailable)
			}
			for _, hash := range entry.NewHashes {
				if !hashes[hash] {
					return res, fmt.Errorf("%w: unreferenced notebook snapshot", ErrUnavailable)
				}
				data, err := in.SnapshotData(ctx, hash)
				if err != nil {
					return res, notebookErr(err)
				}
				added, err := backend.PutBlob(ctx, hash, data)
				if err != nil {
					return res, err
				}
				if added {
					stored += int64(len(data))
				}
			}
			for _, f := range entry.Files {
				data, err := backend.ReadBlob(ctx, f.Hash)
				if err != nil {
					return res, fmt.Errorf("%w: notebook file %s: %v", ErrUnavailable, f.Path, err)
				}
				if int64(len(data)) != f.Size || hashBytes(data) != f.Hash {
					return res, fmt.Errorf("%w: notebook file %s does not match its blob", ErrUnavailable, f.Path)
				}
			}
		}
		if err := s.commitNotebookEntries(ctx, slugName, outbox, entries, &res); err != nil {
			return res, err
		}
		cursor = entries[len(entries)-1].Seq
		if err := in.Acknowledge(ctx, cursor, stored); err != nil {
			return res, notebookErr(err)
		}
	}
	if len(res.revisions) > 0 {
		s.notebookStats.observeImport(time.Since(start))
		_ = s.st.Touch(ctx, slugName, s.now())
	}
	s.settleNotebookHoldsLocked(ctx, slugName, in)
	s.notebookRetry.Delete(slugName)
	return res, nil
}

func validateNotebookFile(f notebook.File) error {
	if !validBlobHash(f.Hash) || f.Size < 0 {
		return fmt.Errorf("invalid notebook blob")
	}
	p := strings.Split(f.Path, "/")
	validID := func(id string, kind notebook.Kind) bool { return notebook.ID(id).ValidateKind(kind) == nil }
	bucket := func(s string) bool {
		if len(s) != 7 || !strings.HasSuffix(s, ".json") {
			return false
		}
		return strings.ContainsRune("0123456789abcdef", rune(s[0])) && strings.ContainsRune("0123456789abcdef", rune(s[1]))
	}
	if f.Path == "notebook.json" {
		return nil
	}
	if len(p) == 2 && p[0] == "assets" {
		dot := strings.LastIndexByte(p[1], '.')
		if dot == 64 && p[1][:dot] == f.Hash {
			switch p[1][dot+1:] {
			case "png", "jpg", "gif", "webp":
				return nil
			}
		}
	}
	if len(p) >= 3 && p[0] == "pages" && validID(p[1], notebook.PageKind) {
		if len(p) == 3 && p[2] == "page.json" {
			return nil
		}
		if len(p) == 4 && p[2] == "blocks" && bucket(p[3]) {
			return nil
		}
		if len(p) >= 5 && p[2] == "databases" && validID(p[3], notebook.DatabaseKind) {
			if len(p) == 5 && p[4] == "schema.json" {
				return nil
			}
			if len(p) == 6 && p[4] == "rows" && bucket(p[5]) {
				return nil
			}
		}
	}
	return fmt.Errorf("invalid notebook content path %q", f.Path)
}

func notebookFiles(files []store.RevisionFile) []notebook.File {
	out := make([]notebook.File, len(files))
	for i, f := range files {
		out[i] = notebook.File{Path: f.Path, Hash: f.Hash, Size: f.Size}
	}
	return out
}

func (s *Service) notebookPageSummaries(ctx context.Context, slugName string, e notebook.OutboxEntry, prev []store.RevisionFile) ([]store.NotebookPageChange, error) {
	read := func(ctx context.Context, hash string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return readBlob(s.docsStoreDir(slugName), hash)
	}
	previous := notebookFiles(prev)
	changes, err := notebook.PageChanges(e, previous, read)
	if err != nil {
		return nil, err
	}
	headers := func(files []notebook.File) map[string]string {
		out := map[string]string{}
		for _, f := range files {
			if strings.HasSuffix(f.Path, "/page.json") {
				out[f.Path] = f.Hash
			}
		}
		return out
	}
	beforeHeaders, afterHeaders := headers(previous), headers(e.Files)
	title := func(hashes map[string]string, id notebook.ID) (string, error) {
		hash, ok := hashes["pages/"+string(id)+"/page.json"]
		if !ok {
			return "", nil
		}
		data, err := read(ctx, hash)
		if err != nil {
			return "", err
		}
		var header struct {
			Title string `json:"title"`
		}
		err = json.Unmarshal(data, &header)
		return header.Title, err
	}
	out := make([]store.NotebookPageChange, 0, len(changes))
	for _, c := range changes {
		before, err := title(beforeHeaders, c.Page)
		if err != nil {
			return nil, err
		}
		after, err := title(afterHeaders, c.Page)
		if err != nil {
			return nil, err
		}
		out = append(out, store.NotebookPageChange{Page: string(c.Page), Change: c.Change, TitleBefore: before, TitleAfter: after})
	}
	return out, nil
}

func (s *Service) commitNotebookEntries(ctx context.Context, slugName, outbox string, entries []notebook.OutboxEntry, res *notebookImport) error {
	var prev []store.RevisionFile
	revs, err := s.st.ListRevisions(ctx, slugName, 0, 1)
	if err != nil {
		return err
	}
	if len(revs) > 0 {
		prev, err = s.st.RevisionFiles(ctx, slugName, revs[0].Number)
		if err != nil {
			return err
		}
	}
	type prepared struct {
		files  []store.RevisionFile
		pages  []store.NotebookPageChange
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
		p.pages, err = s.notebookPageSummaries(ctx, slugName, e, prev)
		if err != nil {
			return err
		}
		p.change = diffFiles(prev, p.files)
		preps[i], prev = p, p.files
	}
	holds, err := s.st.Holds(ctx, slugName)
	if err != nil {
		return err
	}
	var imported []int
	bySeq := map[int64]int{}
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
			r := store.Revision{Number: latest, CreatedAt: time.Unix(e.CreatedAt, 0), Kind: kind, Authors: dedupe(e.Authors), Message: truncate(e.Message, 500), RollbackOf: e.RollbackOf, Changes: p.change, Files: len(p.files), Size: p.size}
			if err := tx.InsertRevision(ctx, r, p.files); err != nil {
				return err
			}
			if err := tx.PutNotebookRevPages(ctx, latest, p.pages); err != nil {
				return err
			}
			if err := tx.PutNotebookImport(ctx, outbox, e.Seq, e.OpID, latest); err != nil {
				return err
			}
			if e.Kind == "rollback" {
				if e.OpID != "" {
					if err := tx.DeleteHold(ctx, e.OpID); err != nil {
						return err
					}
				}
				for _, h := range holds {
					if notebookRollbackHoldMatches(h.Op, outbox, e.Seq) && h.Number == e.RollbackOf {
						if err := tx.DeleteHold(ctx, h.Op); err != nil {
							return err
						}
					}
				}
			}
			imported = append(imported, latest)
			bySeq[e.Seq] = latest
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
	for seq, n := range bySeq {
		res.bySeq[seq] = n
	}
	if deleted > 0 {
		go s.sweepBlobs(context.WithoutCancel(ctx), slugName, contenttype.Notebook)
	}
	return nil
}

// The history seam chooses its own op id. A durable pre-call outbox boundary
// ties the core hold to the first matching rollback even across a crash.
func notebookRollbackHoldMatches(op, outbox string, seq int64) bool {
	prefix := "notebook-rollback:" + outbox + ":"
	if !strings.HasPrefix(op, prefix) {
		return false
	}
	boundary, err := strconv.ParseInt(strings.TrimPrefix(op, prefix), 10, 64)
	return err == nil && seq > boundary
}

func (s *Service) settleNotebookHoldsLocked(ctx context.Context, slugName string, in *notebook.Instance) {
	holds, err := s.st.Holds(ctx, slugName)
	if err != nil {
		return
	}
	for _, h := range holds {
		committed := false
		err := in.ReadTx(ctx, func(tx *sql.Tx, _ notebook.Sequence) error {
			if strings.HasPrefix(h.Op, "notebook-rollback:"+in.StoreID()+":") {
				rows, err := tx.QueryContext(ctx, `SELECT seq FROM outbox WHERE kind='rollback' AND rollback_target=?`, h.Number)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var seq int64
					if err := rows.Scan(&seq); err != nil {
						return err
					}
					committed = committed || notebookRollbackHoldMatches(h.Op, in.StoreID(), seq)
				}
				return rows.Err()
			}
			var count int
			err := tx.QueryRowContext(ctx, `SELECT count(*) FROM commands WHERE op_id=?`, h.Op).Scan(&count)
			committed = count > 0
			return err
		})
		if err != nil || committed {
			continue
		}
		_ = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.DeleteHold(ctx, h.Op) })
	}
}

func (s *Service) prepareNotebookSweepLocked(ctx context.Context, slugName string) (*notebook.Instance, []string, bool) {
	in := s.liveNotebook(slugName)
	if in == nil {
		return nil, nil, false
	}
	if _, err := s.importNotebookLocked(ctx, slugName, in); err != nil {
		return nil, nil, false
	}
	retained, err := s.st.ReferencedHashes(ctx, slugName)
	if err != nil {
		return nil, nil, false
	}
	cutoff := time.Now().Add(-blobGrace)
	root := s.docsStoreDir(slugName)
	hashes, err := in.SweepReferences(ctx, func(hash string) (bool, error) {
		if retained[hash] || s.leased(slugName, hash) {
			return true, nil
		}
		p, ok := blobPath(root, hash)
		if !ok {
			return false, fmt.Errorf("invalid notebook asset hash")
		}
		info, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return info.ModTime().After(cutoff), nil
	})
	return in, hashes, err == nil
}

func (s *Service) catchUpNotebook(ctx context.Context, slugName string) error {
	unlock, err := s.lockStorageCtx(ctx, slugName)
	if err != nil {
		return err
	}
	defer unlock()
	if in := s.liveNotebook(slugName); in != nil {
		if _, err := s.importNotebookLocked(ctx, slugName, in); err != nil {
			s.logf("notebook revisions of %s: %v", slugName, err)
		}
	}
	return nil
}

// TickNotebooks catches up available notebooks without blocking on a busy flat.
func (s *Service) TickNotebooks(ctx context.Context) {
	now := time.Now()
	s.notebookPending.Range(func(k, _ any) bool {
		slugName := k.(string)
		in := s.liveNotebook(slugName)
		if in == nil {
			s.notebookPending.Delete(slugName)
			s.notebookRetry.Delete(slugName)
			return true
		}
		var backoff notebookBackoff
		if v, ok := s.notebookRetry.Load(slugName); ok {
			backoff = v.(notebookBackoff)
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
		in = s.liveNotebook(slugName)
		if in == nil {
			return true
		}
		s.notebookPending.Delete(slugName)
		ictx, cancel := context.WithTimeout(ctx, notebookTickTimeout)
		_, err := s.importNotebookLocked(ictx, slugName, in)
		cancel()
		if err != nil {
			s.notebookPending.Store(slugName, true)
			backoff.delay = min(max(2*backoff.delay, 5*time.Second), 5*time.Minute)
			backoff.next = time.Now().Add(backoff.delay)
			s.notebookRetry.Store(slugName, backoff)
			s.logf("notebook revisions of %s (next try in %s): %v", slugName, backoff.delay, err)
		}
		return true
	})
}

func (s *Service) remeasureNotebooks() {
	s.mu.Lock()
	var ins []*notebook.Instance
	for _, lf := range s.live {
		if d := lf.cur.Load(); d != nil {
			if in, ok := d.inst.(*notebook.Instance); ok {
				ins = append(ins, in)
			}
		}
	}
	s.mu.Unlock()
	for _, in := range ins {
		if err := in.Remeasure(context.Background()); err != nil {
			s.logf("remeasure notebook: %v", err)
		}
	}
}
