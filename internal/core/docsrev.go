package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/store"
)

// Document revisions (docs/revision-store.md, docs/internal/docs-app.md): the docs worker
// records each revision boundary in an outbox, in the same transaction as
// the change; core imports it into the revision store shared with designs.

// ErrNoRevisions refuses a revision tool on a flat without revisions.
var ErrNoRevisions = fmt.Errorf("%w: flat has no revisions: only documents and designs do", ErrInvalid)

const (
	// docsTick is how often each running document syncs and imports.
	docsTick = 15 * time.Second
	// maxRevisionDocuments is the most documents a restored revision may
	// hold: the docs app's stored-document limit.
	maxRevisionDocuments = 128
	// restoreWindow is how long after it is sent a restore may commit.
	restoreWindow = 12 * time.Second
	// docsBudgetTTL is how long a docs worker relies on a disk budget
	// without a refresh: several ticks, so a skipped tick does not matter.
	docsBudgetTTL = 2 * time.Minute
	// maxStoredReport bounds the blob hashes one budget reports stored.
	maxStoredReport = 10_000
	// walTruncateAt is the write-ahead log size above which a budget
	// measurement first truncates it.
	walTruncateAt = 256 << 10
)

// docsImportBudget bounds the text one import fetches; the rest follows in
// the next call. Tests lower it.
var docsImportBudget = 64 << 20

var errNotRunningDocs = fmt.Errorf("%w: the document is not running", ErrNotDeployed)

// docsHost calls one host operation of the running docs worker of slug,
// directly on its instance (not through the flat's routes). The caller holds
// the flat lock.
func (s *Service) docsHost(ctx context.Context, slugName, method, op string, body, out any) error {
	d := s.state(slugName).cur.Load()
	if d == nil || d.inst == nil || contenttype.FromManifest(d.version.Manifest) != contenttype.Docs {
		return errNotRunningDocs
	}
	var r io.Reader
	if body != nil {
		// No HTML escaping: a staged text full of < would otherwise grow
		// six-fold on the way.
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(body); err != nil {
			return err
		}
		r = &buf
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, method, "http://docs.internal/_docs/host/revisions/"+op, r)
	req = trustedAccess(req, false)
	req.Header.Set("X-Flats-Host-Op", "revisions")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	d.inst.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		var refusal struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
		msg := truncate(refusal.Message, 300)
		switch rec.Code {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", store.ErrNotFound, msg)
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", ErrConflict, msg)
		case http.StatusBadRequest:
			return invalidf("%s", msg)
		case http.StatusUnprocessableEntity:
			return fmt.Errorf("%w: %s", ErrDocumentCapacity, msg)
		}
		return fmt.Errorf("%w: docs revision %s returned HTTP %d %s", ErrUnavailable, op, rec.Code, msg)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		return fmt.Errorf("%w: docs revision %s: %v", ErrUnavailable, op, err)
	}
	return nil
}

func newOpID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// --- sources ---

// registerDocsSource stores the non-document files of a docs version as
// blobs and records them as its source, before its worker can name it.
func (s *Service) registerDocsSource(ctx context.Context, slugName string, v store.Version, dir string) error {
	if _, ok, err := s.st.SourceFiles(ctx, slugName, v.Hash); err != nil || ok {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	var files []store.RevisionFile
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || bundle.IsMarkdown(p) {
			return err
		}
		b, err := fs.ReadFile(root.FS(), p)
		if err != nil {
			return err
		}
		h, err := putBlob(s.docsStoreDir(slugName), b)
		if err != nil {
			return err
		}
		files = append(files, store.RevisionFile{Path: p, Hash: h, Size: int64(len(b))})
		return nil
	})
	if err != nil {
		return err
	}
	return s.st.PutSource(ctx, slugName, v.Hash, files)
}

// docsSource returns the non-document files of source, rebuilding its rows
// from a stored version with that content hash when they are missing.
func (s *Service) docsSource(ctx context.Context, slugName, source string) ([]store.RevisionFile, error) {
	files, ok, err := s.st.SourceFiles(ctx, slugName, source)
	if err != nil || ok {
		return files, err
	}
	vs, err := s.st.ListVersions(ctx, slugName)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		if v.Hash != source || v.Pruned {
			continue
		}
		if err := s.registerDocsSource(ctx, slugName, v, s.versionDir(slugName, v.Number)); err != nil {
			return nil, err
		}
		files, _, err = s.st.SourceFiles(ctx, slugName, source)
		return files, err
	}
	if d, err := s.st.GetDraft(ctx, slugName); err == nil && d.Hash == source {
		v := store.Version{Revision: d.Revision, Role: "draft", Hash: d.Hash}
		if err := s.registerDocsSource(ctx, slugName, v, s.contentDir(slugName, v)); err != nil {
			return nil, err
		}
		files, _, err = s.st.SourceFiles(ctx, slugName, source)
		return files, err
	}
	return nil, fmt.Errorf("%w: unknown content source %.12s", store.ErrNotFound, source)
}

// revisionFiles returns every file of a revision: a document's revision
// stores its documents and names the rest by its source.
func (s *Service) revisionFiles(ctx context.Context, slugName string, r store.Revision) ([]store.RevisionFile, error) {
	files, err := s.st.RevisionFiles(ctx, slugName, r.Number)
	if err != nil || r.Source == "" {
		return files, err
	}
	src, err := s.docsSource(ctx, slugName, r.Source)
	if err != nil {
		return nil, err
	}
	files = append(files, src...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// --- sync and import ---

type docsSyncResult struct {
	Store  string  `json:"store"`
	Ready  int     `json:"ready"`
	Intent *string `json:"intent"`
}

type docsEntry struct {
	Seq        int64                `json:"seq"`
	CreatedAt  int64                `json:"created_at"`
	Kind       string               `json:"kind"`
	Authors    []string             `json:"authors"`
	Message    string               `json:"message"`
	RollbackOf int                  `json:"rollback_of"`
	Op         string               `json:"op"`
	Source     string               `json:"source"`
	Documents  []store.RevisionFile `json:"documents"`
}

type docsOutbox struct {
	Store   string      `json:"store"`
	Entries []docsEntry `json:"entries"`
	More    bool        `json:"more"`
}

// docsImport reports one import.
type docsImport struct {
	revisions []int          // imported revision numbers, in order
	byOp      map[string]int // revision of each entry with an op
	more      bool           // entries remain in the outbox
	// stored lists the texts of entries the import read whose blobs are
	// stored, acknowledged or not: a partial import stores some blobs of an
	// entry it cannot finish yet. The next budget reports them, so the worker
	// stops reserving what the measured flat directory already holds.
	stored map[string]bool
}

func (s *Service) syncDocsLocked(ctx context.Context, slugName string) (docsSyncResult, error) {
	var out docsSyncResult
	err := s.docsHost(ctx, slugName, http.MethodPost, "sync", map[string]any{}, &out)
	return out, err
}

// importDocsLocked imports the worker's outbox into the revision store,
// within docsImportBudget of fetched text, and acknowledges it. The caller
// holds the flat lock.
func (s *Service) importDocsLocked(ctx context.Context, slugName string) (docsImport, error) {
	res := docsImport{byOp: map[string]int{}, stored: map[string]bool{}}
	st, err := s.st.ContentState(ctx, slugName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return res, err
	}
	if err == nil && st.Type != contenttype.Docs {
		return res, ErrNoRevisions
	}
	// Repair an acknowledgement a crash lost: the cursor is durable.
	if st.ImportStore != "" && st.ImportSeq > 0 {
		if err := s.docsHost(ctx, slugName, http.MethodPost, "ack", map[string]any{"store": st.ImportStore, "through": st.ImportSeq, "budget": s.docsBudget(ctx, slugName, nil)}, nil); err != nil && !errors.Is(err, ErrConflict) {
			return res, err
		}
	}
	budget := int64(docsImportBudget)
	root := s.docsStoreDir(slugName)
	after := st.ImportSeq
	for {
		var page docsOutbox
		if err := s.docsHost(ctx, slugName, http.MethodGet, fmt.Sprintf("outbox?after=%d", after), nil, &page); err != nil {
			return res, err
		}
		if page.Store == "" {
			break
		}
		if page.Store != st.ImportStore && after != 0 {
			// Another worker database: start from its beginning.
			after = 0
			continue
		}
		if len(page.Entries) == 0 {
			break
		}
		// Fetch what the entries need, oldest first, within the budget.
		sources := map[string][]store.RevisionFile{}
		var ready []docsEntry
		stop := false
		for _, e := range page.Entries {
			for _, d := range e.Documents {
				if len(d.Hash) != 64 {
					return res, fmt.Errorf("%w: invalid revision entry", ErrUnavailable)
				}
				if _, err := os.Stat(blobPath(root, d.Hash)); err == nil {
					res.stored[d.Hash] = true
					continue
				}
				if budget <= 0 {
					stop = true
					break
				}
				var t struct {
					Text string `json:"text"`
				}
				if err := s.docsHost(ctx, slugName, http.MethodGet, "text?hash="+url.QueryEscape(d.Hash), nil, &t); err != nil {
					return res, err
				}
				if hashBytes([]byte(t.Text)) != d.Hash {
					return res, fmt.Errorf("%w: revision text of %s does not match its hash", ErrUnavailable, d.Path)
				}
				if _, err := putBlob(root, []byte(t.Text)); err != nil {
					return res, err
				}
				res.stored[d.Hash] = true
				budget -= int64(len(t.Text))
			}
			if stop {
				break
			}
			if _, ok := sources[e.Source]; !ok {
				src, err := s.docsSource(ctx, slugName, e.Source)
				if err != nil {
					s.Event(ctx, slugName, "error", "revision", "cannot import a revision: "+err.Error(), nil)
					stop = true
					break
				}
				sources[e.Source] = src
			}
			ready = append(ready, e)
		}
		if len(ready) > 0 {
			if err := s.commitDocsEntries(ctx, slugName, page.Store, ready, sources, &res); err != nil {
				return res, err
			}
			last := ready[len(ready)-1].Seq
			st.ImportStore, st.ImportSeq, after = page.Store, last, last
			if err := s.docsHost(ctx, slugName, http.MethodPost, "ack", map[string]any{"store": page.Store, "through": last, "budget": s.docsBudget(ctx, slugName, res.stored)}, nil); err != nil {
				s.logf("acknowledge revisions of %s: %v", slugName, err)
			}
		}
		if stop || len(ready) < len(page.Entries) {
			res.more = true
			break
		}
		if !page.More {
			break
		}
	}
	if len(res.revisions) > 0 {
		if deleted, err := s.retainLocked(ctx, slugName); err != nil {
			s.logf("retain revisions of %s: %v", slugName, err)
		} else if deleted > 0 {
			go s.sweepBlobs(context.WithoutCancel(ctx), slugName, contenttype.Docs)
		}
	}
	return res, nil
}

// commitDocsEntries inserts imported entries as revisions in one
// transaction, skipping entries the cursor already passed. sources holds
// the files of every source the entries name.
func (s *Service) commitDocsEntries(ctx context.Context, slugName, outbox string, entries []docsEntry, sources map[string][]store.RevisionFile, res *docsImport) error {
	// The previous revision, read before the transaction: the store has
	// one connection, and the flat lock keeps it current.
	var prev []store.RevisionFile
	if revs, err := s.st.ListRevisions(ctx, slugName, 0, 1); err != nil {
		return err
	} else if len(revs) == 1 {
		if prev, err = s.revisionFiles(ctx, slugName, revs[0]); err != nil {
			return err
		}
	}
	var imported []int
	byOp := map[string]int{}
	err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		st, err := tx.State(ctx)
		if errors.Is(err, store.ErrNotFound) {
			if err := tx.InitReadyState(ctx, contenttype.Docs); err != nil {
				return err
			}
			st, err = tx.State(ctx)
		}
		if err != nil {
			return err
		}
		latest, err := tx.LatestRevision(ctx)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if st.ImportStore == outbox && e.Seq <= st.ImportSeq {
				continue
			}
			docs := slices.Clone(e.Documents)
			sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
			all := append(slices.Clone(docs), sources[e.Source]...)
			sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
			var size int64
			for _, f := range all {
				size += f.Size
			}
			latest++
			r := store.Revision{
				Number: latest, CreatedAt: time.UnixMilli(e.CreatedAt), Kind: e.Kind, Authors: dedupe(e.Authors),
				Message: truncate(e.Message, 500), RollbackOf: e.RollbackOf, Changes: diffFiles(prev, all),
				Files: len(all), Size: size, Source: e.Source,
			}
			if r.Kind == "" {
				r.Kind = "person"
			}
			if err := tx.InsertRevision(ctx, r, docs); err != nil {
				return err
			}
			if e.Op != "" {
				if err := tx.DeleteHold(ctx, e.Op); err != nil {
					return err
				}
				byOp[e.Op] = latest
			}
			imported = append(imported, latest)
			prev = all
			st.ImportStore, st.ImportSeq = outbox, e.Seq
		}
		return tx.SetImport(ctx, st.ImportStore, st.ImportSeq)
	})
	if err != nil {
		return err
	}
	res.revisions = append(res.revisions, imported...)
	for k, v := range byOp {
		res.byOp[k] = v
	}
	return nil
}

// refreshDocsLocked syncs a running document, imports its outbox, settles
// the holds of rollbacks that did not happen and then tells the worker its
// disk budget, measured with the imported blobs in place. Failures are
// returned after the import still ran, so a failing activation does not
// stop the backlog from draining.
func (s *Service) refreshDocsLocked(ctx context.Context, slugName string) (docsImport, error) {
	res, err := s.syncImportDocsLocked(ctx, slugName)
	if !errors.Is(err, errNotRunningDocs) {
		if berr := s.pushDocsBudgetStoredLocked(ctx, slugName, res.stored); berr != nil && !errors.Is(berr, errNotRunningDocs) {
			s.logf("disk budget of %s: %v", slugName, berr)
		}
	}
	return res, err
}

func (s *Service) syncImportDocsLocked(ctx context.Context, slugName string) (docsImport, error) {
	sync, syncErr := s.syncDocsLocked(ctx, slugName)
	if errors.Is(syncErr, errNotRunningDocs) {
		return docsImport{}, syncErr
	}
	if syncErr == nil && sync.Ready == 0 {
		// Nothing to import or acknowledge: ready counts every entry the
		// worker still holds.
		s.settleHoldsLocked(ctx, slugName, intentID(sync))
		return docsImport{byOp: map[string]int{}}, nil
	}
	res, err := s.importDocsLocked(ctx, slugName)
	if err != nil {
		return res, err
	}
	if syncErr == nil {
		s.settleHoldsLocked(ctx, slugName, intentID(sync))
	}
	return res, syncErr
}

// docsBudget is the disk budget of a running document, measured now: the
// per-flat disk quota (0 = off), how much of it the flat's directory uses
// apart from the worker's database file, and how long the worker may rely
// on it without a refresh. The worker checks people's edits and live agent
// edits against it in the transaction that would commit them, counting its
// database as it is then and the revisions still to be stored; it admits no
// live edit while its budget is missing or expired
// (docs/revision-store.md#disk-quota). stored lists texts whose blobs the
// measurement includes, so the worker stops reserving them. The caller
// holds the flat lock.
func (s *Service) docsBudget(ctx context.Context, slugName string, stored map[string]bool) map[string]any {
	quota, other := s.diskQuota(), int64(0)
	if quota > 0 {
		dbPath := filepath.Join(s.dataDirOf(slugName), dbName)
		// The worker truncates its write-ahead log after every commit that
		// leaves it above 64 frames; a reader can keep that truncation from
		// finishing, so truncate a log above walTruncateAt here too. A busy
		// checkpoint leaves it for the next measurement.
		if fi, err := os.Stat(dbPath + "-wal"); err == nil && fi.Size() > walTruncateAt {
			if err := s.docsHost(ctx, slugName, http.MethodPost, "checkpoint", map[string]any{}, nil); err != nil {
				s.logf("checkpoint the document database of %s: %v", slugName, err)
			}
		}
		// The worker counts its database as it is and its write-ahead log
		// as the log's bound at rest, so other leaves both out. They are
		// skipped in the same walk: a log the worker truncates between two
		// separate measurements would otherwise understate other.
		other = dirSizeExcept(s.flatDir(slugName), dbPath, dbPath+"-wal")
	} else {
		quota = 0
	}
	// Hashes past the bound stay reserved until their entries are
	// acknowledged: conservative, never undercounted.
	hashes := make([]string, 0, min(len(stored), maxStoredReport))
	for h := range stored {
		if len(hashes) == maxStoredReport {
			break
		}
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	return map[string]any{"quota": quota, "other": other, "ttl_ms": docsBudgetTTL.Milliseconds(), "stored": hashes}
}

// pushDocsBudgetLocked installs a freshly measured budget in the running
// docs worker. The caller holds the flat lock.
func (s *Service) pushDocsBudgetLocked(ctx context.Context, slugName string) error {
	return s.pushDocsBudgetStoredLocked(ctx, slugName, nil)
}

func (s *Service) pushDocsBudgetStoredLocked(ctx context.Context, slugName string, stored map[string]bool) error {
	return s.docsHost(ctx, slugName, http.MethodPost, "budget", s.docsBudget(ctx, slugName, stored), nil)
}

// pushAllDocsBudgets applies a changed disk quota to every running
// document, each under its flat lock, before the settings call returns,
// even when the caller has gone. A document it cannot reach keeps its
// previous budget until that expires (docsBudgetTTL) and then admits no
// live edit until a later refresh succeeds; each failure is a warn event of
// that flat.
func (s *Service) pushAllDocsBudgets(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	for _, slugName := range s.runningDocs() {
		unlock, err := s.lockStorageCtx(ctx, slugName)
		if err == nil {
			err = s.pushDocsBudgetLocked(ctx, slugName)
			unlock()
		}
		if err != nil && !errors.Is(err, errNotRunningDocs) {
			s.Event(ctx, slugName, "warn", "quota", "the changed disk quota could not be applied to the running document yet: "+err.Error()+"; live edits are refused once its previous budget expires, until a refresh succeeds", nil)
		}
	}
}

func intentID(r docsSyncResult) string {
	if r.Intent == nil {
		return ""
	}
	return *r.Intent
}

// settleHoldsLocked releases the holds of rollbacks that did not commit.
// A committed rollback keeps its receipt in the worker until its entry is
// imported, which deletes the hold, so "not committed" is definitive.
func (s *Service) settleHoldsLocked(ctx context.Context, slugName, intent string) {
	holds, err := s.st.Holds(ctx, slugName)
	if err != nil {
		return
	}
	for _, h := range holds {
		if _, running := s.docsRollbacks.Load(h.Op); h.Op == intent || running {
			continue
		}
		var status struct {
			Committed bool `json:"committed"`
		}
		if err := s.docsHost(ctx, slugName, http.MethodGet, "op?id="+url.QueryEscape(h.Op), nil, &status); err != nil || status.Committed {
			continue
		}
		_ = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.DeleteHold(ctx, h.Op) })
	}
}

// --- writes ---

// docsIntentLocked tells the running docs worker which agent save the next
// version is, so its activation records it with the agent's label. It
// does nothing when no document runs or the save starts no new version.
func (s *Service) docsIntentLocked(ctx context.Context, slugName, source, label, message string) {
	d := s.state(slugName).cur.Load()
	if d == nil || contenttype.FromManifest(d.version.Manifest) != contenttype.Docs || d.version.Hash == source {
		return
	}
	body := map[string]any{"id": newOpID(), "source": source, "kind": "agent", "label": label, "message": message}
	if err := s.docsHost(ctx, slugName, http.MethodPost, "intent", body, nil); err != nil {
		s.logf("revision intent of %s: %v", slugName, err)
	}
}

// docsAfterWriteLocked imports what a document write recorded. A failure
// is logged: the write itself succeeded, and the ticker imports later.
func (s *Service) docsAfterWriteLocked(ctx context.Context, slugName string) docsImport {
	res, err := s.refreshDocsLocked(ctx, slugName)
	if err != nil {
		s.Event(ctx, slugName, "warn", "revision", "could not record the revision yet: "+err.Error(), nil)
	}
	return res
}

// --- ticker ---

// TickDocs syncs and imports every running document whose lock is free.
func (s *Service) TickDocs(ctx context.Context) {
	for _, slugName := range s.runningDocs() {
		m, _ := s.locks.LoadOrStore(slugName, &sync.Mutex{})
		mu := m.(*sync.Mutex)
		if !mu.TryLock() {
			continue
		}
		if err := s.checkStorageJournal(slugName); err == nil {
			if _, err := s.refreshDocsLocked(ctx, slugName); err != nil && !errors.Is(err, errNotRunningDocs) {
				s.logf("document revisions of %s: %v", slugName, err)
			}
		}
		mu.Unlock()
	}
}

// runningDocs lists the flats whose live version is a document.
func (s *Service) runningDocs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for slugName, lf := range s.live {
		if d := lf.cur.Load(); d != nil && contenttype.FromManifest(d.version.Manifest) == contenttype.Docs {
			out = append(out, slugName)
		}
	}
	sort.Strings(out)
	return out
}

// startDocsRevisions syncs, migrates and imports every running document
// once after startup, then applies retention to it.
func (s *Service) startDocsRevisions(ctx context.Context) {
	for _, slugName := range s.runningDocs() {
		unlock, err := s.lockStorage(slugName)
		if err != nil {
			continue
		}
		if _, err := s.refreshDocsLocked(ctx, slugName); err != nil && !errors.Is(err, errNotRunningDocs) {
			s.logf("document revisions of %s: %v", slugName, err)
		}
		unlock()
		s.retainRevisions(ctx, slugName, contenttype.Docs)
	}
}

// prepareDocsSweepLocked imports a document before its blobs are swept and
// drops sources nothing needs. It reports false when the sweep must wait:
// an entry still in the worker may name blobs no revision references yet.
func (s *Service) prepareDocsSweepLocked(ctx context.Context, slugName string) bool {
	if d := s.state(slugName).cur.Load(); d != nil && contenttype.FromManifest(d.version.Manifest) == contenttype.Docs {
		if res, err := s.importDocsLocked(ctx, slugName); err != nil || res.more {
			return false
		}
	} else {
		// A document that is not running cannot hand over its outbox.
		return false
	}
	keep := map[string]bool{}
	if d := s.state(slugName).cur.Load(); d != nil {
		keep[d.version.Hash] = true
	}
	if vs, err := s.st.ListVersions(ctx, slugName); err == nil {
		for _, v := range vs {
			if !v.Pruned {
				keep[v.Hash] = true
			}
		}
	} else {
		return false
	}
	if dr, err := s.st.GetDraft(ctx, slugName); err == nil {
		keep[dr.Hash] = true
	}
	if err := s.st.DeleteUnusedSources(ctx, slugName, keep); err != nil {
		return false
	}
	return true
}

// --- rollback ---

// rollbackDocs restores revision n of a document as a new revision.
func (s *Service) rollbackDocs(ctx context.Context, slugName string, n int, label string, via Via) (int, error) {
	unlock, err := s.lockStorageCtx(ctx, slugName)
	if err != nil {
		return 0, err
	}
	defer unlock()
	// Hold n before anything can run retention; only a revision that does
	// not exist yet needs an import first.
	op := newOpID()
	s.docsRollbacks.Store(op, true)
	held := false
	defer func() {
		// The hold settles like any other once this call is over: imported
		// with the rollback, or released when it did not commit.
		s.docsRollbacks.Delete(op)
		if !held {
			return
		}
		if _, err := s.refreshDocsLocked(context.WithoutCancel(ctx), slugName); err != nil && !errors.Is(err, errNotRunningDocs) {
			s.logf("document revisions of %s: %v", slugName, err)
		}
	}()
	hold := func() error {
		return s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
			if _, err := tx.Revision(ctx, n); err != nil {
				return err
			}
			return tx.AddHold(ctx, op, n)
		})
	}
	// A version whose activation reaches a history limit fails its sync;
	// the import still ran, and the rollback below is how it recovers.
	refresh := func() error {
		_, err := s.refreshDocsLocked(ctx, slugName)
		if errors.Is(err, errNotRunningDocs) || errors.Is(err, ErrDocumentCapacity) {
			return nil
		}
		return err
	}
	if err := hold(); errors.Is(err, store.ErrNotFound) {
		if err := refresh(); err != nil {
			return 0, err
		}
		if err := hold(); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	held = true
	r, err := s.st.Revision(ctx, slugName, n)
	if err != nil {
		return 0, err
	}
	docs, err := s.st.RevisionFiles(ctx, slugName, n)
	if err != nil {
		return 0, err
	}
	if len(docs) > maxRevisionDocuments {
		return 0, invalidf("revision %d holds %d documents, more than the %d a document can run; read its texts with get_revision and save them instead", n, len(docs), maxRevisionDocuments)
	}
	if err := refresh(); err != nil {
		return 0, err
	}
	live := s.state(slugName).cur.Load()
	if live == nil || contenttype.FromManifest(live.version.Manifest) != contenttype.Docs {
		return 0, errNotRunningDocs
	}
	src, err := s.docsSource(ctx, slugName, r.Source)
	if err != nil {
		return 0, err
	}
	release := s.lease(slugName, hashesOf(docs, src))
	defer release()
	root := s.docsStoreDir(slugName)
	targets := make([]map[string]string, 0, len(docs))
	texts := map[string][]byte{}
	for _, d := range docs {
		b, err := readBlob(root, d.Hash)
		if err != nil {
			return 0, err
		}
		texts[d.Path] = b
		var staged struct {
			Hash string `json:"hash"`
		}
		if err := s.docsHost(ctx, slugName, http.MethodPost, "stage", map[string]any{"text": string(b)}, &staged); err != nil {
			return 0, err
		}
		if staged.Hash != d.Hash {
			return 0, fmt.Errorf("%w: staged text of %s has another hash", ErrUnavailable, d.Path)
		}
		targets = append(targets, map[string]string{"path": d.Path, "hash": d.Hash})
	}
	same, err := s.sameDocsShape(ctx, slugName, live.version, docs, src)
	if err != nil {
		return 0, err
	}
	if same {
		deadline := time.Now().Add(restoreWindow)
		body := map[string]any{"id": op, "label": label, "rollback_of": n, "documents": targets, "deadline_ms": deadline.UnixMilli()}
		if err := s.docsHost(ctx, slugName, http.MethodPost, "restore", body, nil); err != nil {
			if !errors.Is(err, ErrUnavailable) {
				return 0, err
			}
			// The response may be lost after COMMIT: ask once the restore
			// can no longer commit.
			time.Sleep(time.Until(deadline.Add(editSettleMargin)))
			var status struct {
				Committed bool `json:"committed"`
			}
			if serr := s.docsHost(ctx, slugName, http.MethodGet, "op?id="+op, nil, &status); serr != nil || !status.Committed {
				return 0, err
			}
		}
	} else {
		files := make([]bundle.File, 0, len(docs)+len(src))
		for _, d := range docs {
			files = append(files, bundle.File{Path: d.Path, Data: texts[d.Path]})
		}
		for _, f := range src {
			b, err := readBlob(root, f.Hash)
			if err != nil {
				return 0, err
			}
			files = append(files, bundle.File{Path: f.Path, Data: b})
		}
		// The bundle hash depends on file order: write them sorted, as
		// every other save does.
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		v, err := s.saveDraftLocked(ctx, slugName, files, SaveMeta{Message: fmt.Sprintf("restored revision %d", n), historical: true}, via)
		if err != nil {
			return 0, err
		}
		body := map[string]any{"id": op, "source": v.Hash, "kind": "rollback", "label": label, "rollback_of": n, "documents": targets, "message": fmt.Sprintf("restored revision %d", n)}
		if err := s.docsHost(ctx, slugName, http.MethodPost, "intent", body, nil); err != nil {
			return 0, err
		}
		if err := s.applyDocsLocked(ctx, slugName, v.Revision, via); err != nil {
			return 0, err
		}
	}
	res, err := s.refreshDocsLocked(ctx, slugName)
	if num, ok := res.byOp[op]; ok {
		s.Event(ctx, slugName, "info", "revision", fmt.Sprintf("restored revision %d as revision %d via %s", n, num, via), map[string]int{"revision": num, "rollback_of": n})
		return num, nil
	}
	if err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%w: the rollback is recorded but not imported yet; list_revisions shows it shortly", ErrUnavailable)
}

func hashesOf(lists ...[]store.RevisionFile) []string {
	var out []string
	for _, l := range lists {
		for _, f := range l {
			out = append(out, f.Hash)
		}
	}
	return out
}

// sameDocsShape reports whether a revision's documents and other files
// have the paths of the running version and the same non-document files,
// so a restore needs no new version.
func (s *Service) sameDocsShape(ctx context.Context, slugName string, v store.Version, docs, src []store.RevisionFile) (bool, error) {
	cur, err := s.docsSource(ctx, slugName, v.Hash)
	if err != nil {
		return false, err
	}
	if len(cur) != len(src) {
		return false, nil
	}
	for i := range cur {
		if cur[i].Path != src[i].Path || cur[i].Hash != src[i].Hash {
			return false, nil
		}
	}
	paths, err := s.versionDocuments(slugName, v)
	if err != nil {
		return false, err
	}
	if len(paths) != len(docs) {
		return false, nil
	}
	for i, d := range docs {
		if paths[i] != d.Path {
			return false, nil
		}
	}
	return true, nil
}

// versionDocuments lists the Markdown documents of a version, sorted.
func (s *Service) versionDocuments(slugName string, v store.Version) ([]string, error) {
	root, err := os.OpenRoot(s.contentDir(slugName, v))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var out []string
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && bundle.IsMarkdown(p) {
			out = append(out, p)
		}
		return err
	})
	sort.Strings(out)
	return out, err
}
