package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/design"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

// ErrDesignUnversioned refuses version, Draft, preview, publish and rollback
// operations on a design, and design manifests on the bundle paths.
var ErrDesignUnversioned = fmt.Errorf("%w: designs have no versions, drafts, previews or publishing; create one with create_flat {type: design}, write files with save_design, and use the revision tools for history", ErrInvalid)

// ErrNotDesign refuses a design or revision tool on a flat that is not a
// design.
var ErrNotDesign = fmt.Errorf("%w: flat is not a design", ErrInvalid)

// ErrDiskQuota refuses a change that would put a flat over its disk quota.
var ErrDiskQuota = errors.New("disk quota")

// Edit groups of a person close after this idle time or age.
const (
	groupIdle   = 30 * time.Second
	groupMaxAge = 5 * time.Minute
	blobGrace   = 10 * time.Minute
)

// DesignFile is one file written to a design.
type DesignFile struct {
	Path string
	Data []byte
}

// DesignWrite is one agent write: one revision.
type DesignWrite struct {
	Files            []DesignFile
	Delete           []string
	Message          string
	ExpectedRevision *int
	Name             string // flat name when the write creates the design
}

// DesignWriteResult reports an agent write.
type DesignWriteResult struct {
	Revision int      `json:"revision"`
	Changed  []string `json:"changed"`
	Created  bool     `json:"created"`
	Seq      int64    `json:"-"`
}

// designState is the in-memory bookkeeping of the design subsystem.
type designState struct {
	mu     sync.Mutex
	leases map[string]int // slug + "\x00" + hash -> readers
}

func (s *Service) designs() *designState {
	s.designOnce.Do(func() { s.design = &designState{leases: map[string]int{}} })
	return s.design
}

// --- blobs ---

func (s *Service) designDir(slugName string) string {
	return filepath.Join(s.flatDir(slugName), "design")
}

// docsStoreDir holds the revision blobs of a document.
func (s *Service) docsStoreDir(slugName string) string {
	return filepath.Join(s.flatDir(slugName), "revisions")
}

// storeDir is the blob store of a Revision-model flat of type typ.
func (s *Service) storeDir(slugName, typ string) string {
	if typ == contenttype.Docs {
		return s.docsStoreDir(slugName)
	}
	return s.designDir(slugName)
}

func blobPath(root, hash string) string {
	return filepath.Join(root, "blobs", hash[:2], hash)
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// putBlob stores data durably and returns its hash. An existing blob with
// the same hash is reused.
func putBlob(root string, data []byte) (string, error) {
	h := hashBytes(data)
	dst := blobPath(root, h)
	if _, err := os.Stat(dst); err == nil {
		return h, nil
	}
	tmpDir := filepath.Join(root, "tmp")
	if err := mkdirDurable(tmpDir); err != nil {
		return "", err
	}
	if err := mkdirDurable(filepath.Dir(dst)); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(tmpDir, "blob-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	_ = os.Chmod(name, 0o444)
	if err := os.Rename(name, dst); err != nil {
		os.Remove(name)
		return "", err
	}
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return h, nil
}

// mkdirDurable creates dir and its missing parents, syncing each parent
// after a new entry so the directory survives power loss.
func mkdirDurable(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := mkdirDurable(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if d, err := os.Open(parent); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func readBlob(root, hash string) ([]byte, error) {
	if len(hash) != 64 {
		return nil, fmt.Errorf("blob %q: %w", hash, store.ErrNotFound)
	}
	b, err := os.ReadFile(blobPath(root, hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("blob %s: %w", hash[:12], store.ErrNotFound)
	}
	return b, err
}

func (s *Service) lease(slugName string, hashes []string) func() {
	d := s.designs()
	d.mu.Lock()
	for _, h := range hashes {
		d.leases[slugName+"\x00"+h]++
	}
	d.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			for _, h := range hashes {
				k := slugName + "\x00" + h
				if d.leases[k]--; d.leases[k] <= 0 {
					delete(d.leases, k)
				}
			}
			d.mu.Unlock()
		})
	}
}

func (s *Service) leased(slugName, hash string) bool {
	d := s.designs()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leases[slugName+"\x00"+hash] > 0
}

// lockCtx takes the per-flat lock like s.lock, but gives up when ctx ends.
// Request paths use it: rename and delete hold the lock while they stop the
// design instance, which cancels its requests, so a request waiting here
// must not hold up that teardown.
func (s *Service) lockCtx(ctx context.Context, slugName string) (func(), error) {
	m, _ := s.locks.LoadOrStore(slugName, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	for {
		if mu.TryLock() {
			return mu.Unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// lockStorageCtx is lockCtx for an operation that writes or removes files
// below the flat's storage directory: like lockStorage, it refuses while an
// unfinished rename or delete journal names the slug.
func (s *Service) lockStorageCtx(ctx context.Context, slugName string) (func(), error) {
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return nil, err
	}
	if err := s.checkStorageJournal(slugName); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// --- type and access ---

// isDesign reports whether f is a design.
func (s *Service) isDesign(ctx context.Context, f store.Flat) (bool, error) {
	t, err := s.flatType(ctx, f)
	return t == contenttype.Design, err
}

// refuseUnversioned refuses Version-model operations on documents and
// designs.
func (s *Service) refuseUnversioned(ctx context.Context, f store.Flat) error {
	t, err := s.flatType(ctx, f)
	if err != nil {
		return err
	}
	switch t {
	case contenttype.Docs:
		return ErrDocsUnversioned
	case contenttype.Design:
		return ErrDesignUnversioned
	}
	return nil
}

func (s *Service) designFlat(ctx context.Context, slugName string) (store.Flat, store.ContentState, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return f, store.ContentState{}, err
	}
	st, err := s.st.ContentState(ctx, slugName)
	if errors.Is(err, store.ErrNotFound) || (err == nil && st.Type != contenttype.Design) {
		return f, st, fmt.Errorf("%w (%s)", ErrNotDesign, slugName)
	}
	return f, st, err
}

// revisionFlat returns a Revision-model flat (a design or a document with
// revisions) and its state.
func (s *Service) revisionFlat(ctx context.Context, slugName string) (store.Flat, store.ContentState, error) {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return f, store.ContentState{}, err
	}
	st, err := s.st.ContentState(ctx, slugName)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !contenttype.RevisionModel(st.Type)) {
		return f, st, fmt.Errorf("%w (%s)", ErrNoRevisions, slugName)
	}
	return f, st, err
}

// catchUpDocs imports a document's recorded revisions before a history
// read or change, so it sees them, and a document without history yet gets
// its first revision. It does nothing for other flats; a document that is
// not running keeps the history it has.
func (s *Service) catchUpDocs(ctx context.Context, slugName string) error {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return err
	}
	if docs, err := s.isDocs(ctx, f); err != nil || !docs {
		return err
	}
	// The page's history refreshes every few seconds; the ticker and every
	// write import too, so frequent reads need not each reach the worker.
	if last, ok := s.docsCatchUps.Load(slugName); ok && time.Since(last.(time.Time)) < 2*time.Second {
		return nil
	}
	unlock, err := s.lockStorageCtx(ctx, slugName)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.refreshDocsLocked(ctx, slugName); err != nil && !errors.Is(err, errNotRunningDocs) {
		s.logf("document revisions of %s: %v", slugName, err)
	}
	s.docsCatchUps.Store(slugName, time.Now())
	return nil
}

// --- creation ---

// starterDesign returns the files of a new design.
func starterDesign(title string) []DesignFile {
	esc := html.EscapeString(title)
	canvas := map[string]any{
		"title":  title,
		"boards": map[string]any{"main.html": map[string]any{"x": 0, "y": 0, "w": 1280, "h": 800, "title": "Main"}},
		"order":  []string{"main.html"},
		"notes": map[string]any{"start": map[string]any{"x": 0, "y": -170, "w": 520, "color": "yellow",
			"text": "A design is a canvas of HTML artboards. Agents write them with save_design (guide topic.canvas); people move, retype and tweak them here."}},
	}
	cj, _ := json.MarshalIndent(canvas, "", "  ")
	page := `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + esc + `</title>
<script type="application/json" data-flats-tweaks>
{
  "accent": {"type": "color", "label": "Accent", "value": "#2459d6"},
  "radius": {"type": "number", "label": "Corner radius", "value": 12, "min": 0, "max": 40, "unit": "px"}
}
</script>
<style>
:root { --accent: #2459d6; --radius: 12px; color-scheme: light; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; font: 18px/1.5 system-ui, sans-serif; background: #f6f5f1; color: #1d1c1a; }
main { max-width: 720px; padding: 48px; background: #fff; border-radius: var(--radius); box-shadow: 0 8px 32px rgba(0,0,0,.08); }
h1 { margin: 0 0 12px; font-size: 44px; line-height: 1.1; }
p { margin: 0 0 24px; color: #4a4843; }
a.button { display: inline-block; padding: 12px 20px; border-radius: var(--radius); background: var(--accent); color: #fff; text-decoration: none; font-weight: 600; }
</style>
</head>
<body>
<main>
<h1>` + esc + `</h1>
<p>Replace this artboard with your design.</p>
<a class="button" href="main.html">Get started</a>
</main>
</body>
</html>
`
	return []DesignFile{{Path: design.IndexPath, Data: cj}, {Path: "main.html", Data: []byte(page)}}
}

// CreateDesign creates a design flat holding files (the starter when nil)
// as revision 1 and starts it at once, Private. An existing slug is refused.
func (s *Service) CreateDesign(ctx context.Context, slugName, name string, files []DesignFile, message string, via Via) (FlatView, DesignWriteResult, error) {
	if err := slug.Validate(slugName); err != nil {
		return FlatView{}, DesignWriteResult{}, invalid(err)
	}
	unlock, lerr := s.lockCtx(ctx, slugName)
	if lerr != nil {
		return FlatView{}, DesignWriteResult{}, lerr
	}
	defer unlock()
	res, err := s.createDesignLocked(ctx, slugName, name, files, message, via)
	if err != nil {
		return FlatView{}, res, err
	}
	fv, err := s.GetFlat(ctx, slugName)
	return fv, res, err
}

func (s *Service) createDesignLocked(ctx context.Context, slugName, name string, files []DesignFile, message string, via Via) (DesignWriteResult, error) {
	title := strings.Join(strings.Fields(name), " ")
	if title == "" {
		title = slugName
	}
	if files == nil {
		files = starterDesign(title)
	}
	if message == "" {
		message = "created"
	}
	if _, err := s.st.GetFlat(ctx, slugName); err == nil {
		return DesignWriteResult{}, fmt.Errorf("%w: flat %q already exists", ErrConflict, slugName)
	}
	if err := s.validateOnly(files); err != nil {
		return DesignWriteResult{}, err
	}
	if _, err := s.createFlatLocked(ctx, slugName, title, via); err != nil {
		return DesignWriteResult{}, err
	}
	head, changed, err := s.prepareWrite(ctx, slugName, nil, files, nil)
	if err != nil {
		_ = s.st.DeleteFlat(ctx, slugName)
		_ = os.RemoveAll(s.flatDir(slugName))
		return DesignWriteResult{}, err
	}
	return s.initDesignLocked(ctx, slugName, title, head, changed, message, via, true)
}

// validateOnly checks files as a complete new design without storing them.
func (s *Service) validateOnly(files []DesignFile) error {
	paths := make([]string, 0, len(files))
	data := map[string][]byte{}
	for _, f := range files {
		if err := design.ValidPath(f.Path); err != nil {
			return &bundle.ValidationError{Problems: []bundle.Problem{{Path: f.Path, Message: err.Error(), Fix: "use a portable relative path such as boards/home.html"}}}
		}
		paths = append(paths, f.Path)
		data[f.Path] = f.Data
	}
	raw, ok := data[design.IndexPath]
	if !ok {
		return &bundle.ValidationError{Problems: []bundle.Problem{{Path: design.IndexPath, Message: "a design needs canvas.json", Fix: "write canvas.json with title, boards and order (guide topic.canvas)"}}}
	}
	c, err := design.ParseCanvas(raw)
	if err != nil {
		return &bundle.ValidationError{Problems: []bundle.Problem{{Path: design.IndexPath, Message: err.Error(), Fix: "write canvas.json as described in guide topic.canvas"}}}
	}
	if ps := c.Normalize(func(p string) bool { _, ok := data[p]; return ok }); len(ps) > 0 {
		verr := &bundle.ValidationError{}
		for _, p := range ps {
			verr.Problems = append(verr.Problems, bundle.Problem{Path: p.Path, Message: p.Message, Fix: p.Fix})
		}
		return verr
	}
	return nil
}

// adoptDesignLocked turns an untyped flat into a design holding w's files
// (the starter when none).
func (s *Service) adoptDesignLocked(ctx context.Context, f store.Flat, w DesignWrite, via Via) (DesignWriteResult, error) {
	if len(w.Delete) > 0 {
		return DesignWriteResult{}, invalidf("design %q has no files yet; nothing to delete", f.Slug)
	}
	title := f.Name
	if w.Name != "" {
		title = strings.Join(strings.Fields(w.Name), " ")
	}
	files := w.Files
	if len(files) == 0 {
		files = starterDesign(title)
	}
	head, changed, err := s.prepareWrite(ctx, f.Slug, nil, files, nil)
	if err != nil {
		return DesignWriteResult{}, err
	}
	msg := w.Message
	if msg == "" {
		msg = "created"
	}
	return s.initDesignLocked(ctx, f.Slug, title, head, changed, msg, via, false)
}

// initDesignLocked writes the design marker and revision 1 for an existing
// flat row, then publishes the shell.
func (s *Service) initDesignLocked(ctx context.Context, slugName, title string, head []store.HeadFile, changed []string, message string, via Via, fresh bool) (DesignWriteResult, error) {
	res := DesignWriteResult{Created: true, Changed: changed}
	err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		if err := tx.InitState(ctx, contenttype.Design); err != nil {
			return err
		}
		for _, f := range head {
			if err := putHeadGen(ctx, tx, f); err != nil {
				return err
			}
		}
		n, err := s.commitTx(ctx, tx, "created", []string{agentLabel(via)}, message, 0)
		if err != nil {
			return err
		}
		res.Revision = n
		res.Seq, err = tx.BumpSeq(ctx)
		return err
	})
	if err != nil {
		if fresh {
			_ = s.st.DeleteFlat(ctx, slugName)
		}
		return DesignWriteResult{}, err
	}
	if err := s.finishDesignLocked(ctx, slugName, title, via); err != nil {
		return res, err
	}
	s.Event(ctx, slugName, "info", "design", fmt.Sprintf("created design (revision %d) via %s", res.Revision, via), map[string]int{"revision": res.Revision})
	return res, nil
}

// finishDesignLocked stores and publishes the design's shell and marks the
// design ready. It is idempotent, for recovery of an interrupted creation.
func (s *Service) finishDesignLocked(ctx context.Context, slugName, title string, via Via) error {
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return err
	}
	if f.LiveVersion == 0 {
		manifest, _ := json.Marshal(map[string]string{"type": contenttype.Design, "name": title})
		v, err := s.saveDraftLocked(ctx, slugName, []bundle.File{{Path: bundle.ManifestName, Data: manifest}}, SaveMeta{Message: "design shell", quiet: true}, via)
		if err != nil {
			return err
		}
		rev, err := s.st.GetDraftRevision(ctx, slugName, v.Revision)
		if err != nil {
			return err
		}
		if f, err = s.st.GetFlat(ctx, slugName); err != nil {
			return err
		}
		if _, err := s.publishRevision(ctx, f, rev, "", via, "save"); err != nil {
			return err
		}
	}
	return s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.SetPhase(ctx, "ready") })
}

// startPendingDesigns finishes designs whose creation was interrupted.
func (s *Service) startPendingDesigns(ctx context.Context) {
	states, err := s.st.ContentStates(ctx)
	if err != nil {
		s.logf("start pending designs: %v", err)
		return
	}
	for _, st := range states {
		if st.Type != contenttype.Design || st.Phase != "init" {
			continue
		}
		unlock, err := s.lockStorage(st.Flat)
		if err == nil {
			var f store.Flat
			if f, err = s.st.GetFlat(ctx, st.Flat); err == nil {
				err = s.finishDesignLocked(ctx, st.Flat, f.Name, ViaConsole)
			}
			unlock()
		}
		if err != nil {
			s.Event(ctx, st.Flat, "error", "design", "could not finish creating the design: "+err.Error(), nil)
		}
	}
}

// agentLabel credits a write by where it came from: agents through MCP,
// and the CLI, API and console by name.
func agentLabel(via Via) string {
	switch via {
	case ViaMCP:
		return "Agent"
	case ViaCLI:
		return "CLI"
	case ViaAPI:
		return "API"
	case ViaConsole:
		return "Console"
	}
	return string(via)
}

// --- agent writes ---

// prepareWrite validates a write against the current head and stores the
// new blobs. It returns the new head and the changed paths.
func (s *Service) prepareWrite(ctx context.Context, slugName string, cur []store.HeadFile, files []DesignFile, deletes []string) ([]store.HeadFile, []string, error) {
	var problems []bundle.Problem
	add := func(p, msg, fix string) { problems = append(problems, bundle.Problem{Path: p, Message: msg, Fix: fix}) }
	limit := s.UploadLimit()
	var total int64
	byPath := map[string]store.HeadFile{}
	for _, f := range cur {
		byPath[f.Path] = f
	}
	seen := map[string]bool{}
	for _, f := range files {
		if err := design.ValidPath(f.Path); err != nil {
			add(f.Path, err.Error(), "use a portable relative path such as boards/home.html")
			continue
		}
		if seen[f.Path] {
			add(f.Path, "file listed twice", "list each file once")
		}
		seen[f.Path] = true
		total += int64(len(f.Data))
		if limit > 0 && int64(len(f.Data)) > limit {
			add(f.Path, fmt.Sprintf("file is %d bytes, over the %d-byte upload limit", len(f.Data), limit), "make the file smaller")
		}
	}
	if limit > 0 && total > limit {
		add("", fmt.Sprintf("write is %d bytes, over the %d-byte upload limit", total, limit), "split it into several save_design calls")
	}
	for _, p := range deletes {
		if seen[p] {
			add(p, "file is both written and deleted", "either write or delete it")
		}
		if _, ok := byPath[p]; !ok {
			add(p, "file to delete does not exist", "list existing paths in delete")
		}
	}
	if len(problems) > 0 {
		return nil, nil, &bundle.ValidationError{Problems: problems}
	}
	// The resulting content, read lazily.
	newData := map[string][]byte{}
	for _, f := range files {
		newData[f.Path] = f.Data
	}
	for _, p := range deletes {
		delete(byPath, p)
	}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		if _, ok := newData[p]; !ok {
			paths = append(paths, p)
		}
	}
	for p := range newData {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if err := design.CheckTree(paths); err != nil {
		add("", err.Error(), "rename or remove files")
	}
	exists := func(p string) bool { return slices.Contains(paths, p) }
	read := func(p string) ([]byte, error) {
		if d, ok := newData[p]; ok {
			return d, nil
		}
		if f, ok := byPath[p]; ok {
			return readBlob(s.designDir(slugName), f.Hash)
		}
		return nil, store.ErrNotFound
	}
	if raw, err := read(design.IndexPath); err != nil {
		add(design.IndexPath, "a design needs canvas.json", "write canvas.json with title, boards and order (guide topic.canvas)")
	} else if c, err := design.ParseCanvas(raw); err != nil {
		add(design.IndexPath, err.Error(), "write canvas.json as described in guide topic.canvas")
	} else {
		for _, p := range c.Normalize(exists) {
			add(p.Path, p.Message, p.Fix)
		}
	}
	for p, d := range newData {
		if !design.IsArtboardPath(p) {
			continue
		}
		if !utf8Valid(d) {
			add(p, "artboard is not UTF-8", "save the HTML file as UTF-8")
			continue
		}
		sc, err := design.Scan(d)
		if err != nil {
			add(p, err.Error(), "keep one tweaks block per artboard")
			continue
		}
		if sc.HasTweaks {
			if _, err := design.ParseTweaks(d[sc.Tweaks[0]:sc.Tweaks[1]]); err != nil {
				add(p, err.Error(), "fix the tweaks block (guide topic.canvas)")
			}
		}
	}
	if len(problems) > 0 {
		return nil, nil, &bundle.ValidationError{Problems: problems}
	}
	if err := s.checkDesignQuota(slugName, newData); err != nil {
		return nil, nil, err
	}
	var changed []string
	head := make([]store.HeadFile, 0, len(paths))
	for _, p := range paths {
		if d, ok := newData[p]; ok {
			h, err := putBlob(s.designDir(slugName), d)
			if err != nil {
				return nil, nil, err
			}
			old, had := byPath[p]
			gen := int64(0) // replaced: a new generation is assigned in the transaction
			if had && old.Hash == h {
				gen = old.Gen
			}
			if !had || old.Hash != h {
				changed = append(changed, p)
			}
			head = append(head, store.HeadFile{Path: p, Hash: h, Size: int64(len(d)), Gen: gen})
			continue
		}
		head = append(head, byPath[p])
	}
	changed = append(changed, deletes...)
	sort.Strings(changed)
	return head, changed, nil
}

func utf8Valid(b []byte) bool { return strings.ToValidUTF8(string(b), "�") == string(b) }

// checkDesignQuota checks the disk quota for the new unique bytes.
func (s *Service) checkDesignQuota(slugName string, data map[string][]byte) error {
	quota := s.diskQuota()
	if quota <= 0 {
		return nil
	}
	var add int64
	seen := map[string]bool{}
	for _, d := range data {
		h := hashBytes(d)
		if seen[h] {
			continue
		}
		seen[h] = true
		if _, err := os.Stat(blobPath(s.designDir(slugName), h)); err == nil {
			continue
		}
		add += int64(len(d))
	}
	if add == 0 {
		return nil
	}
	if used := dirSize(s.flatDir(slugName)); used+add > quota {
		return fmt.Errorf("%w: %w", ErrDiskQuota, &bundle.ValidationError{Problems: []bundle.Problem{{Message: fmt.Sprintf("design would use %d bytes, over its %d-byte disk quota", used+add, quota), Fix: "remove files or unpin old revisions, or raise the per-flat disk quota in system settings"}}})
	}
	return nil
}

// WriteDesign applies one agent write as one revision, creating the design
// when the slug is absent.
func (s *Service) WriteDesign(ctx context.Context, slugName string, w DesignWrite, via Via) (DesignWriteResult, error) {
	if err := slug.Validate(slugName); err != nil {
		return DesignWriteResult{}, invalid(err)
	}
	unlock, lerr := s.lockStorageCtx(ctx, slugName)
	if lerr != nil {
		return DesignWriteResult{}, lerr
	}
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	// A design that does not exist yet is at revision 0.
	if w.ExpectedRevision != nil && *w.ExpectedRevision != 0 {
		if errors.Is(err, store.ErrNotFound) {
			return DesignWriteResult{}, fmt.Errorf("%w: design %s does not exist (revision 0), not revision %d", ErrConflict, slugName, *w.ExpectedRevision)
		}
		if err == nil {
			if _, _, derr := s.designFlat(ctx, slugName); derr != nil {
				if t, terr := s.flatType(ctx, f); terr == nil && t == "" {
					return DesignWriteResult{}, fmt.Errorf("%w: %s has no design yet (revision 0), not revision %d", ErrConflict, slugName, *w.ExpectedRevision)
				}
			}
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		if len(w.Delete) > 0 {
			return DesignWriteResult{}, invalidf("design %q does not exist; nothing to delete", slugName)
		}
		files := w.Files
		if len(files) == 0 {
			files = nil
		}
		return s.createDesignLocked(ctx, slugName, w.Name, files, w.Message, via)
	}
	if err != nil {
		return DesignWriteResult{}, err
	}
	if _, _, err := s.designFlat(ctx, slugName); err != nil {
		t, terr := s.flatType(ctx, f)
		if terr != nil {
			return DesignWriteResult{}, terr
		}
		if t == "" {
			// An untyped flat (created empty, or left by an interrupted
			// creation) becomes a design on its first design write.
			return s.adoptDesignLocked(ctx, f, w, via)
		}
		if t != contenttype.Design {
			return DesignWriteResult{}, typeChangeError(f.Slug, t, contenttype.Design, "this save")
		}
		return DesignWriteResult{}, err
	}
	if len(w.Files) == 0 && len(w.Delete) == 0 {
		return DesignWriteResult{}, invalidf("nothing to write: pass files or delete")
	}
	// Check the revision guard before storing any blob: a refused write
	// must not leave bytes behind. The flat lock keeps it valid until the
	// transaction below.
	if w.ExpectedRevision != nil {
		err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return checkExpected(ctx, tx, *w.ExpectedRevision) })
		if err != nil {
			return DesignWriteResult{}, err
		}
	}
	cur, err := s.st.Head(ctx, slugName)
	if err != nil {
		return DesignWriteResult{}, err
	}
	head, changed, err := s.prepareWrite(ctx, slugName, cur, w.Files, w.Delete)
	if err != nil {
		return DesignWriteResult{}, err
	}
	res := DesignWriteResult{Changed: changed}
	deleted := 0
	err = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		latest, err := tx.LatestRevision(ctx)
		if err != nil {
			return err
		}
		if w.ExpectedRevision != nil {
			if err := checkExpected(ctx, tx, *w.ExpectedRevision); err != nil {
				return err
			}
		}
		if err := s.closeGroupsTx(ctx, tx, ""); err != nil {
			return err
		}
		if len(changed) == 0 {
			// Closing open groups may have committed a revision; report it.
			after, err := tx.LatestRevision(ctx)
			if err != nil {
				return err
			}
			res.Revision = after
			if after == latest {
				return nil
			}
			if res.Seq, err = tx.BumpSeq(ctx); err != nil {
				return err
			}
			deleted, err = s.retainTx(ctx, tx)
			return err
		}
		keep := map[string]bool{}
		for _, h := range head {
			keep[h.Path] = true
			if err := putHeadGen(ctx, tx, h); err != nil {
				return err
			}
		}
		for _, h := range cur {
			if !keep[h.Path] {
				if err := tx.DeleteHead(ctx, h.Path); err != nil {
					return err
				}
			}
		}
		n, err := s.commitTx(ctx, tx, "agent", []string{agentLabel(via)}, w.Message, 0)
		if err != nil {
			return err
		}
		res.Revision = n
		if res.Seq, err = tx.BumpSeq(ctx); err != nil {
			return err
		}
		deleted, err = s.retainTx(ctx, tx)
		return err
	})
	if err != nil {
		return DesignWriteResult{}, err
	}
	if res.Seq > 0 && len(changed) == 0 {
		s.notifyDesign(slugName, res.Seq, res.Revision, nil)
	}
	if len(changed) > 0 {
		s.notifyDesign(slugName, res.Seq, res.Revision, changed)
		s.Event(ctx, slugName, "info", "design", fmt.Sprintf("saved revision %d (%d files changed) via %s", res.Revision, len(changed), via), map[string]any{"revision": res.Revision, "changed": changed})
	}
	if deleted > 0 {
		go s.sweepDesignBlobs(context.WithoutCancel(ctx), slugName)
	}
	return res, nil
}

// putHeadGen stores a head file, assigning a new generation when Gen is 0.
func putHeadGen(ctx context.Context, tx *store.ContentTx, f store.HeadFile) error {
	if f.Gen == 0 {
		g, err := tx.NextGen(ctx)
		if err != nil {
			return err
		}
		f.Gen = g
	}
	return tx.PutHead(ctx, f)
}

// checkExpected refuses a write whose expected revision is not the latest,
// or while people's edits are not yet a revision (they count as a change
// since the agent read the design).
func checkExpected(ctx context.Context, tx *store.ContentTx, expected int) error {
	latest, err := tx.LatestRevision(ctx)
	if err != nil {
		return err
	}
	if expected != latest {
		return fmt.Errorf("%w: the design is at revision %d, not %d; read it again with get_design", ErrConflict, latest, expected)
	}
	pending, err := headDiffers(ctx, tx)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("%w: people are editing the design (changes since revision %d are not yet a revision); read it again with get_design, or retry in 30 seconds", ErrConflict, latest)
	}
	return nil
}

// --- revisions ---

// commitTx turns the head into the next revision. It does not run
// retention.
func (s *Service) commitTx(ctx context.Context, tx *store.ContentTx, kind string, authors []string, message string, rollbackOf int) (int, error) {
	latest, err := tx.LatestRevision(ctx)
	if err != nil {
		return 0, err
	}
	head, err := tx.Head(ctx)
	if err != nil {
		return 0, err
	}
	var prev []store.RevisionFile
	if latest > 0 {
		if prev, err = tx.RevisionFiles(ctx, latest); err != nil {
			return 0, err
		}
	}
	files := make([]store.RevisionFile, len(head))
	var size int64
	for i, h := range head {
		files[i] = store.RevisionFile{Path: h.Path, Hash: h.Hash, Size: h.Size}
		size += h.Size
	}
	r := store.Revision{
		Number: latest + 1, CreatedAt: s.now(), Kind: kind, Authors: dedupe(authors),
		Message: truncate(message, 500), RollbackOf: rollbackOf, Changes: diffFiles(prev, files),
		Files: len(files), Size: size,
	}
	if err := tx.InsertRevision(ctx, r, files); err != nil {
		return 0, err
	}
	return r.Number, nil
}

func dedupe(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x != "" && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func diffFiles(prev, cur []store.RevisionFile) store.Changes {
	c := store.Changes{Added: []string{}, Changed: []string{}, Removed: []string{}}
	old := map[string]string{}
	for _, f := range prev {
		old[f.Path] = f.Hash
	}
	for _, f := range cur {
		h, ok := old[f.Path]
		switch {
		case !ok:
			c.Added = append(c.Added, f.Path)
		case h != f.Hash:
			c.Changed = append(c.Changed, f.Path)
		}
		delete(old, f.Path)
	}
	for p := range old {
		c.Removed = append(c.Removed, p)
	}
	sort.Strings(c.Removed)
	return c
}

// headDiffers reports whether the head differs from the latest revision.
func headDiffers(ctx context.Context, tx *store.ContentTx) (bool, error) {
	latest, err := tx.LatestRevision(ctx)
	if err != nil {
		return false, err
	}
	head, err := tx.Head(ctx)
	if err != nil {
		return false, err
	}
	if latest == 0 {
		return len(head) > 0, nil
	}
	files, err := tx.RevisionFiles(ctx, latest)
	if err != nil {
		return false, err
	}
	if len(files) != len(head) {
		return true, nil
	}
	for i := range files {
		if files[i].Path != head[i].Path || files[i].Hash != head[i].Hash {
			return true, nil
		}
	}
	return false, nil
}

// closeGroupsTx keeps a changed head as a person revision crediting every
// editor with an open group. With editor set, only that editor's group
// closes and the others stay open; otherwise every group closes.
func (s *Service) closeGroupsTx(ctx context.Context, tx *store.ContentTx, editor string) error {
	groups, err := tx.Groups(ctx)
	if err != nil || len(groups) == 0 {
		return err
	}
	differs, err := headDiffers(ctx, tx)
	if err != nil {
		return err
	}
	if differs {
		var authors []string
		for _, g := range groups {
			authors = append(authors, g.Label)
		}
		if _, err := s.commitTx(ctx, tx, "person", authors, "", 0); err != nil {
			return err
		}
	}
	return tx.DeleteGroup(ctx, editor)
}

// quotas returns the revision and pin quotas of a design.
func (s *Service) quotas(st store.ContentState) (revisions, pins int) {
	revisions, pins = clampInt(s.intSetting(SetRevisionQuota)), clampInt(s.intSetting(SetPinQuota))
	if st.RevisionQuota != nil {
		revisions = *st.RevisionQuota
	}
	if st.PinQuota != nil {
		pins = *st.PinQuota
	}
	return revisions, pins
}

// retainTx deletes the oldest unpinned revisions over the revision quota,
// never the newest revision. It returns how many it deleted.
func (s *Service) retainTx(ctx context.Context, tx *store.ContentTx) (int, error) {
	st, err := tx.State(ctx)
	if err != nil {
		return 0, err
	}
	quota, _ := s.quotas(st)
	if quota <= 0 {
		return 0, nil
	}
	latest, err := tx.LatestRevision(ctx)
	if err != nil {
		return 0, err
	}
	unpinned, err := tx.UnpinnedOldestFirst(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, n := range unpinned {
		if len(unpinned)-deleted <= quota {
			break
		}
		if n == latest {
			continue
		}
		if err := tx.DeleteRevision(ctx, n); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// retainAllRevisions applies retention to every Revision-model flat, after
// a quota change or at startup. Startup leaves documents to
// startDocsRevisions, which imports them first.
func (s *Service) retainAllRevisions(ctx context.Context, docs bool) {
	states, err := s.st.ContentStates(ctx)
	if err != nil {
		return
	}
	for _, st := range states {
		if st.Type == contenttype.Docs && !docs {
			continue
		}
		s.retainRevisions(ctx, st.Flat, st.Type)
	}
}

func (s *Service) retainDesign(ctx context.Context, slugName string) {
	s.retainRevisions(ctx, slugName, contenttype.Design)
}

// retainRevisions applies retention to one flat under its lock and sweeps
// its blobs when it deleted revisions.
func (s *Service) retainRevisions(ctx context.Context, slugName, typ string) {
	unlock := s.lock(slugName)
	deleted, err := s.retainLocked(ctx, slugName)
	unlock()
	if err != nil {
		s.logf("retain revisions of %s: %v", slugName, err)
	}
	if deleted > 0 {
		s.sweepBlobs(ctx, slugName, typ)
	}
}

// retainLocked applies retention; the caller holds the flat lock.
func (s *Service) retainLocked(ctx context.Context, slugName string) (int, error) {
	deleted := 0
	err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		var err error
		deleted, err = s.retainTx(ctx, tx)
		return err
	})
	return deleted, err
}

// --- person operations ---

// ApplyDesignOp applies one person operation from the canvas app.
func (s *Service) ApplyDesignOp(ctx context.Context, slugName, editor, label string, op design.Op) error {
	unlock, lerr := s.lockStorageCtx(ctx, slugName)
	if lerr != nil {
		return lerr
	}
	defer unlock()
	if _, st, err := s.designFlat(ctx, slugName); err != nil {
		return err
	} else if st.Phase != "ready" {
		return fmt.Errorf("%w: the design is still being created", design.ErrStale)
	}
	cur, err := s.st.Head(ctx, slugName)
	if err != nil {
		return err
	}
	byPath := map[string]store.HeadFile{}
	for _, f := range cur {
		byPath[f.Path] = f
	}
	get := func(p string) (design.File, bool) {
		f, ok := byPath[p]
		if !ok {
			return design.File{}, false
		}
		b, err := readBlob(s.designDir(slugName), f.Hash)
		if err != nil {
			return design.File{}, false
		}
		return design.File{Data: b, Gen: f.Gen}, true
	}
	out, err := design.Apply(op, get)
	if err != nil {
		return err
	}
	if limit := s.UploadLimit(); limit > 0 {
		for p, d := range out {
			if int64(len(d)) > limit {
				return fmt.Errorf("%w: %s would be %d bytes, over the %d-byte upload limit", design.ErrInvalidOp, p, len(d), limit)
			}
		}
	}
	if err := s.checkDesignQuota(slugName, out); err != nil {
		return fmt.Errorf("%w: %w", design.ErrQuota, err)
	}
	var paths []string
	var seq int64
	var revision int
	changes := map[string]store.HeadFile{}
	for p, d := range out {
		h, err := putBlob(s.designDir(slugName), d)
		if err != nil {
			return err
		}
		old := byPath[p]
		changes[p] = store.HeadFile{Path: p, Hash: h, Size: int64(len(d)), Gen: old.Gen}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	err = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		for _, p := range paths {
			if err := tx.PutHead(ctx, changes[p]); err != nil {
				return err
			}
		}
		if err := tx.TouchGroup(ctx, editor, label, s.now()); err != nil {
			return err
		}
		var err error
		if revision, err = tx.LatestRevision(ctx); err != nil {
			return err
		}
		seq, err = tx.BumpSeq(ctx)
		return err
	})
	if err != nil {
		return err
	}
	s.notifyDesign(slugName, seq, revision, paths)
	return nil
}

// CloseDueGroups closes the edit groups that are idle or old enough, as
// person revisions. The design ticker calls it; tests call it directly.
func (s *Service) CloseDueGroups(ctx context.Context) {
	states, err := s.st.ContentStates(ctx)
	if err != nil {
		return
	}
	now := s.now()
	for _, st := range states {
		if st.Type != contenttype.Design {
			continue
		}
		s.closeDue(ctx, st.Flat, now)
	}
}

func (s *Service) closeDue(ctx context.Context, slugName string, now time.Time) {
	unlock := s.lock(slugName)
	defer unlock()
	var seq int64
	var revision int
	committed := false
	deleted := 0
	err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		groups, err := tx.Groups(ctx)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if now.Sub(g.Last) < groupIdle && now.Sub(g.Started) < groupMaxAge {
				continue
			}
			before, err := tx.LatestRevision(ctx)
			if err != nil {
				return err
			}
			if err := s.closeGroupsTx(ctx, tx, g.Editor); err != nil {
				return err
			}
			after, err := tx.LatestRevision(ctx)
			if err != nil {
				return err
			}
			if after != before {
				committed, revision = true, after
			}
		}
		if !committed {
			return nil
		}
		if seq, err = tx.BumpSeq(ctx); err != nil {
			return err
		}
		deleted, err = s.retainTx(ctx, tx)
		return err
	})
	if err != nil {
		s.logf("close edit groups of %s: %v", slugName, err)
		return
	}
	if committed {
		s.notifyDesign(slugName, seq, revision, nil)
	}
	if deleted > 0 {
		go s.sweepDesignBlobs(context.WithoutCancel(ctx), slugName)
	}
}

// --- history operations ---

// RollbackRevision restores revision n as a new revision.
func (s *Service) RollbackRevision(ctx context.Context, slugName string, n int, label string, via Via) (int, error) {
	// A document holds its target before it imports anything (rollbackDocs).
	if f, err := s.st.GetFlat(ctx, slugName); err != nil {
		return 0, err
	} else if docs, err := s.isDocs(ctx, f); err != nil {
		return 0, err
	} else if docs {
		return s.rollbackDocs(ctx, slugName, n, label, via)
	}
	if _, _, err := s.revisionFlat(ctx, slugName); err != nil {
		return 0, err
	}
	unlock, lerr := s.lockCtx(ctx, slugName)
	if lerr != nil {
		return 0, lerr
	}
	defer unlock()
	if _, _, err := s.designFlat(ctx, slugName); err != nil {
		return 0, err
	}
	var res int
	var seq int64
	var paths []string
	deleted := 0
	err := s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		if _, err := tx.Revision(ctx, n); err != nil {
			return err
		}
		target, err := tx.RevisionFiles(ctx, n)
		if err != nil {
			return err
		}
		if err := s.closeGroupsTx(ctx, tx, ""); err != nil {
			return err
		}
		cur, err := tx.Head(ctx)
		if err != nil {
			return err
		}
		curBy := map[string]store.HeadFile{}
		for _, h := range cur {
			curBy[h.Path] = h
		}
		keep := map[string]bool{}
		for _, f := range target {
			keep[f.Path] = true
			if old, had := curBy[f.Path]; !had || old.Hash != f.Hash {
				paths = append(paths, f.Path)
			}
			// Every restored file gets a new generation, so any change
			// prepared before the rollback is refused.
			if err := putHeadGen(ctx, tx, store.HeadFile{Path: f.Path, Hash: f.Hash, Size: f.Size}); err != nil {
				return err
			}
		}
		for _, h := range cur {
			if !keep[h.Path] {
				paths = append(paths, h.Path)
				if err := tx.DeleteHead(ctx, h.Path); err != nil {
					return err
				}
			}
		}
		if res, err = s.commitTx(ctx, tx, "rollback", []string{label}, fmt.Sprintf("restored revision %d", n), n); err != nil {
			return err
		}
		if seq, err = tx.BumpSeq(ctx); err != nil {
			return err
		}
		deleted, err = s.retainTx(ctx, tx)
		return err
	})
	if err != nil {
		return 0, err
	}
	sort.Strings(paths)
	s.notifyDesign(slugName, seq, res, paths)
	s.Event(ctx, slugName, "info", "design", fmt.Sprintf("restored revision %d as revision %d via %s", n, res, via), map[string]int{"revision": res, "rollback_of": n})
	if deleted > 0 {
		go s.sweepDesignBlobs(context.WithoutCancel(ctx), slugName)
	}
	return res, nil
}

// PinRevision pins or unpins revision n.
func (s *Service) PinRevision(ctx context.Context, slugName string, n int, pinned bool) error {
	if err := s.catchUpDocs(ctx, slugName); err != nil {
		return err
	}
	unlock, lerr := s.lockCtx(ctx, slugName)
	if lerr != nil {
		return lerr
	}
	defer unlock()
	_, st, err := s.revisionFlat(ctx, slugName)
	if err != nil {
		return err
	}
	typ := st.Type
	deleted := 0
	err = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
		r, err := tx.Revision(ctx, n)
		if err != nil {
			return err
		}
		if r.Pinned == pinned {
			return nil
		}
		if pinned {
			st, err := tx.State(ctx)
			if err != nil {
				return err
			}
			_, quota := s.quotas(st)
			if quota > 0 {
				count, err := tx.CountPins(ctx)
				if err != nil {
					return err
				}
				if count >= quota {
					return fmt.Errorf("%w: all %d pins are in use; unpin a revision first", ErrConflict, quota)
				}
			}
		}
		if err := tx.SetPinned(ctx, n, pinned); err != nil {
			return err
		}
		if !pinned {
			deleted, err = s.retainTx(ctx, tx)
		}
		return err
	})
	if err != nil {
		return err
	}
	if typ == contenttype.Design {
		s.notifyDesign(slugName, 0, 0, nil)
	}
	if deleted > 0 {
		go s.sweepBlobs(context.WithoutCancel(ctx), slugName, typ)
	}
	return nil
}

// ListRevisions returns a page of a flat's history, newest first.
func (s *Service) ListRevisions(ctx context.Context, slugName string, before, limit int) (design.RevisionPage, error) {
	if err := s.catchUpDocs(ctx, slugName); err != nil {
		return design.RevisionPage{}, err
	}
	_, st, err := s.revisionFlat(ctx, slugName)
	if err != nil {
		return design.RevisionPage{}, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	revs, err := s.st.ListRevisions(ctx, slugName, before, limit+1)
	if err != nil {
		return design.RevisionPage{}, err
	}
	page := design.RevisionPage{Revisions: revs}
	if len(revs) > limit {
		page.Revisions = revs[:limit]
		page.Next = revs[limit-1].Number
	}
	if page.Revisions == nil {
		page.Revisions = []store.Revision{}
	}
	page.Pins, _ = s.st.CountPins(ctx, slugName)
	page.RevisionQuota, page.PinQuota = s.quotas(st)
	return page, nil
}

// GetRevision returns revision n with every file it holds.
func (s *Service) GetRevision(ctx context.Context, slugName string, n int) (store.Revision, []store.RevisionFile, error) {
	if err := s.catchUpDocs(ctx, slugName); err != nil {
		return store.Revision{}, nil, err
	}
	if _, _, err := s.revisionFlat(ctx, slugName); err != nil {
		return store.Revision{}, nil, err
	}
	r, err := s.st.Revision(ctx, slugName, n)
	if err != nil {
		return r, nil, err
	}
	files, err := s.revisionFiles(ctx, slugName, r)
	return r, files, err
}

// RevisionFileContent returns one file of revision n. The bytes are read
// under a lease, so the sweep cannot remove them meanwhile.
func (s *Service) RevisionFileContent(ctx context.Context, slugName string, n int, p string) ([]byte, store.RevisionFile, error) {
	_, st, err := s.revisionFlat(ctx, slugName)
	if err != nil {
		return nil, store.RevisionFile{}, err
	}
	r, err := s.st.Revision(ctx, slugName, n)
	if err != nil {
		return nil, store.RevisionFile{}, err
	}
	unlock, err := s.lockCtx(ctx, slugName)
	if err != nil {
		return nil, store.RevisionFile{}, err
	}
	files, err := s.revisionFiles(ctx, slugName, r)
	var f store.RevisionFile
	found := false
	for _, x := range files {
		if x.Path == p {
			f, found = x, true
		}
	}
	var release func()
	if found {
		release = s.lease(slugName, []string{f.Hash})
	}
	unlock()
	if err != nil {
		return nil, f, err
	}
	if !found {
		return nil, f, fmt.Errorf("%w: revision %d has no file %q", store.ErrNotFound, n, p)
	}
	defer release()
	b, err := readBlob(s.storeDir(slugName, st.Type), f.Hash)
	return b, f, err
}

// SetRevisionQuotas stores per-design quota overrides (nil = host default)
// and applies retention at once.
func (s *Service) SetRevisionQuotas(ctx context.Context, slugName string, revisions, pins *int) error {
	for _, v := range []*int{revisions, pins} {
		if v != nil && *v < 0 {
			return invalidf("quotas are whole numbers ≥ 0 (0 = unlimited)")
		}
	}
	if err := s.catchUpDocs(ctx, slugName); err != nil {
		return err
	}
	unlock, lerr := s.lockCtx(ctx, slugName)
	if lerr != nil {
		return lerr
	}
	_, st, err := s.revisionFlat(ctx, slugName)
	if err != nil {
		unlock()
		return err
	}
	err = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error { return tx.SetQuotas(ctx, revisions, pins) })
	unlock()
	if err != nil {
		return err
	}
	s.retainRevisions(ctx, slugName, st.Type)
	return nil
}

// RevisionQuotaView is a design's quota overrides and the host defaults.
type RevisionQuotaView struct {
	RevisionQuota        *int `json:"revision_quota"`
	PinQuota             *int `json:"pin_quota"`
	DefaultRevisionQuota int  `json:"default_revision_quota"`
	DefaultPinQuota      int  `json:"default_pin_quota"`
	Pins                 int  `json:"pins"`
}

// RevisionQuotas returns a design's quota overrides and the host defaults.
func (s *Service) RevisionQuotas(ctx context.Context, slugName string) (RevisionQuotaView, error) {
	if err := s.catchUpDocs(ctx, slugName); err != nil {
		return RevisionQuotaView{}, err
	}
	_, st, err := s.revisionFlat(ctx, slugName)
	if err != nil {
		return RevisionQuotaView{}, err
	}
	v := RevisionQuotaView{RevisionQuota: st.RevisionQuota, PinQuota: st.PinQuota,
		DefaultRevisionQuota: clampInt(s.intSetting(SetRevisionQuota)), DefaultPinQuota: clampInt(s.intSetting(SetPinQuota))}
	v.Pins, _ = s.st.CountPins(ctx, slugName)
	return v, nil
}

// DesignSnapshot reads the head (rev 0) or a revision under read leases.
// Selecting the files and registering their leases happen under the flat
// lock, which the blob sweep also takes, so no blob of the snapshot can be
// swept in between. Reading the bytes happens after the lock is released.
func (s *Service) DesignSnapshot(ctx context.Context, slugName string, rev int) (*design.Snapshot, error) {
	unlock, lerr := s.lockCtx(ctx, slugName)
	if lerr != nil {
		return nil, lerr
	}
	snap, err := s.designSnapshotLocked(ctx, slugName, rev)
	unlock()
	return snap, err
}

func (s *Service) designSnapshotLocked(ctx context.Context, slugName string, rev int) (*design.Snapshot, error) {
	snap := &design.Snapshot{Revision: rev}
	if f, err := s.st.GetFlat(ctx, slugName); err == nil {
		snap.Visibility = string(f.Visibility)
	}
	var hashes []string
	if rev == 0 {
		st, err := s.st.ContentState(ctx, slugName)
		if err != nil {
			return nil, err
		}
		var head []store.HeadFile
		var latest int
		// One read transaction: head, latest revision and seq agree.
		err = s.st.ContentTx(ctx, slugName, func(tx *store.ContentTx) error {
			var err error
			if head, err = tx.Head(ctx); err != nil {
				return err
			}
			if latest, err = tx.LatestRevision(ctx); err != nil {
				return err
			}
			if snap.Pending, err = headDiffers(ctx, tx); err != nil {
				return err
			}
			cur, err := tx.State(ctx)
			st = cur
			return err
		})
		if err != nil {
			return nil, err
		}
		snap.Revision, snap.Seq = latest, st.Seq
		for _, h := range head {
			snap.Files = append(snap.Files, design.SnapFile{Path: h.Path, Hash: h.Hash, Size: h.Size, Gen: h.Gen})
			hashes = append(hashes, h.Hash)
		}
	} else {
		files, err := s.st.RevisionFiles(ctx, slugName, rev)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			if _, err := s.st.Revision(ctx, slugName, rev); err != nil {
				return nil, err
			}
		}
		for _, f := range files {
			snap.Files = append(snap.Files, design.SnapFile{Path: f.Path, Hash: f.Hash, Size: f.Size})
			hashes = append(hashes, f.Hash)
		}
	}
	snap.Release = s.lease(slugName, hashes)
	snap.Read = func(p string) ([]byte, error) {
		f, ok := snap.File(p)
		if !ok {
			return nil, fmt.Errorf("%s: %w", p, store.ErrNotFound)
		}
		return readBlob(s.designDir(slugName), f.Hash)
	}
	return snap, nil
}

// GetDesign returns the canvas state of a design for agents.
func (s *Service) GetDesign(ctx context.Context, slugName string) (*design.State, error) {
	if _, _, err := s.designFlat(ctx, slugName); err != nil {
		return nil, err
	}
	snap, err := s.DesignSnapshot(ctx, slugName, 0)
	if err != nil {
		return nil, err
	}
	defer snap.Release()
	st, err := design.BuildState(snap)
	if err != nil {
		return nil, err
	}
	st.Slug, st.Access = slugName, "private"
	return st, nil
}

// DesignFileContent returns one current file of a design.
func (s *Service) DesignFileContent(ctx context.Context, slugName, p string) ([]byte, design.SnapFile, error) {
	if _, _, err := s.designFlat(ctx, slugName); err != nil {
		return nil, design.SnapFile{}, err
	}
	snap, err := s.DesignSnapshot(ctx, slugName, 0)
	if err != nil {
		return nil, design.SnapFile{}, err
	}
	defer snap.Release()
	f, ok := snap.File(p)
	if !ok {
		return nil, f, fmt.Errorf("%w: design %s has no file %q", store.ErrNotFound, slugName, p)
	}
	b, err := snap.Read(p)
	return b, f, err
}

// --- blob sweep ---

// SweepDesignBlobs deletes unreferenced blobs and stale staging files of
// every design.
func (s *Service) SweepDesignBlobs(ctx context.Context) {
	states, err := s.st.ContentStates(ctx)
	if err != nil {
		return
	}
	for _, st := range states {
		s.sweepBlobs(ctx, st.Flat, st.Type)
	}
}

func (s *Service) sweepDesignBlobs(ctx context.Context, slugName string) {
	s.sweepBlobs(ctx, slugName, contenttype.Design)
}

// sweepBlobs deletes the unreferenced blobs of one Revision-model flat.
func (s *Service) sweepBlobs(ctx context.Context, slugName, typ string) {
	// During an unfinished rename or delete the files under this slug may
	// belong to another flat: never delete them by this slug's references.
	unlock, err := s.lockStorage(slugName)
	if err != nil {
		return
	}
	defer unlock()
	if typ == contenttype.Docs && !s.prepareDocsSweepLocked(ctx, slugName) {
		return
	}
	if err := s.st.Checkpoint(ctx, "FULL"); err != nil {
		s.logf("sweep blobs of %s: checkpoint: %v", slugName, err)
		return
	}
	refs, err := s.st.ReferencedHashes(ctx, slugName)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-blobGrace)
	root := s.storeDir(slugName, typ)
	_ = filepath.WalkDir(filepath.Join(root, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		h := d.Name()
		if refs[h] || s.leased(slugName, h) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(cutoff) {
			return nil
		}
		_ = os.Chmod(p, 0o644)
		_ = os.Remove(p)
		return nil
	})
	entries, _ := os.ReadDir(filepath.Join(root, "tmp"))
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(root, "tmp", e.Name()))
		}
	}
}

// designLoop closes due edit groups, records document revisions and sweeps
// blobs in the background. It first migrates and imports every running
// document, then applies their retention.
func (s *Service) designLoop() {
	defer s.wg.Done()
	s.startDocsRevisions(context.Background())
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	lastSweep, lastDocs := time.Now(), time.Now()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			ctx := context.Background()
			s.CloseDueGroups(ctx)
			if time.Since(lastDocs) >= docsTick {
				lastDocs = time.Now()
				s.TickDocs(ctx)
			}
			if time.Since(lastSweep) >= blobGrace {
				lastSweep = time.Now()
				s.SweepDesignBlobs(ctx)
			}
		}
	}
}

// --- serving ---

// notifyDesign tells the live design instance of slug about a change. A
// content change (seq > 0) also marks the flat as updated.
func (s *Service) notifyDesign(slugName string, seq int64, revision int, paths []string) {
	if seq > 0 {
		_ = s.st.Touch(context.Background(), slugName, s.now())
	}
	lf := s.state(slugName)
	if d := lf.cur.Load(); d != nil {
		if inst, ok := d.inst.(*design.Instance); ok {
			inst.Notify(seq, revision, paths)
		}
	}
}

// designBackend adapts the service to design.Backend for one slug.
type designBackend struct {
	s    *Service
	slug string
}

func (b designBackend) Snapshot(ctx context.Context, rev int) (*design.Snapshot, error) {
	snap, err := b.s.DesignSnapshot(ctx, b.slug, rev)
	return snap, designErr(err)
}

func (b designBackend) Apply(ctx context.Context, editor, label string, op design.Op) error {
	return designErr(b.s.ApplyDesignOp(ctx, b.slug, editor, label, op))
}

func (b designBackend) Revisions(ctx context.Context, before, limit int) (design.RevisionPage, error) {
	p, err := b.s.ListRevisions(ctx, b.slug, before, limit)
	return p, designErr(err)
}

func (b designBackend) Revision(ctx context.Context, n int) (store.Revision, []store.RevisionFile, error) {
	r, f, err := b.s.GetRevision(ctx, b.slug, n)
	return r, f, designErr(err)
}

func (b designBackend) Pin(ctx context.Context, n int, pinned bool) error {
	return designErr(b.s.PinRevision(ctx, b.slug, n, pinned))
}

func (b designBackend) Rollback(ctx context.Context, n int, label string) error {
	_, err := b.s.RollbackRevision(ctx, b.slug, n, label, ViaConsole)
	return designErr(err)
}

// designErr maps core errors to the design handler's sentinels.
func designErr(err error) error {
	var verr *bundle.ValidationError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, design.ErrStale), errors.Is(err, design.ErrInvalidOp), errors.Is(err, design.ErrQuota), errors.Is(err, design.ErrNotFound):
		return err
	case errors.Is(err, ErrDiskQuota):
		return fmt.Errorf("%w: %w", design.ErrQuota, err)
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%w: %w", design.ErrNotFound, err)
	case errors.Is(err, ErrConflict):
		return fmt.Errorf("%w: %w", design.ErrStale, err)
	case errors.As(err, &verr), errors.Is(err, ErrInvalid):
		return fmt.Errorf("%w: %w", design.ErrInvalidOp, err)
	}
	return err
}

// designInstance builds the live handler of a design flat.
func (s *Service) designInstance(slugName string) *design.Instance {
	return design.New(slugName, designBackend{s: s, slug: slugName})
}
