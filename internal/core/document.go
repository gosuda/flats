package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/store"
)

var (
	ErrNotDocs          = fmt.Errorf("%w: flat is not a docs flat", ErrInvalid)
	ErrDocumentNotFound = fmt.Errorf("%w: no such document", store.ErrNotFound)
	// ErrEditConflict refuses a live edit whose guard no longer matches: the
	// find text or block changed, is missing or ambiguous. Nothing changed.
	ErrEditConflict = errors.New("live edit not applied")
	// ErrDocumentCapacity refuses a live edit that would exceed a document
	// or history limit. Nothing changed.
	ErrDocumentCapacity = errors.New("document capacity")
)

// receiptLookupTimeout bounds checking a live edit whose response was lost.
var receiptLookupTimeout = 5 * time.Second

// documentEditRate bounds live edits per flat (per second, burst twice
// that), well under the docs app's per-room human update rate.
const documentEditRate = 10

// MaxDocumentEditOps is the largest number of operations in one live edit.
const MaxDocumentEditOps = 32

// maxDocumentEditBody matches the docs app's edit request limit.
const maxDocumentEditBody = 512 << 10

// DocumentConflict describes retained private recovery text without including it.
type DocumentConflict struct {
	Generation int64 `json:"generation"`
	Bytes      int64 `json:"bytes"`
}

// Document is exact Markdown from the live app, Current Draft, or preserved conflict.
type Document struct {
	Generation int64              `json:"generation,omitempty"`
	Conflicts  []DocumentConflict `json:"conflicts,omitempty"`
	Format     int                `json:"format"`
	Doc        string             `json:"doc"`
	Markdown   string             `json:"markdown"`
	Hash       string             `json:"hash,omitempty"`
	// Blocks, BlocksTotal and BlockOffset are present (also when empty or
	// zero) exactly when an outline of live text was requested. One page
	// holds at most 1,000 blocks.
	Blocks      *[]DocumentBlock `json:"blocks,omitempty"`
	BlocksTotal *int             `json:"blocks_total,omitempty"`
	BlockOffset *int             `json:"block_offset,omitempty"`
	Epoch       string           `json:"epoch,omitempty"`
	Chain       string           `json:"chain,omitempty"`
	Seq         int64            `json:"seq"`
	Source      string           `json:"source"`
}

// DocumentBlock is one Markdown block of live text with the guard hash a
// live edit names it by.
type DocumentBlock struct {
	Hash    string `json:"hash"`
	Kind    string `json:"kind"`
	Level   int    `json:"level,omitempty"`
	Line    int    `json:"line"`
	Preview string `json:"preview"`
}

// GetDocument prefers the running docs version, including collaborative edits.
// It never starts a worker just to read an unpublished Draft.
func (s *Service) GetDocument(ctx context.Context, slugName, doc string) (Document, error) {
	return s.getDocument(ctx, slugName, doc, 0, -1)
}

// GetDocumentBlocks is GetDocument plus one page of the live block outline
// used to guard live edits, starting at block index offset. A Draft has no
// blocks: it cannot be edited live.
func (s *Service) GetDocumentBlocks(ctx context.Context, slugName, doc string, offset int) (Document, error) {
	if offset < 0 {
		return Document{}, invalidf("block_offset must not be negative")
	}
	return s.getDocument(ctx, slugName, doc, 0, offset)
}

// GetDocumentConflict reads preserved text through the trusted host route only.
func (s *Service) GetDocumentConflict(ctx context.Context, slugName, doc string, generation int64) (Document, error) {
	if generation < 1 {
		return Document{}, invalidf("conflict must be a positive generation")
	}
	return s.getDocument(ctx, slugName, doc, generation, -1)
}

// getDocument reads live or Draft text; blockOffset >= 0 also requests a
// page of the live block outline.
func (s *Service) getDocument(ctx context.Context, slugName, doc string, conflict int64, blockOffset int) (Document, error) {
	unlock := s.lock(slugName)
	defer unlock()
	if _, err := s.st.GetFlat(ctx, slugName); err != nil {
		return Document{}, err
	}
	if doc != "" && (!fs.ValidPath(doc) || !bundle.IsMarkdown(doc)) {
		return Document{}, invalidf("doc must be a relative .md or .markdown path")
	}
	lf := s.state(slugName)
	live := lf.cur.Load()
	if live != nil && contenttype.FromManifest(live.version.Manifest) == contenttype.Docs {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		path := "/_docs/api/document?doc=" + url.QueryEscape(doc)
		if conflict != 0 {
			path = fmt.Sprintf("/_docs/api/conflict?doc=%s&generation=%d", url.QueryEscape(doc), conflict)
		} else if blockOffset >= 0 {
			path += fmt.Sprintf("&blocks=1&block_offset=%d", blockOffset)
		}
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://docs.internal"+path, nil)
		rec := httptest.NewRecorder()
		live.handler.ServeHTTP(rec, trustedAccess(req, false))
		if rec.Code == http.StatusNotFound {
			return Document{}, ErrDocumentNotFound
		}
		if rec.Code != http.StatusOK {
			return Document{}, fmt.Errorf("%w: docs app returned HTTP %d", ErrUnavailable, rec.Code)
		}
		if conflict != 0 {
			var row struct {
				Generation int64  `json:"generation"`
				Markdown   string `json:"markdown"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
				return Document{}, err
			}
			if row.Generation != conflict {
				return Document{}, fmt.Errorf("invalid conflict response")
			}
			if doc == "" {
				var m bundle.Manifest
				if err := json.Unmarshal(live.version.Manifest, &m); err != nil {
					return Document{}, err
				}
				doc = m.Entry
			}
			return Document{Format: 1, Doc: doc, Markdown: row.Markdown, Source: "conflict", Generation: conflict}, nil
		}
		var out Document
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			return Document{}, fmt.Errorf("docs app document response: %w", err)
		}
		if out.Format != 1 || !fs.ValidPath(out.Doc) || !bundle.IsMarkdown(out.Doc) {
			return Document{}, fmt.Errorf("docs app returned an invalid document response")
		}
		out.Source = "live"
		return out, nil
	}
	if conflict != 0 {
		return Document{}, ErrDocumentNotFound
	}
	draft, err := s.st.GetDraft(ctx, slugName)
	if errors.Is(err, store.ErrNotFound) {
		if live != nil {
			return Document{}, ErrNotDocs
		}
		return Document{}, fmt.Errorf("%w: no docs version is running and no Current Draft exists", ErrNotDeployed)
	}
	if err != nil {
		return Document{}, err
	}
	if contenttype.FromManifest(draft.Manifest) != contenttype.Docs {
		return Document{}, ErrNotDocs
	}
	var m bundle.Manifest
	if err := json.Unmarshal(draft.Manifest, &m); err != nil {
		return Document{}, err
	}
	if doc == "" {
		doc = m.Entry
	}
	if !fs.ValidPath(doc) || !bundle.IsMarkdown(doc) {
		return Document{}, ErrDocumentNotFound
	}
	root, err := os.OpenRoot(s.contentDir(slugName, store.Version{Revision: draft.Revision, Role: "draft"}))
	if err != nil {
		return Document{}, err
	}
	defer root.Close()
	b, err := root.ReadFile(doc)
	if os.IsNotExist(err) {
		return Document{}, ErrDocumentNotFound
	}
	if err != nil {
		return Document{}, err
	}
	sum := sha256.Sum256(b)
	return Document{Format: 1, Doc: doc, Markdown: string(b), Hash: hex.EncodeToString(sum[:]), Source: "draft"}, nil
}

// DocumentEditOp is one guarded live edit operation. The docs app validates
// and resolves it; see guide topic.docs.
type DocumentEditOp struct {
	Op         string  `json:"op" jsonschema:"replace, replace_block, delete_block or insert"`
	Find       string  `json:"find,omitempty" jsonschema:"replace: exact text to find; must occur once unless nth is given"`
	With       *string `json:"with,omitempty" jsonschema:"replace: replacement text (empty deletes the found text); replace_block: new Markdown for the block"`
	Block      string  `json:"block,omitempty" jsonschema:"replace_block, delete_block: a block hash from get_document blocks"`
	Text       string  `json:"text,omitempty" jsonschema:"insert: Markdown to insert as one or more blocks"`
	Before     string  `json:"before,omitempty" jsonschema:"insert: put text before this block hash"`
	After      string  `json:"after,omitempty" jsonschema:"insert: put text after this block hash"`
	SectionEnd string  `json:"section_end,omitempty" jsonschema:"insert: put text at the end of the section this heading block hash starts"`
	At         string  `json:"at,omitempty" jsonschema:"insert: start or end of the document"`
	Nth        *int    `json:"nth,omitempty" jsonschema:"pick the nth (1-based) match when the find text or block hash occurs more than once; requires if_hash"`
}

// DocumentEditChange summarizes one applied operation, without its text.
type DocumentEditChange struct {
	Op       string `json:"op"`
	Line     int    `json:"line" jsonschema:"line where the operation applied, in the text before it"`
	Removed  int    `json:"removed" jsonschema:"characters removed (UTF-16 code units)"`
	Inserted int    `json:"inserted" jsonschema:"characters inserted (UTF-16 code units)"`
}

// DocumentEdit is the result of a live edit.
type DocumentEdit struct {
	Format       int                  `json:"format"`
	Doc          string               `json:"doc"`
	SeqBefore    int64                `json:"seq_before"`
	Seq          int64                `json:"seq"`
	Chain        string               `json:"chain"`
	Hash         string               `json:"hash"`
	Bytes        int64                `json:"bytes"`
	Changed      bool                 `json:"changed"`
	ID           string               `json:"id,omitempty"`
	Ops          []DocumentEditChange `json:"ops"`
	Source       string               `json:"source"`
	PublicNotice string               `json:"public_notice,omitempty"`
}

// LiveEditPublicNotice is attached to every live edit result of a Public flat.
const LiveEditPublicNotice = PublicAccessNotice + " This edit is already live there."

// UpdateDocument applies guarded operations to the live Markdown of a
// running docs version, through the same committed path as people's edits.
// All operations apply or none do. It never publishes a version.
// ifHash and each op's Nth are pointers so an explicit but empty guard is
// passed on and refused, never silently dropped.
func (s *Service) UpdateDocument(ctx context.Context, slugName, doc string, ops []DocumentEditOp, ifHash *string, via Via) (DocumentEdit, error) {
	if len(ops) == 0 || len(ops) > MaxDocumentEditOps {
		return DocumentEdit{}, invalidf("ops must hold 1 to %d operations", MaxDocumentEditOps)
	}
	if doc != "" && (!fs.ValidPath(doc) || !bundle.IsMarkdown(doc)) {
		return DocumentEdit{}, invalidf("doc must be a relative .md or .markdown path")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return DocumentEdit{}, err
	}
	// The receipt id lets an edit whose response is lost be looked up.
	id := "agent:" + hex.EncodeToString(nonce[:])
	body, err := json.Marshal(struct {
		Ops    []DocumentEditOp `json:"ops"`
		IfHash *string          `json:"if_hash,omitempty"`
		ID     string           `json:"id"`
	}{ops, ifHash, id})
	if err != nil {
		return DocumentEdit{}, err
	}
	if len(body) > maxDocumentEditBody {
		return DocumentEdit{}, fmt.Errorf("%w: edit request is %d bytes, above %d; split it into smaller calls", ErrDocumentCapacity, len(body), maxDocumentEditBody)
	}
	unlock := s.lock(slugName)
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return DocumentEdit{}, err
	}
	lf := s.state(slugName)
	live := lf.cur.Load()
	if live == nil || contenttype.FromManifest(live.version.Manifest) != contenttype.Docs {
		draft, err := s.st.GetDraft(ctx, slugName)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return DocumentEdit{}, err
		}
		// A website (live or Draft) is not a document; a docs Draft or an
		// empty flat only lacks a running docs version.
		if docsDraft := err == nil && contenttype.FromManifest(draft.Manifest) == contenttype.Docs; !docsDraft && (live != nil || err == nil) {
			return DocumentEdit{}, ErrNotDocs
		}
		return DocumentEdit{}, fmt.Errorf("%w: no docs version is running, so there is no live text to edit; use save_document and publish", ErrNotDeployed)
	}
	if !lf.editLimiter.allow() {
		return DocumentEdit{}, fmt.Errorf("%w: too many live edits of this flat; wait a second and retry", ErrUnavailable)
	}
	// The audit record outlives the caller: a commit near the deadline or a
	// cancelled request must still leave its event.
	auditCtx := context.WithoutCancel(ctx)
	// Longer than the worker's own request deadline, so the worker normally
	// ends a slow edit (and rolls it back) before the host stops waiting.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://docs.internal/_docs/api/edit?doc="+url.QueryEscape(doc), bytes.NewReader(body))
	req = trustedAccess(req, false)
	req.Header.Set("X-Flats-Host-Op", "edit")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	live.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		var refusal struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
		msg := strings.TrimSpace(refusal.Message)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		switch {
		case rec.Code == http.StatusNotFound:
			return DocumentEdit{}, ErrDocumentNotFound
		case refusal.Code == "edit_conflict":
			return DocumentEdit{}, fmt.Errorf("%w: %s", ErrEditConflict, msg)
		case refusal.Code == "invalid":
			return DocumentEdit{}, invalidf("%s", msg)
		case refusal.Code == "capacity":
			return DocumentEdit{}, fmt.Errorf("%w: %s", ErrDocumentCapacity, msg)
		case rec.Code == http.StatusForbidden:
			return DocumentEdit{}, fmt.Errorf("%w: the docs app refused the host edit", ErrForbidden)
		case refusal.Code == "unavailable":
			return DocumentEdit{}, fmt.Errorf("%w: the docs app rolled the edit back; nothing changed, retry", ErrUnavailable)
		}
		// A timeout or worker failure can end the request after COMMIT.
		return DocumentEdit{}, s.uncertainEdit(auditCtx, live, slugName, doc, id, via, fmt.Sprintf("docs app returned HTTP %d", rec.Code), ctx.Err() == nil)
	}
	var out DocumentEdit
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Format != 1 || !fs.ValidPath(out.Doc) || !bundle.IsMarkdown(out.Doc) {
		return DocumentEdit{}, s.uncertainEdit(auditCtx, live, slugName, doc, id, via, "docs app returned an invalid edit response", true)
	}
	out.Source = "live"
	if f.Visibility.Public() {
		out.PublicNotice = LiveEditPublicNotice
	}
	if out.Changed {
		auditCtx, cancelAudit := context.WithTimeout(auditCtx, 10*time.Second)
		defer cancelAudit()
		s.Event(auditCtx, slugName, "info", "document", editSummary(out, via), map[string]any{
			"doc": out.Doc, "id": out.ID, "seq_before": out.SeqBefore, "seq": out.Seq, "hash": out.Hash, "ops": out.Ops,
		})
	}
	return out, nil
}

// uncertainEdit reconciles an edit whose outcome the response did not
// report: it looks the receipt id up, records what it found in the event
// log either way, and tells the caller whether the edit applied. A missing
// receipt proves nothing when the host stopped waiting first (answered is
// false): the worker may still commit.
func (s *Service) uncertainEdit(auditCtx context.Context, live *deployed, slugName, doc, id string, via Via, cause string, answered bool) error {
	// The lookup and the audit write have separate deadlines, so a lookup
	// that times out still leaves its warning.
	lookupCtx, cancelLookup := context.WithTimeout(auditCtx, receiptLookupTimeout)
	defer cancelLookup()
	ctx, cancel := context.WithTimeout(auditCtx, 10*time.Second)
	defer cancel()
	name := doc
	if name == "" {
		name = "the entry document"
	}
	req := httptest.NewRequestWithContext(lookupCtx, http.MethodGet, "http://docs.internal/_docs/api/receipt?doc="+url.QueryEscape(doc)+"&id="+url.QueryEscape(id), nil)
	req = trustedAccess(req, false)
	req.Header.Set("X-Flats-Host-Op", "edit")
	rec := httptest.NewRecorder()
	live.handler.ServeHTTP(rec, req)
	var receipt struct {
		Committed *bool `json:"committed"`
		Seq       int64 `json:"seq"`
	}
	data := map[string]any{"doc": doc, "id": id, "cause": cause}
	if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &receipt) == nil && receipt.Committed != nil {
		if *receipt.Committed {
			data["seq"] = receipt.Seq
			s.Event(ctx, slugName, "warn", "document", fmt.Sprintf("live edit of %s via %s committed at seq %d, but its response was lost (%s)", name, via, receipt.Seq, cause), data)
			return fmt.Errorf("%w: %s, but the edit was applied at seq %d; read get_document and do not repeat it", ErrUnavailable, cause, receipt.Seq)
		}
		if answered {
			s.Event(ctx, slugName, "warn", "document", fmt.Sprintf("live edit of %s via %s did not apply (%s)", name, via, cause), data)
			return fmt.Errorf("%w: %s; the edit did not apply, retry", ErrUnavailable, cause)
		}
	}
	s.Event(ctx, slugName, "warn", "document", fmt.Sprintf("live edit of %s via %s: outcome unknown (%s)", name, via, cause), data)
	return fmt.Errorf("%w: %s; the edit may or may not have applied: read get_document before retrying", ErrUnavailable, cause)
}

// editSummary describes a live edit for the event log without its text.
func editSummary(e DocumentEdit, via Via) string {
	var b strings.Builder
	fmt.Fprintf(&b, "live edit of %s via %s: %d op(s)", e.Doc, via, len(e.Ops))
	for i, op := range e.Ops {
		if i == 8 {
			fmt.Fprintf(&b, ", …")
			break
		}
		sep := ", "
		if i == 0 {
			sep = " ("
		}
		fmt.Fprintf(&b, "%s%s at line %d -%d/+%d", sep, op.Op, op.Line, op.Removed, op.Inserted)
	}
	if len(e.Ops) > 0 {
		b.WriteString(")")
	}
	fmt.Fprintf(&b, "; seq %d -> %d; now %d bytes", e.SeqBefore, e.Seq, e.Bytes)
	return b.String()
}
