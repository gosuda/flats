package notebook

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

const readPageBytes = 1 << 20
const readPartBytes = 128 << 10

// readQuery is deliberately separate from mutation targets: selectors may be ids.
type readQuery struct {
	Page      json.RawMessage `json:"page,omitempty"`
	Database  json.RawMessage `json:"database,omitempty"`
	View      json.RawMessage `json:"view,omitempty"`
	Format    string          `json:"format,omitempty"`
	Sort      []Sort          `json:"sort,omitempty"`
	Filter    []Filter        `json:"filter,omitempty"`
	Limit     int             `json:"limit,omitempty"`
	Cursor    string          `json:"cursor,omitempty"`
	OpID      string          `json:"op_id,omitempty"`
	RetryHint string          `json:"retry_hint,omitempty"`
}

type readMetadata struct {
	Store    string `json:"store"`
	Slug     string `json:"slug"`
	Query    string `json:"query"`
	Key      string `json:"key"`
	Limit    int    `json:"limit"`
	Owners   []Ref  `json:"owners"`
	Ordinal  uint64 `json:"ordinal"`
	Format   string `json:"format,omitempty"`
	Revision *int   `json:"revision"`
	Pending  bool   `json:"pending"`
	Outbox   int64  `json:"outbox"`
	Dirty    bool   `json:"dirty"`
}

// ReadRevisionBackend resolves an exact captured outbox boundary, not the
// current host head. Imported remains true when retention pruned its revision.
// The lookup runs after capture releases its SQL transaction and writer barrier.
type ReadRevisionBackend interface {
	NotebookReadRevision(context.Context, string, int64) (revision *int, imported bool, err error)
}

type readPosition struct {
	Token  string `json:"token"`
	Query  string `json:"query"`
	Record int64  `json:"record"`
	Offset int64  `json:"offset"`
	Page   int    `json:"page"`
}

type readContinuation struct {
	Record ID     `json:"record"`
	Part   int64  `json:"part"`
	Parts  int64  `json:"parts"`
	Data   string `json:"data"`
}

type readPageData struct {
	Items []json.RawMessage `json:"items"`
}

type readResult struct {
	State        *State             `json:"state,omitempty"`
	Revision     json.RawMessage    `json:"revision,omitempty"`
	Pending      *bool              `json:"pending,omitempty"`
	Tree         *[]json.RawMessage `json:"tree,omitempty"`
	Page         *readPageData      `json:"page,omitempty"`
	Schema       *[]json.RawMessage `json:"schema,omitempty"`
	Rows         *[]json.RawMessage `json:"rows,omitempty"`
	Continuation *readContinuation  `json:"continuation,omitempty"`
	Next         *string            `json:"next"`
	Complete     bool               `json:"complete"`
	Warnings     []Warning          `json:"warnings"`
	Markdown     *string            `json:"markdown,omitempty"`
	Offset       *int64             `json:"offset,omitempty"`
	References   map[ID]AssignedRef `json:"references,omitempty"`
}

func invalidRead(reason string) error { return &WriteError{Code: "invalid", Reason: reason} }
func expiredRead() error {
	return &WriteError{Code: "conflict", Reason: "read_expired", Detail: "cursor expired; read again"}
}

// Read captures current content once; subsequent pages read only its immutable spill.
func (in *Instance) Read(ctx context.Context, req []byte, public bool) ([]byte, error) {
	if in.ctx.Err() != nil {
		return nil, unavailable("stopped")
	}
	var q readQuery
	if err := strictDecode(req, &q); err != nil {
		return nil, invalidRead("query")
	}
	var fields map[string]json.RawMessage
	if err := strictDecode(req, &fields); err != nil || fields == nil {
		return nil, invalidRead("query")
	}
	if q.OpID != "" {
		if public {
			return nil, &WriteError{Code: "forbidden", Reason: "public"}
		}
		if len(fields) != 2 || q.Cursor == "" || !operationID.MatchString(q.OpID) {
			return nil, invalidRead("mapping_query")
		}
		return in.readMapping(ctx, q.OpID, q.Cursor)
	}
	if _, ok := fields["op_id"]; ok {
		return nil, invalidRead("op_id")
	}
	if q.Cursor != "" {
		if len(fields) != 1 {
			return nil, invalidRead("cursor_query")
		}
		return in.readCursor(ctx, q.Cursor, public)
	}
	if _, ok := fields["cursor"]; ok {
		return nil, invalidRead("cursor")
	}
	if q.Format != "" && q.Format != "structured" && q.Format != "markdown" {
		return nil, invalidRead("format")
	}
	if q.Format == "markdown" && (len(q.Page) == 0 || len(q.Database) != 0) {
		return nil, invalidRead("markdown_page")
	}
	if q.Limit == 0 {
		if _, ok := fields["limit"]; ok {
			return nil, invalidRead("limit")
		}
		q.Limit = 500
	}
	if q.Limit < 1 || q.Limit > 2000 {
		return nil, invalidRead("limit")
	}
	scope := Scope{Tree: true, Public: public}
	var selected *Ref
	var viewID ID
	var viewRef *Ref
	var err error
	if len(q.Page) != 0 {
		scope.Page, selected, err = readSelector(q.Page, PageKind)
		scope.Tree = false
	}
	if len(q.Database) != 0 {
		if !scope.Tree {
			return nil, invalidRead("selector")
		}
		scope.Database, selected, err = readSelector(q.Database, DatabaseKind)
		scope.Tree = false
	}
	if err != nil {
		return nil, err
	}
	if len(q.View) != 0 {
		viewID, viewRef, err = readSelector(q.View, ViewKind)
		if err != nil {
			return nil, err
		}
		if q.Sort != nil || q.Filter != nil {
			return nil, invalidRead("view_query")
		}
	}
	if scope.Database == "" && (len(q.View) != 0 || q.Sort != nil || q.Filter != nil) {
		return nil, invalidRead("database_query")
	}
	if len(q.View) != 0 || q.Sort != nil || q.Filter != nil {
		scope.View = &ViewDef{}
	}
	key, err := randomNotebookToken()
	if err != nil {
		return nil, err
	}
	meta := readMetadata{Store: in.storeID, Slug: in.slug, Query: contentHash(req), Key: key, Limit: q.Limit, Format: q.Format}
	var sourceError error
	read := func(c context.Context, fn func(*sql.Tx, Sequence) error) error {
		readErr := in.ReadTx(c, func(tx *sql.Tx, seq Sequence) error {
			meta.Owners = nil
			if !public {
				if err := captureReadStatus(c, tx, &meta); err != nil {
					return err
				}
			}
			var root Item
			if !scope.Tree {
				id := scope.Page
				if id == "" {
					id = scope.Database
				}
				root, err = readPreparedSelection(c, tx, id, selected)
				if err != nil {
					return err
				}
			}
			// These owners are outside a page/database capture but still need
			// mutation-ready references from exactly the capture transaction.
			var owner *ID
			switch d := root.Data.(type) {
			case Page:
				owner = d.Parent
			case Database:
				owner = &d.Block
			}
			if owner != nil && !public {
				r, e := readPreparedSelection(c, tx, *owner, nil)
				if e != nil {
					return e
				}
				meta.Owners = append(meta.Owners, r.Ref)
			}
			if scope.View != nil {
				v, e := readQueryView(c, tx, scope.Database, viewID, viewRef, q.Sort, q.Filter)
				if e != nil {
					return e
				}
				*scope.View = v
			}
			return fn(tx, seq)
		})
		if readErr != nil {
			sourceError = readErr
		}
		return readErr
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(in.ctx, cancel)
	defer stop()
	if _, ok := fields["retry_hint"]; ok {
		if q.RetryHint == "" {
			return nil, invalidRead("retry_hint")
		}
		delete(fields, "retry_hint")
	}
	query, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if err := in.admitAgentRead(); err != nil {
		return nil, readFailure(ctx, err)
	}
	ctx = context.WithValue(ctx, agentAdmissionKey{}, agentAdmission{query: contentHash(query), ticket: q.RetryHint})
	var s *Spill
	if q.Format == "markdown" {
		s, err = in.captureMarkdownRead(ctx, read, scope)
	} else {
		s, err = in.spills.Capture(ctx, read, scope, OrderRows)
	}
	if err != nil {
		if sourceError != nil {
			err = sourceError
		}
		return nil, readFailure(ctx, err)
	}
	if !public && q.Format != "markdown" {
		if err := buildReadReferences(ctx, s); err != nil {
			return nil, errors.Join(readFailure(ctx, err), s.Close())
		}
	}
	if !public {
		if err := in.resolveReadStatus(ctx, &meta); err != nil {
			return nil, errors.Join(readFailure(ctx, err), s.Close())
		}
	}
	if err := in.saveReadMetadata(s, meta); err != nil {
		return nil, errors.Join(readFailure(ctx, err), s.Close())
	}
	out, err := in.deliverRead(ctx, s, meta, readPosition{Token: s.Cursor(), Query: meta.Query}, public)
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return out, nil
}

func readFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var e *WriteError
	if errors.As(err, &e) {
		return err
	}
	if errors.Is(err, errSpillExpired) {
		return expiredRead()
	}
	return unavailable("snapshot")
}

func captureReadStatus(ctx context.Context, tx *sql.Tx, meta *readMetadata) error {
	var unacked bool
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='outbox'),0),
		EXISTS(SELECT 1 FROM groups WHERE dirty<>0) OR EXISTS(SELECT 1 FROM files WHERE dirty_seq<>'0'),
		EXISTS(SELECT 1 FROM outbox)`).Scan(&meta.Outbox, &meta.Dirty, &unacked)
	meta.Revision = nil
	meta.Pending = meta.Dirty || unacked
	return err
}

func (in *Instance) resolveReadStatus(ctx context.Context, meta *readMetadata) error {
	b, ok := in.b.(ReadRevisionBackend)
	if !ok {
		return nil
	}
	revision, imported, err := b.NotebookReadRevision(ctx, meta.Store, meta.Outbox)
	if err != nil {
		return err
	}
	meta.Pending = meta.Dirty || !imported
	if !meta.Pending {
		meta.Revision = revision
	}
	return nil
}

func readSelector(raw []byte, kind Kind) (ID, *Ref, error) {
	var id ID
	if len(raw) > 0 && raw[0] == '"' {
		if err := strictDecode(raw, &id); err != nil || id.ValidateKind(kind) != nil {
			return "", nil, invalidRead("selector")
		}
		return id, nil, nil
	}
	var ref Ref
	if err := strictDecode(raw, &ref); err != nil || ref.Inc == 0 || ref.ValidateKind(kind) != nil {
		return "", nil, invalidRead("selector")
	}
	return ref.ID, &ref, nil
}

func readSelected(ctx context.Context, tx *sql.Tx, id ID, ref *Ref) (Item, error) {
	r, err := loadRecord(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !r.Alive {
		return Item{}, &WriteError{Code: "not_found", Reason: "gone"}
	}
	if err != nil {
		return Item{}, err
	}
	if ref != nil && r.Item.Ref != *ref {
		return Item{}, &WriteError{Code: "conflict", Reason: "incarnation"}
	}
	return r.Item, nil
}

// Preparation needs only identity and query metadata; rendering needs full items.
func readPreparedSelection(ctx context.Context, tx *sql.Tx, id ID, ref *Ref) (Item, error) {
	r, alive, err := loadReadSelection(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !alive {
		return Item{}, &WriteError{Code: "not_found", Reason: "gone"}
	}
	if err != nil {
		return Item{}, err
	}
	if ref != nil && r.Ref != *ref {
		return Item{}, &WriteError{Code: "conflict", Reason: "incarnation"}
	}
	return r, nil
}

func readQueryView(ctx context.Context, tx *sql.Tx, database, view ID, ref *Ref, sorts []Sort, filters []Filter) (ViewDef, error) {
	resolve := func(id ID) (Ref, error) {
		i, err := readPreparedSelection(ctx, tx, id, nil)
		return i.Ref, err
	}
	if view != "" {
		i, err := readPreparedSelection(ctx, tx, view, ref)
		if err != nil {
			return ViewDef{}, err
		}
		v := i.Data.(View)
		if v.Database != database {
			return ViewDef{}, invalidRead("view_database")
		}
		return v.Definition.Wire(resolve)
	}
	v := ViewDef{Display: Display{Layout: "table", Visible: []Target{}, Widths: []Width{}}, Sort: sorts, Filter: filters}
	if v.Sort == nil {
		v.Sort = []Sort{}
	}
	if v.Filter == nil {
		v.Filter = []Filter{}
	}
	if err := v.Validate(); err != nil {
		return v, invalidRead("query")
	}
	property := func(t Target) (Property, error) {
		if t.Ref == nil {
			return Property{}, invalidRead("query_reference")
		}
		i, err := readPreparedSelection(ctx, tx, t.Ref.ID, t.Ref)
		if err != nil {
			return Property{}, err
		}
		p := i.Data.(Property)
		if p.Database != database {
			return p, invalidRead("query_property")
		}
		return p, nil
	}
	for _, s := range sorts {
		if _, err := property(s.Property); err != nil {
			return v, err
		}
	}
	for _, f := range filters {
		p, err := property(f.Property)
		if err != nil {
			return v, err
		}
		if err := f.ValidateFor(p.Definition.Type); err != nil {
			return v, invalidRead("query_filter")
		}
		if !f.Op.NeedsValue() || p.Definition.Type != SelectType && p.Definition.Type != MultiSelectType {
			continue
		}
		var options []Target
		if f.Op == IsAnyOf {
			if err := json.Unmarshal(f.Value, &options); err != nil {
				return v, invalidRead("query_filter")
			}
		} else {
			var option Target
			if err := json.Unmarshal(f.Value, &option); err != nil {
				return v, invalidRead("query_filter")
			}
			options = []Target{option}
		}
		for _, option := range options {
			if option.Ref == nil {
				return v, invalidRead("query_reference")
			}
			i, err := readPreparedSelection(ctx, tx, option.Ref.ID, option.Ref)
			if err != nil {
				return v, err
			}
			if i.Data.(Option).Property != f.Property.Ref.ID {
				return v, invalidRead("query_option")
			}
		}
	}
	return v, nil
}

// Evict before capture admission: an old over-budget agent spill must not
// block the fifth read from reaching the cap's eviction path.
func (in *Instance) admitAgentRead() error {
	m := in.spills
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	count := 0
	var oldest *Spill
	var ordinal uint64
	for _, s := range m.spills {
		if s.manager != m {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, "agent"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var meta readMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			return err
		}
		if meta.Store != in.storeID || meta.Slug != in.slug {
			continue
		}
		count++
		if oldest == nil || meta.Ordinal < ordinal {
			oldest, ordinal = s, meta.Ordinal
		}
	}
	if count >= 4 {
		m.removeLocked(oldest, errSpillExpired)
	}
	return nil
}

func (in *Instance) saveReadMetadata(s *Spill, meta readMetadata) error {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if s.removed {
		return errSpillExpired
	}
	var oldest *Spill
	var ordinal uint64
	count := 0
	for _, other := range m.spills {
		raw, err := os.ReadFile(filepath.Join(other.dir, "agent"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var previous readMetadata
		if err := json.Unmarshal(raw, &previous); err != nil {
			return err
		}
		if previous.Store != meta.Store || previous.Slug != meta.Slug {
			continue
		}
		count++
		ordinal = max(ordinal, previous.Ordinal)
		if oldest == nil || previous.Ordinal < meta.Ordinal {
			oldest, meta.Ordinal = other, previous.Ordinal
		}
	}
	meta.Ordinal = ordinal + 1
	if count >= 4 {
		m.removeLocked(oldest, errSpillExpired)
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// Account for the immutable query envelope as part of the same spill.
	n := int64(len(raw))
	if !s.over && n > m.budget-m.used {
		if m.over != nil || m.overGrantedLocked() || m.waitingLocked() {
			return errSpillUnavailable
		}
		m.used -= s.reserved
		s.over, m.over = true, s
	}
	if !s.over {
		m.used += n
	}
	s.reserved += n
	return os.WriteFile(filepath.Join(s.dir, "agent"), raw, 0600)
}

func (in *Instance) readCursor(ctx context.Context, cursor string, public bool) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(in.ctx, cancel)
	defer stop()
	if pieces := strings.Split(cursor, "."); len(pieces) == 2 && len(cursor) <= 2048 {
		if raw, err := base64.RawURLEncoding.DecodeString(pieces[0]); err == nil && len(strings.Split(string(raw), ":")) == 3 {
			if public {
				return nil, expiredRead()
			}
			runtime, err := in.historyRuntime()
			if err != nil {
				return nil, err
			}
			return in.historyCursor(ctx, runtime, 0, cursor)
		}
	}
	p, payload, signature, err := decodeReadCursor(cursor)
	if err != nil {
		return nil, expiredRead()
	}
	s, ok := in.spills.Lookup(p.Token)
	if !ok || s.scope.Public != public {
		return nil, expiredRead()
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, "agent"))
	if err != nil {
		return nil, expiredRead()
	}
	var meta readMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, readFailure(ctx, err)
	}
	mac := hmac.New(sha256.New, []byte(meta.Key))
	_, err = mac.Write(payload)
	if err != nil {
		return nil, err
	}
	if meta.Store != in.storeID || meta.Slug != in.slug || meta.Query != p.Query || !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, expiredRead()
	}
	out, err := in.deliverRead(ctx, s, meta, p, public)
	if ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), s.Close())
	}
	return out, err
}

func encodeReadCursor(p readPosition, meta readMetadata) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(meta.Key))
	if _, err := mac.Write(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func decodeReadCursor(cursor string) (readPosition, []byte, []byte, error) {
	var p readPosition
	pieces := strings.Split(cursor, ".")
	if len(pieces) != 2 || len(cursor) > 2048 {
		return p, nil, nil, expiredRead()
	}
	raw, err := base64.RawURLEncoding.DecodeString(pieces[0])
	if err != nil {
		return p, nil, nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(pieces[1])
	if err != nil {
		return p, nil, nil, err
	}
	if err := strictDecode(raw, &p); err != nil || p.Record < 0 || p.Offset < 0 || p.Page < 0 {
		return p, nil, nil, expiredRead()
	}
	return p, raw, sig, nil
}

// agentFiles uses the same lifecycle-owned handles as browser delivery so
// cancellation closes and unlinks active reads before releasing admission.
func agentFiles(s *Spill) (*spillCapture, func(), error) {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if s.removed {
		return nil, nil, errSpillExpired
	}
	local := &Spill{scope: s.scope}
	files := []*os.File{}
	closeFiles := func() {
		for _, f := range files {
			_ = f.Close()
			delete(s.handles, f)
		}
	}
	for _, entry := range []struct {
		name string
		file **os.File
	}{{"data", &local.data}, {"records", &local.records}, {"parts", &local.index}} {
		f, err := os.Open(filepath.Join(s.dir, entry.name))
		if err != nil {
			closeFiles()
			return nil, nil, err
		}
		*entry.file = f
		files = append(files, f)
		s.handles[f] = true
	}
	return &spillCapture{s: local, root: s.root, schemaNodes: s.schemaNodes}, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		closeFiles()
	}, nil
}

func (in *Instance) deliverRead(ctx context.Context, s *Spill, meta readMetadata, p readPosition, public bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if meta.Format == "markdown" {
		return in.deliverMarkdownRead(ctx, s, meta, p, public)
	}
	c, closeFiles, err := agentFiles(s)
	if err != nil {
		return nil, readFailure(ctx, err)
	}
	defer closeFiles()
	var refs *os.File
	if !public {
		var closeRefs func()
		refs, closeRefs, err = openReadFile(s, "agent-refs", os.O_RDONLY)
		if err != nil {
			return nil, readFailure(ctx, err)
		}
		defer closeRefs()
	}
	last, err := readSpillWindow(c.s.index, s.windows-1)
	if err != nil {
		return nil, readFailure(ctx, err)
	}
	total := s.schemaNodes
	if s.windows > 1 {
		total += last.start + last.count
	}
	if p.Record > total || p.Record == total && p.Offset != 0 {
		return nil, expiredRead()
	}
	result := readResult{Warnings: []Warning{}}
	if !public {
		result.Pending = &meta.Pending
		result.Revision, err = json.Marshal(meta.Revision)
		if err != nil {
			return nil, err
		}
		result.State = &State{Store: meta.Store, Seq: s.Sequence}
	}
	items := []json.RawMessage{}
	rows := []json.RawMessage{}
	schema := []json.RawMessage{}
	switch {
	case s.scope.Tree:
		result.Tree = &items
	case s.scope.Page != "":
		result.Page = &readPageData{Items: items}
	default:
		result.Schema, result.Rows = &schema, &rows
	}
	used := 0
	for n := 0; n < meta.Limit && p.Record < total; n++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		isRow := p.Record >= s.schemaNodes
		at := p.Record
		if isRow {
			at -= s.schemaNodes
		}
		node, err := c.at(isRow, at)
		if err != nil {
			return nil, readFailure(ctx, err)
		}
		if node.length > readPageBytes-8192 {
			if used > 0 {
				break
			}
			var source io.ReaderAt = c.s.data
			if !public {
				source, node, _, err = readContinuationSource(refs, meta, c.s.data, node)
				if err != nil {
					return nil, readFailure(ctx, err)
				}
			}
			if p.Offset >= node.length {
				return nil, expiredRead()
			}
			nominalEnd := min(p.Offset+readPartBytes, node.length)
			start, err := readRecordBoundary(source, node, p.Offset)
			if err != nil {
				return nil, readFailure(ctx, err)
			}
			end, err := readRecordBoundary(source, node, nominalEnd)
			if err != nil {
				return nil, readFailure(ctx, err)
			}
			data := make([]byte, end-start)
			if _, err := source.ReadAt(data, node.start+start); err != nil {
				return nil, readFailure(ctx, err)
			}
			id, err := readRecordID(io.NewSectionReader(source, node.start, node.length), public)
			if err != nil {
				return nil, readFailure(ctx, err)
			}
			result.Continuation = &readContinuation{Record: id, Part: p.Offset / readPartBytes, Parts: (node.length + readPartBytes - 1) / readPartBytes, Data: string(data)}
			p.Offset = nominalEnd
			if p.Offset == node.length {
				p.Record++
				p.Offset = 0
			}
			break
		}
		data := make([]byte, node.length)
		if _, err := c.s.data.ReadAt(data, node.start); err != nil {
			return nil, readFailure(ctx, err)
		}
		if !public {
			data, err = readWireItem(refs, meta, data)
			if err != nil {
				return nil, readFailure(ctx, err)
			}
		}
		if used > 0 && used+len(data) > readPageBytes-8192 {
			break
		}
		used += len(data) + 1
		switch {
		case s.scope.Tree:
			items = append(items, data)
		case s.scope.Page != "":
			result.Page.Items = append(result.Page.Items, data)
		case isRow:
			rows = append(rows, data)
		default:
			schema = append(schema, data)
		}
		p.Record++
	}
	result.Complete = p.Record == total
	p.Page++
	if !result.Complete {
		cursor, err := encodeReadCursor(p, meta)
		if err != nil {
			return nil, err
		}
		result.Next = &cursor
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if s.removed {
		return nil, expiredRead()
	}
	if !s.over || p.Page > s.high {
		s.last = m.now()
	}
	s.high = max(s.high, p.Page)
	if result.Complete {
		s.traversed = true
	}
	if s.over && s.traversed && m.waitingLocked() {
		m.removeLocked(s, errSpillExpired)
	}
	return out, nil
}

func readRecordBoundary(f io.ReaderAt, node spillNode, at int64) (int64, error) {
	if at == 0 || at == node.length {
		return at, nil
	}
	var b [1]byte
	for {
		if _, err := f.ReadAt(b[:], node.start+at); err != nil {
			return 0, err
		}
		if utf8.RuneStart(b[0]) {
			return at, nil
		}
		at--
	}
}

func readRecordID(r io.Reader, public bool) (ID, error) {
	if !public {
		ref, err := readRecordReference(r)
		return ref.ID, err
	}
	var id ID
	err := readRecordField(r, "id", &id)
	return id, err
}

// A continuation can append fixed-size ownership references without loading or
// copying its oversized content. Every offset still addresses immutable bytes.
type readAppendAt struct {
	base   io.ReaderAt
	length int64
	tail   *bytes.Reader
}

func (r readAppendAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative record offset")
	}
	n := 0
	if off < r.length {
		want := min(int64(len(p)), r.length-off)
		var err error
		n, err = r.base.ReadAt(p[:want], off)
		if err != nil {
			return n, err
		}
		off += int64(n)
		p = p[n:]
	}
	if len(p) == 0 {
		return n, nil
	}
	k, err := r.tail.ReadAt(p, off-r.length)
	return n + k, err
}

func readContinuationSource(refs *os.File, meta readMetadata, data *os.File, node spillNode) (io.ReaderAt, spillNode, bool, error) {
	source := io.NewSectionReader(data, node.start, node.length)
	var kind Kind
	if err := readRecordField(source, "kind", &kind); err != nil {
		return nil, node, false, err
	}
	fields := map[string]Ref{}
	add := func(name, field string) error {
		var id *ID
		if err := readRecordField(io.NewSectionReader(data, node.start, node.length), field, &id); err != nil {
			return err
		}
		if id == nil {
			return nil
		}
		for _, owner := range meta.Owners {
			if owner.ID == *id {
				fields[name] = owner
				return nil
			}
		}
		ref, err := lookupReadReference(refs, *id)
		fields[name] = ref
		return err
	}
	var err error
	switch kind {
	case PageKind:
		err = add("parent_ref", "parent")
	case BlockKind:
		err = add("page_ref", "page")
		if err == nil {
			err = add("parent_ref", "parent")
		}
	case DatabaseKind:
		err = add("block_ref", "block")
	case PropertyKind:
		err = add("database_ref", "database")
	case OptionKind:
		err = add("property_ref", "property")
	case RowKind:
		err = add("database_ref", "database")
	}
	if err != nil {
		return nil, node, false, err
	}
	if kind == RowKind || kind == ViewKind {
		r := &readConvertedAt{data: data, node: node, refs: refs, meta: meta, kind: kind, fields: fields}
		out := &readConvertedOutput{source: data}
		if err := r.write(out); err != nil {
			return nil, node, false, err
		}
		r.length = out.position
		node.start, node.length = 0, r.length
		return r, node, false, nil
	}
	// The capture encoder emits one object, followed by optional whitespace.
	end := node.length
	var last [1]byte
	for end > 0 {
		if _, err := data.ReadAt(last[:], node.start+end-1); err != nil {
			return nil, node, false, err
		}
		if !strings.ContainsRune(" 	\r\n", rune(last[0])) {
			break
		}
		end--
	}
	if end == 0 || last[0] != '}' {
		return nil, node, false, fmt.Errorf("invalid snapshot object")
	}
	tail, err := json.Marshal(fields)
	if err != nil {
		return nil, node, false, err
	}
	if len(fields) == 0 {
		tail = []byte("}")
	} else {
		tail[0] = ','
	}
	baseLength := end - 1
	reader := readAppendAt{base: io.NewSectionReader(data, node.start, baseLength), length: baseLength, tail: bytes.NewReader(tail)}
	node.start, node.length = 0, baseLength+int64(len(tail))
	return reader, node, false, nil
}

// Conversion replays immutable source ranges into just the requested part. Neither
// retained text nor the number of dependency references determines working memory.
type readConvertedAt struct {
	data        *os.File
	node        spillNode
	refs        *os.File
	meta        readMetadata
	kind        Kind
	fields      map[string]Ref
	length      int64
	mu          sync.Mutex
	rangesCache readRangeCache
}

type readConvertedOutput struct {
	source   io.ReaderAt
	position int64
	offset   int64
	buffer   []byte
	written  int
}

var errReadConvertedPart = errors.New("converted part complete")

func (w *readConvertedOutput) emit(start, length int64, literal []byte) error {
	end := w.position + length
	if w.buffer != nil && end > w.offset && w.position < w.offset+int64(len(w.buffer)) {
		lo := max(w.offset-w.position, 0)
		hi := min(length, w.offset+int64(len(w.buffer))-w.position)
		dst := w.buffer[w.written : w.written+int(hi-lo)]
		if literal != nil {
			copy(dst, literal[lo:hi])
		} else if _, err := w.source.ReadAt(dst, start+lo); err != nil {
			return err
		}
		w.written += len(dst)
	}
	w.position = end
	if w.buffer != nil && w.written == len(w.buffer) {
		return errReadConvertedPart
	}
	return nil
}

func (w *readConvertedOutput) text(s string) error {
	return w.emit(0, int64(len(s)), []byte(s))
}

func (r *readConvertedAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if off < 0 {
		return 0, fmt.Errorf("negative record offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.length {
		return 0, io.EOF
	}
	out := &readConvertedOutput{source: r.data, offset: off, buffer: p}
	err := r.write(out)
	if errors.Is(err, errReadConvertedPart) {
		return out.written, nil
	}
	if err == nil && out.written < len(p) {
		err = io.EOF
	}
	return out.written, err
}

func (r *readConvertedAt) resolve(id ID) (Ref, error) {
	for _, ref := range r.meta.Owners {
		if ref.ID == id {
			return ref, nil
		}
	}
	return lookupReadReference(r.refs, id)
}

// Stored object keys and identities are small; values are scanned, never decoded.
type readRangeCache struct {
	entries [64]struct{ start, end int64 }
	next    int
}

func (r *readConvertedAt) ranges(start, length int64) *readJSONRanges {
	ranges := newReadJSONRanges(r.data, start, length)
	ranges.cache = &r.rangesCache
	return ranges
}

type readJSONRanges struct {
	reader *bufio.Reader
	source *io.SectionReader
	start  int64
	cache  *readRangeCache
}

func newReadJSONRanges(source io.ReaderAt, start, length int64) *readJSONRanges {
	s := io.NewSectionReader(source, start, length)
	return &readJSONRanges{reader: bufio.NewReaderSize(s, spillChunk), source: s, start: start}
}

func (r *readJSONRanges) position() int64 {
	at, _ := r.source.Seek(0, io.SeekCurrent)
	return r.start + at - int64(r.reader.Buffered())
}

func (r *readJSONRanges) nonspace() (byte, error) {
	for {
		b, err := r.reader.ReadByte()
		if err != nil || !strings.ContainsRune(" 	\r\n", rune(b)) {
			return b, err
		}
	}
}

func (r *readJSONRanges) small(out any) error {
	var b bytes.Buffer
	if err := scanReadValue(r.reader, &b); err != nil {
		return err
	}
	return json.Unmarshal(b.Bytes(), out)
}

func readSmallRange(source io.ReaderAt, start, end int64, out any) error {
	var data [512]byte
	if end-start > int64(len(data)) {
		return fmt.Errorf("oversized snapshot identity")
	}
	value := data[:end-start]
	if _, err := source.ReadAt(value, start); err != nil {
		return err
	}
	return json.Unmarshal(value, out)
}

// Skip string interiors in buffered runs; large unchanged text must not cost a
// byte-at-a-time rescan for every virtual continuation offset.
func scanReadRange(r *bufio.Reader) error {
	depth, started, quoted := 0, false, false
	for {
		if quoted {
			chunk, err := r.Peek(max(1, r.Buffered()))
			if err != nil {
				return err
			}
			quote, escape := bytes.IndexByte(chunk, '"'), bytes.IndexByte(chunk, '\\')
			at := len(chunk)
			if quote >= 0 {
				at = quote
			}
			if escape >= 0 {
				at = min(at, escape)
			}
			if _, err := r.Discard(at); err != nil {
				return err
			}
			if at == len(chunk) {
				continue
			}
			b, err := r.ReadByte()
			if err != nil {
				return err
			}
			if b == '\\' {
				if _, err := r.ReadByte(); err != nil {
					return err
				}
				continue
			}
			quoted = false
			if depth == 0 {
				return nil
			}
			continue
		}
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		space := b == ' ' || b == '\n' || b == '\r' || b == '\t'
		if !started && space {
			continue
		}
		if started && depth == 0 && (space || b == ',' || b == '}' || b == ']') {
			return r.UnreadByte()
		}
		started = true
		switch b {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

func (r *readJSONRanges) skip() error {
	start := r.position()
	if r.cache != nil {
		for _, entry := range r.cache.entries {
			if entry.start == start && entry.end > start {
				if _, err := r.source.Seek(entry.end-r.start, io.SeekStart); err != nil {
					return err
				}
				r.reader.Reset(r.source)
				return nil
			}
		}
	}
	if err := scanReadRange(r.reader); err != nil {
		return err
	}
	if end := r.position(); r.cache != nil && end-start > spillChunk {
		entry := &r.cache.entries[r.cache.next%len(r.cache.entries)]
		entry.start, entry.end = start, end
		r.cache.next = (r.cache.next + 1) % len(r.cache.entries)
	}
	return nil
}

func (r *readJSONRanges) each(object bool, visit func(string, int64, int64) error) error {
	open, close := byte('['), byte(']')
	if object {
		open, close = '{', '}'
	}
	b, err := r.nonspace()
	if err != nil {
		return err
	}
	if b != open {
		return fmt.Errorf("invalid snapshot collection")
	}
	for {
		b, err = r.nonspace()
		if err != nil {
			return err
		}
		if b == close {
			return nil
		}
		if err := r.reader.UnreadByte(); err != nil {
			return err
		}
		var name string
		if object {
			if err := r.small(&name); err != nil {
				return err
			}
			b, err = r.nonspace()
			if err != nil {
				return err
			}
			if b != ':' {
				return fmt.Errorf("invalid snapshot field")
			}
		}
		b, err = r.nonspace()
		if err != nil {
			return err
		}
		if err := r.reader.UnreadByte(); err != nil {
			return err
		}
		start := r.position()
		if err := r.skip(); err != nil {
			return err
		}
		if err := visit(name, start, r.position()); err != nil {
			return err
		}
		b, err = r.nonspace()
		if err != nil {
			return err
		}
		if b == close {
			return nil
		}
		if b != ',' {
			return fmt.Errorf("invalid snapshot separator")
		}
	}
}

func (r *readConvertedAt) write(out *readConvertedOutput) error {
	if r.kind == ViewKind {
		keep := r.node.start
		if err := r.view(out, "", r.node.start, r.node.start+r.node.length, &keep); err != nil {
			return err
		}
		return out.emit(keep, r.node.start+r.node.length-keep, nil)
	}
	var cellsStart, cellsEnd int64
	err := r.ranges(r.node.start, r.node.length).each(true, func(name string, start, end int64) error {
		if name == "cells" {
			cellsStart, cellsEnd = start, end
		}
		return nil
	})
	if err != nil {
		return err
	}
	end := r.node.start + r.node.length
	var last [1]byte
	for {
		end--
		if _, err := r.data.ReadAt(last[:], end); err != nil {
			return err
		}
		if !strings.ContainsRune(" 	\r\n", rune(last[0])) {
			break
		}
	}
	if last[0] != '}' {
		return fmt.Errorf("invalid snapshot object")
	}
	if err := out.emit(r.node.start, end-r.node.start, nil); err != nil {
		return err
	}
	owner, err := json.Marshal(r.fields["database_ref"])
	if err != nil {
		return err
	}
	if err := out.text(`,"database_ref":` + string(owner) + `,"property_refs":{`); err != nil {
		return err
	}
	for pass := 0; pass < 2; pass++ {
		first := true
		add := func(id ID) error {
			ref, err := r.resolve(id)
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(ref)
			if err != nil {
				return err
			}
			prefix := ","
			if first {
				prefix, first = "", false
			}
			return out.text(prefix + strconv.Quote(string(id)) + ":" + string(encoded))
		}
		err := r.ranges(cellsStart, cellsEnd-cellsStart).each(true, func(name string, start, end int64) error {
			if pass == 0 {
				return add(ID(name))
			}
			var kind PropertyType
			if err := readRecordField(io.NewSectionReader(r.data, start, end-start), "type", &kind); err != nil {
				return err
			}
			if kind != SelectType && kind != MultiSelectType {
				return nil
			}
			return r.ranges(start, end-start).each(true, func(name string, start, end int64) error {
				if name != "value" {
					return nil
				}
				readID := func(_ string, start, end int64) error {
					var id *ID
					if err := readSmallRange(r.data, start, end, &id); err != nil {
						return err
					}
					if id == nil {
						return nil
					}
					return add(*id)
				}
				if kind == SelectType {
					return readID("", start, end)
				}
				return r.ranges(start, end-start).each(false, readID)
			})
		})
		if err != nil {
			return err
		}
		if pass == 0 {
			if err := out.text(`},"option_refs":{`); err != nil {
				return err
			}
		}
	}
	return out.text("}}")
}

func (r *readConvertedAt) view(out *readConvertedOutput, path string, start, end int64, keep *int64) error {
	replace := func(id ID) error {
		ref, err := r.resolve(id)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(Target{Ref: &ref})
		if err != nil {
			return err
		}
		if err := out.emit(*keep, start-*keep, nil); err != nil {
			return err
		}
		*keep = end
		return out.emit(0, int64(len(encoded)), encoded)
	}
	switch path {
	case "definition.display.visible.*", "definition.display.group_by", "definition.display.widths.*.property", "definition.sort.*.property", "definition.filter.*.property":
		var id *ID
		if err := readSmallRange(r.data, start, end, &id); err != nil {
			return err
		}
		if id != nil {
			return replace(*id)
		}
		return nil
	}
	var b [1]byte
	if _, err := r.data.ReadAt(b[:], start); err != nil {
		return err
	}
	if (path == "definition.filter.*.value" || path == "definition.filter.*.value.*") && b[0] == '{' {
		var option struct {
			ID ID `json:"option"`
		}
		if err := readSmallRange(r.data, start, end, &option); err != nil {
			return err
		}
		return replace(option.ID)
	}
	if b[0] != '{' && b[0] != '[' {
		return nil
	}
	return r.ranges(start, end-start).each(b[0] == '{', func(name string, start, end int64) error {
		if b[0] == '[' {
			name = "*"
		}
		child := name
		if path != "" {
			child = path + "." + name
		}
		return r.view(out, child, start, end, keep)
	})
}

// Skip content without asking encoding/json to allocate an oversized string.
func readRecordField(source io.Reader, name string, out any) error {
	r := bufio.NewReaderSize(source, spillChunk)
	nonspace := func() (byte, error) {
		for {
			b, err := r.ReadByte()
			if err != nil {
				return 0, err
			}
			if b != ' ' && b != '\n' && b != '\r' && b != '	' {
				return b, nil
			}
		}
	}
	first, err := nonspace()
	if err != nil {
		return err
	}
	if first != '{' {
		return fmt.Errorf("invalid snapshot record")
	}
	for {
		first, err = nonspace()
		if err != nil {
			return err
		}
		if first == '}' {
			return fmt.Errorf("snapshot record has no %s", name)
		}
		if err := r.UnreadByte(); err != nil {
			return err
		}
		var key bytes.Buffer
		if err := scanReadValue(r, &key); err != nil {
			return err
		}
		var field string
		if err := json.Unmarshal(key.Bytes(), &field); err != nil {
			return err
		}
		separator, err := nonspace()
		if err != nil {
			return err
		}
		if separator != ':' {
			return fmt.Errorf("invalid snapshot field")
		}
		if field == name {
			var value bytes.Buffer
			if err := scanReadValue(r, &value); err != nil {
				return err
			}
			return json.Unmarshal(value.Bytes(), out)
		}
		if err := scanReadValue(r, nil); err != nil {
			return err
		}
		separator, err = nonspace()
		if err != nil {
			return err
		}
		if separator == '}' {
			return fmt.Errorf("snapshot record has no %s", name)
		}
		if separator != ',' {
			return fmt.Errorf("invalid snapshot field separator")
		}
	}
}

func scanReadValue(r *bufio.Reader, keep *bytes.Buffer) error {
	depth, started, quoted, escaped := 0, false, false, false
	for {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		if !started && (b == ' ' || b == '\n' || b == '\r' || b == '	') {
			continue
		}
		if started && depth == 0 && !quoted && (b == ',' || b == '}' || b == ']' || b == ' ' || b == '\n' || b == '\r' || b == '	') {
			return r.UnreadByte()
		}
		started = true
		if keep != nil {
			if keep.Len() >= 512 {
				return fmt.Errorf("oversized snapshot identity")
			}
			if err := keep.WriteByte(b); err != nil {
				return err
			}
		}
		if quoted {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
				continue
			}
			if b == '"' {
				quoted = false
				if depth == 0 {
					return nil
				}
			}
			continue
		}
		switch b {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

const readRefSlotBytes = 48

func openReadFile(s *Spill, name string, flags int) (*os.File, func(), error) {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if s.removed {
		return nil, nil, errSpillExpired
	}
	f, err := os.OpenFile(filepath.Join(s.dir, name), flags, 0600)
	if err != nil {
		return nil, nil, err
	}
	s.handles[f] = true
	return f, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		_ = f.Close()
		delete(s.handles, f)
	}, nil
}

func buildReadReferences(ctx context.Context, s *Spill) error {
	if _, err := os.Stat(filepath.Join(s.dir, "agent-refs")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	c, closeFiles, err := agentFiles(s)
	if err != nil {
		return err
	}
	defer closeFiles()
	slots := int64(1)
	for slots < s.schemaNodes*2 {
		slots *= 2
	}
	if err := s.charge(slots * readRefSlotBytes); err != nil {
		return err
	}
	f, closeIndex, err := openReadFile(s, "agent-refs", os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err != nil {
		return err
	}
	defer closeIndex()
	if err := f.Truncate(slots * readRefSlotBytes); err != nil {
		return err
	}
	for at := int64(0); at < s.schemaNodes; at++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		node, err := c.node(at)
		if err != nil {
			return err
		}
		ref, err := readRecordReference(io.NewSectionReader(c.s.data, node.start, node.length))
		if err != nil {
			return err
		}
		hash := sha256.Sum256([]byte(ref.ID))
		position := int64(binary.LittleEndian.Uint64(hash[:8]) & uint64(slots-1))
		var slot [readRefSlotBytes]byte
		for {
			if _, err := f.ReadAt(slot[:], position*readRefSlotBytes); err != nil {
				return err
			}
			if slot[0] == 0 {
				break
			}
			position = (position + 1) & (slots - 1)
		}
		slot[0] = byte(len(ref.ID))
		copy(slot[1:32], ref.ID)
		binary.LittleEndian.PutUint64(slot[32:40], uint64(ref.Inc))
		if _, err := f.WriteAt(slot[:], position*readRefSlotBytes); err != nil {
			return err
		}
	}
	return nil
}

func readRecordReference(r io.Reader) (Ref, error) {
	var ref Ref
	err := readRecordField(r, "ref", &ref)
	return ref, err
}

func lookupReadReference(f *os.File, id ID) (Ref, error) {
	info, err := f.Stat()
	if err != nil {
		return Ref{}, err
	}
	slots := info.Size() / readRefSlotBytes
	hash := sha256.Sum256([]byte(id))
	at := int64(binary.LittleEndian.Uint64(hash[:8]) & uint64(slots-1))
	var slot [readRefSlotBytes]byte
	for n := int64(0); n < slots; n++ {
		if _, err := f.ReadAt(slot[:], at*readRefSlotBytes); err != nil {
			return Ref{}, err
		}
		if slot[0] == 0 {
			break
		}
		if slot[0] > 31 {
			return Ref{}, fmt.Errorf("invalid snapshot reference index")
		}
		if ID(slot[1:1+int(slot[0])]) == id {
			return Ref{ID: id, Inc: Incarnation(binary.LittleEndian.Uint64(slot[32:40]))}, nil
		}
		at = (at + 1) & (slots - 1)
	}
	return Ref{}, fmt.Errorf("snapshot dependency is missing")
}

func readWireItem(refs *os.File, meta readMetadata, data []byte) ([]byte, error) {
	i, err := DecodeItem(data)
	if err != nil {
		return nil, err
	}
	resolve := func(id ID) (Ref, error) {
		if id == i.ID() {
			return i.Ref, nil
		}
		for _, ref := range meta.Owners {
			if ref.ID == id {
				return ref, nil
			}
		}
		return lookupReadReference(refs, id)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	add := func(name string, id ID) error {
		ref, err := resolve(id)
		if err == nil {
			fields[name] = ref
		}
		return err
	}
	switch d := i.Data.(type) {
	case Page:
		if d.Parent != nil {
			err = add("parent_ref", *d.Parent)
		}
	case Block:
		err = add("page_ref", d.Page)
		if err == nil && d.Parent != nil {
			err = add("parent_ref", *d.Parent)
		}
	case Database:
		err = add("block_ref", d.Block)
	case Property:
		err = add("database_ref", d.Database)
	case Option:
		err = add("property_ref", d.Property)
	case Row:
		err = add("database_ref", d.Database)
		properties := map[ID]Ref{}
		options := map[ID]Ref{}
		for id, cell := range d.Cells {
			property, e := resolve(id)
			if e != nil {
				return nil, e
			}
			properties[id] = property
			optionIDs := []ID{}
			switch cell.Type {
			case SelectType:
				if cell.Value != nil {
					optionIDs = append(optionIDs, cell.Value.(ID))
				}
			case MultiSelectType:
				optionIDs = cell.Value.([]ID)
			}
			for _, id := range optionIDs {
				option, e := resolve(id)
				if e != nil {
					return nil, e
				}
				options[id] = option
			}
		}
		fields["property_refs"], fields["option_refs"] = properties, options
	case View:
		fields["definition"], err = d.Definition.Wire(resolve)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func (in *Instance) readMapping(ctx context.Context, opID, cursor string) ([]byte, error) {
	var result struct {
		OpID     string         `json:"op_id"`
		Total    int            `json:"total"`
		Entries  []MappingEntry `json:"entries"`
		Next     *string        `json:"next"`
		Complete bool           `json:"complete"`
	}
	result.OpID, result.Entries = opID, []MappingEntry{}
	err := in.ReadTx(ctx, func(tx *sql.Tx, _ Sequence) error {
		var token string
		err := tx.QueryRowContext(ctx, `SELECT mapping_cursor FROM commands WHERE op_id=?`, opID).Scan(&token)
		if errors.Is(err, sql.ErrNoRows) {
			return &WriteError{Code: "conflict", Reason: "result_expired"}
		}
		if err != nil {
			return err
		}
		offset := 0
		if cursor != token {
			prefix := token + "."
			if token == "" || !strings.HasPrefix(cursor, prefix) {
				return &WriteError{Code: "conflict", Reason: "result_expired"}
			}
			offset, err = strconv.Atoi(strings.TrimPrefix(cursor, prefix))
			if err != nil || offset < 0 || strconv.Itoa(offset) != strings.TrimPrefix(cursor, prefix) {
				return invalidRead("mapping_cursor")
			}
		}
		if token == "" {
			return &WriteError{Code: "conflict", Reason: "result_expired"}
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM command_mappings WHERE op_id=?`, opID).Scan(&result.Total); err != nil {
			return err
		}
		if offset > result.Total {
			return invalidRead("mapping_cursor")
		}
		rows, err := tx.QueryContext(ctx, `SELECT operation_index,tmp,source,assigned,version FROM command_mappings WHERE op_id=? AND offset>=? ORDER BY offset LIMIT 500`, opID, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var entry MappingEntry
			var tmp, source sql.NullString
			var assigned, version string
			if err := rows.Scan(&entry.Index, &tmp, &source, &assigned, &version); err != nil {
				return err
			}
			entry.Tmp = tmp.String
			if source.Valid {
				var ref Ref
				if err := json.Unmarshal([]byte(source.String), &ref); err != nil {
					return err
				}
				entry.Source = &ref
			}
			if err := json.Unmarshal([]byte(assigned), &entry.Ref); err != nil {
				return err
			}
			n, err := parseDBDecimal(version)
			if err != nil {
				return err
			}
			entry.Ver = Version(n)
			result.Entries = append(result.Entries, entry)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		next := offset + len(result.Entries)
		result.Complete = next == result.Total
		if !result.Complete {
			cursor := token + "." + strconv.Itoa(next)
			result.Next = &cursor
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

// Markdown is captured directly into bounded, indexed units. Rows are read
// inside the page's capture transaction, never fetched during cursor delivery.
type markdownReadUnit struct {
	Markdown  string       `json:"markdown"`
	Reference *AssignedRef `json:"reference,omitempty"`
	Warnings  []Warning    `json:"warnings,omitempty"`
}

type markdownReadCapture struct {
	ctx     context.Context
	tx      *sql.Tx
	s       *Spill
	writer  captureWriter
	offset  int64
	units   int64
	options *markdownReadOptions
}

func (c *markdownReadCapture) emit(text string, item *Item, warnings []Warning) error {
	for first := true; first || text != ""; first = false {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		end := min(len(text), readPartBytes)
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		u := markdownReadUnit{Markdown: text[:end]}
		if first {
			u.Warnings = warnings
			if item != nil && !c.s.scope.Public {
				u.Reference = &AssignedRef{Ref: item.Ref, Ver: item.Ver}
			}
		}
		start := c.writer.offset
		if err := json.NewEncoder(&c.writer).Encode(u); err != nil {
			return err
		}
		var index [24]byte
		binary.LittleEndian.PutUint64(index[:8], uint64(start))
		binary.LittleEndian.PutUint64(index[8:16], uint64(c.writer.offset-start))
		binary.LittleEndian.PutUint64(index[16:], uint64(c.offset))
		if err := c.s.charge(int64(len(index))); err != nil {
			return err
		}
		if _, err := c.s.records.Write(index[:]); err != nil {
			return err
		}
		c.offset += int64(end)
		c.units++
		text = text[end:]
	}
	return nil
}

func nullableID(id ID) any {
	if id == "" {
		return nil
	}
	return id
}

func (in *Instance) captureMarkdownRead(ctx context.Context, read func(context.Context, func(*sql.Tx, Sequence) error) error, scope Scope) (*Spill, error) {
	m := in.spills
	captureCtx, cancel := context.WithCancelCause(ctx)
	request := &spillRequest{scope: scope, cancel: cancel, estimating: true}
	m.mu.Lock()
	request.started = m.now()
	m.requests[request] = true
	m.scheduleLocked()
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.requests, request)
		m.scheduleLocked()
		m.mu.Unlock()
		cancel(nil)
	}()
	var reservation int64
	err := read(captureCtx, func(tx *sql.Tx, _ Sequence) error {
		var err error
		reservation, err = estimateSpill(captureCtx, tx, scope)
		if err != nil {
			return err
		}
		var rowBytes int64
		err = tx.QueryRowContext(captureCtx, `SELECT COALESCE(sum(length(CAST(c.value AS BLOB))+256),0)
			FROM cells c JOIN rows r ON r.id=c.row_id JOIN databases d ON d.id=r.database_id JOIN blocks b ON b.id=d.block WHERE b.page=?`, scope.Page).Scan(&rowBytes)
		if err == nil {
			reservation, err = quotaAdd(reservation, rowBytes)
		}
		return err
	})
	m.mu.Lock()
	m.expireLocked()
	elapsed := m.now().Sub(request.started)
	request.estimating = false
	m.scheduleLocked()
	m.mu.Unlock()
	if cause := context.Cause(captureCtx); cause != nil {
		return nil, cause
	}
	if err != nil {
		return nil, err
	}
	s, err := m.acquire(captureCtx, reservation, scope)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	s.cancel = cancel
	s.progressAt = s.progressAt.Add(-elapsed)
	m.scheduleLocked()
	m.mu.Unlock()
	success := false
	defer func() {
		for _, f := range []*os.File{s.data, s.records, s.index} {
			if f != nil {
				_ = f.Close()
				m.mu.Lock()
				delete(s.handles, f)
				m.mu.Unlock()
			}
		}
		if !success {
			_ = s.Close()
		}
	}()
	if err := os.Mkdir(s.dir, 0700); err != nil {
		return nil, err
	}
	for _, entry := range []struct {
		name string
		file **os.File
	}{{"data", &s.data}, {"records", &s.records}, {"parts", &s.index}} {
		m.mu.Lock()
		if s.removed {
			m.mu.Unlock()
			return nil, errSpillExpired
		}
		f, err := os.OpenFile(filepath.Join(s.dir, entry.name), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err == nil {
			*entry.file = f
			s.handles[f] = true
		}
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	var out io.Writer = s.data
	if m.wrapWriter != nil {
		out = m.wrapWriter(out)
	}
	c := markdownReadCapture{ctx: captureCtx, s: s, writer: captureWriter{s: s, out: out}}
	defer c.releaseOptions()
	err = read(captureCtx, func(tx *sql.Tx, seq Sequence) error {
		s.Sequence, c.tx = seq, tx
		page, err := c.identity(scope.Page)
		if err != nil {
			return err
		}
		if err := c.emit("", &page, nil); err != nil {
			return err
		}
		return c.forest(page, "", "")
	})
	if cause := context.Cause(captureCtx); cause != nil {
		err = cause
	}
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if s.removed {
		return nil, errSpillExpired
	}
	if !s.over {
		m.used += s.stored - s.reserved
	}
	s.reserved = s.stored
	if s.over && s.stored <= m.budget-m.used {
		s.over, m.over = false, nil
		m.used += s.stored
	}
	s.schemaNodes = c.units
	s.capturing = false
	s.ended = m.now()
	s.last = s.ended
	m.contentionLocked()
	m.admitLocked()
	success = true
	return s, nil
}

// markdownReadPage measures only newly appended JSON values, never the growing
// envelope. The reserve covers the cursor and the final offset/complete fields.
// marshal is explicit so tests can bound encoded work without timing assertions.
type markdownReadPage struct {
	result         *readResult
	text           strings.Builder
	bytes          int
	referenceBytes map[ID]int
	marshal        func(any) ([]byte, error)
}

func newMarkdownReadPage(result *readResult, marshal func(any) ([]byte, error)) (*markdownReadPage, error) {
	raw, err := marshal(result)
	if err != nil {
		return nil, err
	}
	return &markdownReadPage{result: result, bytes: len(raw), referenceBytes: map[ID]int{}, marshal: marshal}, nil
}

func (p *markdownReadPage) append(unit markdownReadUnit) (bool, error) {
	text, err := p.marshal(unit.Markdown)
	if err != nil {
		return false, err
	}
	delta := len(text) - 2 // The envelope already has the string's quotes.
	if len(unit.Warnings) > 0 {
		warnings, err := p.marshal(unit.Warnings)
		if err != nil {
			return false, err
		}
		delta += len(warnings) - 2 // Keep the existing array brackets.
		if len(p.result.Warnings) > 0 {
			delta++
		}
	}
	entryBytes := 0
	if p.result.References != nil && unit.Reference != nil {
		id := unit.Reference.Ref.ID
		key, err := p.marshal(id)
		if err != nil {
			return false, err
		}
		ref, err := p.marshal(unit.Reference)
		if err != nil {
			return false, err
		}
		entryBytes = len(key) + 1 + len(ref)
		if previous, exists := p.referenceBytes[id]; exists {
			delta += entryBytes - previous
		} else {
			delta += entryBytes
			if len(p.referenceBytes) == 0 {
				delta += len(`,"references":{}`)
			} else {
				delta++
			}
		}
	}
	if p.bytes+delta > readPageBytes-4096 {
		return false, nil
	}
	p.bytes += delta
	p.text.WriteString(unit.Markdown)
	p.result.Warnings = append(p.result.Warnings, unit.Warnings...)
	if entryBytes != 0 {
		id := unit.Reference.Ref.ID
		p.result.References[id] = *unit.Reference
		p.referenceBytes[id] = entryBytes
	}
	return true, nil
}

func (in *Instance) deliverMarkdownRead(ctx context.Context, s *Spill, meta readMetadata, p readPosition, public bool) ([]byte, error) {
	c, closeFiles, err := agentFiles(s)
	if err != nil {
		return nil, readFailure(ctx, err)
	}
	defer closeFiles()
	if p.Record > s.schemaNodes || p.Offset != 0 {
		return nil, expiredRead()
	}
	text := ""
	offset := int64(0)
	result := readResult{Markdown: &text, Offset: &offset, Warnings: []Warning{}}
	if !public {
		result.State = &State{Store: meta.Store, Seq: s.Sequence}
		result.Pending = &meta.Pending
		result.Revision, err = json.Marshal(meta.Revision)
		if err != nil {
			return nil, err
		}
		result.References = map[ID]AssignedRef{}
	}
	page, err := newMarkdownReadPage(&result, json.Marshal)
	if err != nil {
		return nil, err
	}
	for n := 0; n < meta.Limit && p.Record < s.schemaNodes; n++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var index [24]byte
		if _, err := c.s.records.ReadAt(index[:], p.Record*24); err != nil {
			return nil, readFailure(ctx, err)
		}
		start := int64(binary.LittleEndian.Uint64(index[:8]))
		length := int64(binary.LittleEndian.Uint64(index[8:16]))
		if n == 0 {
			offset = int64(binary.LittleEndian.Uint64(index[16:]))
		}
		var unit markdownReadUnit
		if err := json.NewDecoder(io.NewSectionReader(c.s.data, start, length)).Decode(&unit); err != nil {
			return nil, readFailure(ctx, err)
		}
		accepted, err := page.append(unit)
		if err != nil {
			return nil, err
		}
		if !accepted {
			if n == 0 {
				return nil, unavailable("snapshot")
			}
			break
		}
		p.Record++
	}
	text = page.text.String()
	result.Complete = p.Record == s.schemaNodes
	p.Page++
	if !result.Complete {
		cursor, err := encodeReadCursor(p, meta)
		if err != nil {
			return nil, err
		}
		result.Next = &cursor
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(out) >= readPageBytes {
		return nil, unavailable("snapshot")
	}
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if s.removed {
		return nil, expiredRead()
	}
	if !s.over || p.Page > s.high {
		s.last = m.now()
	}
	s.high = max(s.high, p.Page)
	if result.Complete {
		s.traversed = true
	}
	if s.over && s.traversed && m.waitingLocked() {
		m.removeLocked(s, errSpillExpired)
	}
	return out, nil
}
