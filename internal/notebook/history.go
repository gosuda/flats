package notebook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// HistoryBackend supplies retained immutable content under host-owned leases.
type HistoryBackend interface {
	Revisions(context.Context, int, int) (RevisionPage, error)
	RevisionSource(context.Context, int) (RevisionSource, func(), error)
	Pin(context.Context, int, bool) error
	Rollback(context.Context, int, string) (int, error)
	RevisionAsset(context.Context, string) (string, bool, error)
}

type RevisionSource struct {
	Number int
	Files  []File
	// Read is a compatibility fallback that materializes a whole blob; it is not bounded.
	Read func(context.Context, string) ([]byte, error)
	// Open supplies a bounded stream of a leased immutable blob.
	Open func(context.Context, string) (io.ReadCloser, error)
}

type RevisionInfo struct {
	Number  int          `json:"number"`
	Kind    string       `json:"kind"`
	Message string       `json:"message,omitempty"`
	Authors []string     `json:"authors,omitempty"`
	Pinned  bool         `json:"pinned"`
	Created time.Time    `json:"created"`
	Pages   []PageChange `json:"pages,omitempty"`
}

type PageChange struct {
	Page   ID     `json:"page"`
	Title  string `json:"title"`
	Change string `json:"change"`
}

type RevisionPage struct {
	Revisions []RevisionInfo `json:"revisions"`
	Next      int            `json:"next,omitempty"`
}

type revisionStore struct {
	store   *store
	dir     string
	files   []File
	sources []*os.File
}

func (s *revisionStore) close() error {
	var err error
	for _, file := range s.sources {
		err = errors.Join(err, file.Close())
	}
	s.sources = nil
	if s.store != nil {
		err = errors.Join(err, s.store.close())
		s.store = nil
	}
	return errors.Join(err, os.RemoveAll(s.dir))
}

type revisionPath struct {
	kind     string
	page     ID
	database ID
	bucket   string
	mime     string
}

func parseRevisionPath(path string) (revisionPath, error) {
	p := strings.Split(path, "/")
	var out revisionPath
	if path == "notebook.json" {
		out.kind = "notebook"
		return out, nil
	}
	if len(p) == 2 && p[0] == "assets" {
		ext := filepath.Ext(p[1])
		hash := strings.TrimSuffix(p[1], ext)
		out.mime = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}[ext]
		if out.mime == "" || validateContentHash(hash) != nil {
			return out, fmt.Errorf("invalid revision asset path")
		}
		out.kind = "asset"
		return out, nil
	}
	if len(p) < 3 || p[0] != "pages" {
		return out, fmt.Errorf("invalid revision content path")
	}
	out.page = ID(p[1])
	if err := out.page.ValidateKind(PageKind); err != nil {
		return out, err
	}
	switch {
	case len(p) == 3 && p[2] == "page.json":
		out.kind = "page"
	case len(p) == 4 && p[2] == "blocks":
		out.kind, out.bucket = "block", strings.TrimSuffix(p[3], ".json")
	case len(p) == 5 && p[2] == "databases" && p[4] == "schema.json":
		out.kind, out.database = "schema", ID(p[3])
	case len(p) == 6 && p[2] == "databases" && p[4] == "rows":
		out.kind, out.database, out.bucket = "row", ID(p[3]), strings.TrimSuffix(p[5], ".json")
	default:
		return out, fmt.Errorf("invalid revision content path")
	}
	if out.database != "" {
		if err := out.database.ValidateKind(DatabaseKind); err != nil {
			return out, err
		}
	}
	if out.bucket != "" && (len(out.bucket) != 2 || !strings.HasSuffix(path, ".json") || strings.Trim(out.bucket, "0123456789abcdef") != "") {
		return out, fmt.Errorf("invalid revision bucket")
	}
	if (out.kind == "block" || out.kind == "row") && len(out.bucket) != 2 {
		return out, fmt.Errorf("invalid revision bucket")
	}
	return out, nil
}

func checkedRevisionFiles(src RevisionSource) ([]File, error) {
	if src.Number <= 0 || src.Read == nil && src.Open == nil {
		return nil, &WriteError{Code: "invalid", Reason: "revision"}
	}
	files := slices.Clone(src.Files)
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	found := false
	for i, f := range files {
		p, err := parseRevisionPath(f.Path)
		if err != nil {
			return nil, err
		}
		if err := validateContentHash(f.Hash); err != nil {
			return nil, err
		}
		if f.Size < 0 || i > 0 && files[i-1].Path == f.Path {
			return nil, fmt.Errorf("invalid revision file list")
		}
		if p.kind == "notebook" {
			found = true
		}
		if p.kind == "asset" && !strings.HasPrefix(f.Path, "assets/"+f.Hash+".") {
			return nil, fmt.Errorf("revision asset hash mismatch")
		}
	}
	if !found {
		return nil, fmt.Errorf("revision has no notebook marker")
	}
	return files, nil
}

func openRevisionFile(ctx context.Context, src RevisionSource, hash string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if src.Open != nil {
		return src.Open(ctx, hash)
	}
	data, err := src.Read(ctx, hash)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func readRevisionFile(ctx context.Context, src RevisionSource, f File) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var data []byte
	var err error
	if src.Open == nil {
		data, err = src.Read(ctx, f.Hash)
	} else {
		var reader io.ReadCloser
		reader, err = openRevisionFile(ctx, src, f.Hash)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		data, err = io.ReadAll(reader)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) != f.Size {
		return nil, fmt.Errorf("revision content integrity failure")
	}
	hash := sha256.New()
	for offset := 0; offset < len(data); offset += spillChunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, _ = hash.Write(data[offset:min(offset+spillChunk, len(data))])
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != f.Hash {
		return nil, fmt.Errorf("revision content integrity failure")
	}
	return data, nil
}

// Materialized stores are used for mutations and Markdown export. Structured
// captures use disk-backed source ranges instead.
func openRevisionStore(ctx context.Context, src RevisionSource, selectFile func(revisionPath) bool) (*revisionStore, error) {
	return streamRevisionStore(ctx, src, selectFile, nil, nil)
}

func streamRevisionStore(ctx context.Context, src RevisionSource, selectFile func(revisionPath) bool, consume func(*sql.Tx, Item) error, charge func(string) error) (*revisionStore, error) {
	files, err := checkedRevisionFiles(src)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "flats-notebook-history-")
	if err != nil {
		return nil, unavailable("scratch")
	}
	s, _, err := openStore(filepath.Join(dir, dbName))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	out := &revisionStore{store: s, dir: dir, files: files}
	consumeItem := func(tx *sql.Tx, item Item) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if consume != nil {
			if err := consume(tx, item); err != nil {
				return err
			}
		}
		if charge != nil {
			return charge(dir)
		}
		return ctx.Err()
	}
	ordered := slices.Clone(files)
	rank := func(f File) int {
		p, _ := parseRevisionPath(f.Path)
		switch p.kind {
		case "notebook":
			return 0
		case "page":
			return 1
		case "block":
			return 2
		case "schema":
			return 3
		case "row":
			return 4
		}
		return 5
	}
	slices.SortStableFunc(ordered, func(a, b File) int { return rank(a) - rank(b) })
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		for _, f := range ordered {
			if err := ctx.Err(); err != nil {
				return err
			}
			p, _ := parseRevisionPath(f.Path)
			if p.kind == "asset" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO assets(hash,mime,width,height,bytes,live_refs,deleted_refs,protected_until) VALUES(?,?,0,0,?,0,0,0)`, f.Hash, p.mime, f.Size); err != nil {
					return err
				}
				continue
			}
			if p.kind != "notebook" && selectFile != nil && !selectFile(p) {
				continue
			}
			data, err := readRevisionFile(ctx, src, f)
			if err != nil {
				return err
			}
			if p.kind == "notebook" {
				var marker struct {
					Format int `json:"format"`
				}
				if err := strictDecode(data, &marker); err != nil || marker.Format != 1 {
					return fmt.Errorf("unsupported notebook revision format")
				}
				continue
			}
			if err := importRevisionFile(ctx, tx, p, data, consumeItem); err != nil {
				return fmt.Errorf("revision %s: %w", f.Path, err)
			}
			if charge != nil {
				if err := charge(dir); err != nil {
					return err
				}
			}
		}
		return setSequence(ctx, tx, 1)
	})
	if err != nil {
		return nil, errors.Join(err, out.close())
	}
	return out, nil
}

// revisionValue returns a view into data; decoding a RawMessage here would
// retain an additional copy of an entire oversized record or child array.
func revisionValue(ctx context.Context, data []byte) ([]byte, []byte, error) {
	data = bytes.Trim(data, " \t\r\n")
	depth, quoted, escaped := 0, false, false
	end := 0
	for ; end < len(data); end++ {
		if end%spillChunk == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		b := data[end]
		if quoted {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				quoted = false
			}
			continue
		}
		if depth == 0 && (b == ',' || b == '}' || b == ']') {
			break
		}
		switch b {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth > 128 {
				return nil, nil, fmt.Errorf("JSON nesting exceeds 128")
			}
		case '}', ']':
			depth--
		}
	}
	raw := bytes.Trim(data[:end], " \t\r\n")
	if !json.Valid(raw) {
		return nil, nil, fmt.Errorf("invalid revision JSON value")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return raw, data[end:], nil
}

func walkRevisionContainer(ctx context.Context, data []byte, object bool, visit func(string, []byte) error) error {
	data = bytes.Trim(data, " \t\r\n")
	open, close := byte('['), byte(']')
	if object {
		open, close = '{', '}'
	}
	if len(data) == 0 || data[0] != open {
		return fmt.Errorf("invalid revision container")
	}
	data = bytes.Trim(data[1:], " \t\r\n")
	if len(data) > 0 && data[0] == close {
		if len(bytes.Trim(data[1:], " \t\r\n")) != 0 {
			return fmt.Errorf("trailing revision content")
		}
		return ctx.Err()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := ""
		if object {
			// Content field names are short even when JSON-escaped.
			const maxKeyBytes = 6*len("property_order") + 2
			end, escaped := 1, false
			if len(data) == 0 || data[0] != '"' {
				return fmt.Errorf("invalid revision field")
			}
			for ; end < len(data) && end <= maxKeyBytes; end++ {
				if escaped {
					escaped = false
				} else if data[end] == '\\' {
					escaped = true
				} else if data[end] == '"' {
					break
				}
			}
			if end >= len(data) || end > maxKeyBytes {
				return fmt.Errorf("invalid revision field")
			}
			if err := strictDecode(data[:end+1], &key); err != nil {
				return err
			}
			data = bytes.Trim(data[end+1:], " \t\r\n")
			if len(data) == 0 || data[0] != ':' {
				return fmt.Errorf("invalid revision field separator")
			}
			data = data[1:]
		}
		raw, rest, err := revisionValue(ctx, data)
		if err != nil {
			return err
		}
		if err := visit(key, raw); err != nil {
			return err
		}
		data = bytes.Trim(rest, " \t\r\n")
		if len(data) == 0 {
			return fmt.Errorf("unterminated revision container")
		}
		if data[0] == close {
			if len(bytes.Trim(data[1:], " \t\r\n")) != 0 {
				return fmt.Errorf("trailing revision content")
			}
			return ctx.Err()
		}
		if data[0] != ',' {
			return fmt.Errorf("invalid revision container separator")
		}
		data = bytes.Trim(data[1:], " \t\r\n")
	}
}

func revisionFields(ctx context.Context, data []byte, allowed ...string) (map[string]json.RawMessage, error) {
	fields := make(map[string]json.RawMessage, len(allowed))
	err := walkRevisionContainer(ctx, data, true, func(key string, raw []byte) error {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("unknown revision field %q", key)
		}
		if _, exists := fields[key]; exists {
			return fmt.Errorf("duplicate revision field %q", key)
		}
		fields[key] = raw
		return nil
	})
	return fields, err
}

func importRevisionFile(ctx context.Context, tx *sql.Tx, p revisionPath, data []byte, consume func(*sql.Tx, Item) error) error {
	if p.kind == "page" {
		return importRevisionItem(ctx, tx, p, PageKind, data, consume)
	}
	if p.kind == "schema" {
		fields, err := revisionFields(ctx, data, "id", "block", "title", "property_order", "view_order", "properties", "options", "views")
		if err != nil {
			return err
		}
		for _, child := range []struct {
			key  string
			kind Kind
		}{{"properties", PropertyKind}, {"options", OptionKind}, {"views", ViewKind}} {
			if err := walkRevisionContainer(ctx, fields[child.key], false, func(_ string, raw []byte) error {
				return importRevisionItem(ctx, tx, p, child.kind, raw, consume)
			}); err != nil {
				return err
			}
			delete(fields, child.key)
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		return importRevisionItem(ctx, tx, p, DatabaseKind, raw, consume)
	}
	kind := BlockKind
	if p.kind == "row" {
		kind = RowKind
	}
	var previous ID
	return walkRevisionContainer(ctx, data, false, func(_ string, raw []byte) error {
		fields, err := revisionFields(ctx, raw, "id", "page", "parent", "position", "content", "database", "cells")
		if err != nil {
			return err
		}
		var id ID
		if err := strictDecode(fields["id"], &id); err != nil {
			return err
		}
		if previous != "" && id <= previous {
			return fmt.Errorf("revision bucket not in id order")
		}
		previous = id
		return importRevisionItem(ctx, tx, p, kind, raw, consume)
	})
}

func importRevisionItem(ctx context.Context, tx *sql.Tx, p revisionPath, kind Kind, raw []byte, consume func(*sql.Tx, Item) error) error {
	var data ItemData
	var targets map[string]any
	switch kind {
	case PageKind:
		d := &Page{}
		data, targets = d, map[string]any{"title": &d.Title, "parent": &d.Parent, "position": &d.Position}
	case BlockKind:
		d := &Block{}
		data, targets = d, map[string]any{"page": &d.Page, "parent": &d.Parent, "position": &d.Position, "content": &d.Content}
	case DatabaseKind:
		d := &Database{}
		data, targets = d, map[string]any{"block": &d.Block, "title": &d.Title, "property_order": &d.PropertyOrder, "view_order": &d.ViewOrder}
	case PropertyKind:
		d := &Property{}
		data, targets = d, map[string]any{"database": &d.Database, "definition": &d.Definition, "option_order": &d.OptionOrder}
	case OptionKind:
		d := &Option{}
		data, targets = d, map[string]any{"property": &d.Property, "name": &d.Name, "color": &d.Color}
	case RowKind:
		d := &Row{}
		data, targets = d, map[string]any{"database": &d.Database, "position": &d.Position, "cells": &d.Cells}
	case ViewKind:
		d := &View{}
		data, targets = d, map[string]any{"database": &d.Database, "definition": &d.Definition}
	default:
		return fmt.Errorf("unknown revision item kind")
	}
	allowed := []string{"id"}
	for key := range targets {
		allowed = append(allowed, key)
	}
	fields, err := revisionFields(ctx, raw, allowed...)
	if err != nil {
		return err
	}
	var id ID
	if err := strictDecode(fields["id"], &id); err != nil {
		return err
	}
	if err := id.ValidateKind(kind); err != nil {
		return err
	}
	for key, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		value, exists := fields[key]
		if !exists || key != "parent" && bytes.Equal(value, []byte("null")) {
			return fmt.Errorf("missing %s", key)
		}
		if err := strictDecode(value, target); err != nil {
			return err
		}
	}
	item := Item{Ref: Ref{ID: id, Inc: 1}, Ver: 1, V: map[GroupKey]Version{}}
	switch d := data.(type) {
	case *Page:
		item.Data = *d
	case *Block:
		item.Data = *d
	case *Database:
		item.Data = *d
	case *Property:
		item.Data = *d
	case *Option:
		item.Data = *d
	case *Row:
		item.Data = *d
	case *View:
		item.Data = *d
	}
	if err := item.Validate(); err != nil {
		return err
	}
	switch d := item.Data.(type) {
	case Page:
		if id != p.page {
			return fmt.Errorf("revision page path mismatch")
		}
	case Block:
		if d.Page != p.page || contentBucket(id) != p.bucket {
			return fmt.Errorf("revision block ownership mismatch")
		}
	case Database:
		if id != p.database {
			return fmt.Errorf("revision database path mismatch")
		}
	case Property:
		if d.Database != p.database {
			return fmt.Errorf("revision property ownership mismatch")
		}
	case View:
		if d.Database != p.database {
			return fmt.Errorf("revision view ownership mismatch")
		}
	case Row:
		if d.Database != p.database || contentBucket(id) != p.bucket {
			return fmt.Errorf("revision row ownership mismatch")
		}
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM allocated_ids WHERE id=?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return fmt.Errorf("duplicate revision identity")
	}
	if err := putItem(ctx, tx, item); err != nil {
		return err
	}
	if consume != nil {
		return consume(tx, item)
	}
	return nil
}

func (in *Instance) serveHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	fail := func(err error) {
		var e *WriteError
		if !errors.As(err, &e) {
			e = &WriteError{Code: "unavailable", Reason: "history", Retry: true}
		}
		status := http.StatusBadRequest
		switch e.Code {
		case "forbidden":
			status = 403
		case "not_found":
			status = 404
		case "conflict":
			status = 409
		case "notebook_capacity":
			status = 413
		case "unavailable":
			status = 503
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(e)
	}
	if r.Header.Get("X-Flats-Access") != "private" {
		fail(&WriteError{Code: "forbidden", Reason: "public"})
		return
	}
	b, ok := in.b.(HistoryBackend)
	if !ok {
		fail(unavailable("history_backend"))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(in.ctx, cancel)
	defer stop()
	if r.Method == http.MethodGet && r.URL.Path == "/_notebook/api/revisions" {
		for key, values := range r.URL.Query() {
			if (key != "before" && key != "limit") || len(values) != 1 {
				fail(&WriteError{Code: "invalid", Reason: "query"})
				return
			}
		}
		before, err := historyInt(r.URL.Query().Get("before"), 0)
		if err != nil {
			fail(err)
			return
		}
		limit, err := historyInt(r.URL.Query().Get("limit"), 50)
		if err != nil || limit < 1 || limit > 200 {
			fail(&WriteError{Code: "invalid", Reason: "limit"})
			return
		}
		page, err := b.Revisions(ctx, before, limit)
		if err != nil {
			fail(err)
			return
		}
		if page.Revisions == nil {
			page.Revisions = []RevisionInfo{}
		}
		_ = json.NewEncoder(w).Encode(page)
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/_notebook/api/revisions/") {
		n, err := historyInt(strings.TrimPrefix(r.URL.Path, "/_notebook/api/revisions/"), 0)
		if err != nil || n < 1 {
			fail(&WriteError{Code: "invalid", Reason: "revision"})
			return
		}
		query := map[string]json.RawMessage{}
		for key, values := range r.URL.Query() {
			if len(values) != 1 {
				fail(&WriteError{Code: "invalid", Reason: "query"})
				return
			}
			raw := []byte(values[0])
			if key != "limit" && key != "sort" && key != "filter" {
				raw, _ = json.Marshal(values[0])
			}
			query[key] = raw
		}
		raw, err := json.Marshal(query)
		if err != nil {
			fail(err)
			return
		}
		var data []byte
		if r.URL.Query().Has("cursor") {
			data, err = in.ReadRevision(ctx, RevisionSource{Number: n}, raw)
		} else {
			var src RevisionSource
			var release func()
			src, release, err = b.RevisionSource(ctx, n)
			if err == nil {
				defer release()
				data, err = in.ReadRevision(ctx, src, raw)
			}
		}
		if err != nil {
			fail(err)
			return
		}
		_, _ = w.Write(data)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(405)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	if r.Header.Get("X-Flats-Notebook") != "1" || r.Header.Get("Origin") != scheme+"://"+r.Host {
		fail(&WriteError{Code: "forbidden", Reason: "origin"})
		return
	}
	var body struct {
		Revision  int    `json:"revision"`
		Page      ID     `json:"page,omitempty"`
		Session   string `json:"session,omitempty"`
		Pinned    *bool  `json:"pinned,omitempty"`
		RequestID string `json:"request_id,omitempty"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		fail(&WriteError{Code: "invalid", Reason: "body"})
		return
	}
	if err := strictDecode(raw, &body); err != nil || body.Revision < 1 {
		fail(&WriteError{Code: "invalid", Reason: "body"})
		return
	}
	switch r.URL.Path {
	case "/_notebook/api/pin":
		if body.Pinned == nil || body.Page != "" || body.Session != "" || body.RequestID != "" {
			fail(&WriteError{Code: "invalid", Reason: "body"})
			return
		}
		if err := b.Pin(ctx, body.Revision, *body.Pinned); err != nil {
			fail(err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case "/_notebook/api/rollback":
		if body.Pinned != nil || body.Page != "" || body.Session != "" || body.RequestID != "" {
			fail(&WriteError{Code: "invalid", Reason: "body"})
			return
		}
		n, err := b.Rollback(ctx, body.Revision, "Browser")
		if err != nil {
			fail(err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": n, "revision_pending": n == 0})
	case "/_notebook/api/recover":
		if body.Pinned != nil || body.Page.ValidateKind(PageKind) != nil || body.Session == "" {
			fail(&WriteError{Code: "invalid", Reason: "body"})
			return
		}
		identity, hash, err := recoveryIdentity(body.Revision, body.Page, body.Session, body.RequestID)
		if err != nil {
			fail(err)
			return
		}
		result, found, err := in.replayUpdate(ctx, identity, hash)
		if err == nil && !found {
			var src RevisionSource
			var release func()
			src, release, err = b.RevisionSource(ctx, body.Revision)
			if err == nil {
				defer release()
				result, err = in.Recover(ctx, src, body.Page, Placement{}, body.Session, body.RequestID)
			}
		}
		if err != nil {
			fail(err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"root": result.Refs["root"].Ref, "revision": result.Revision, "revision_pending": result.RevisionPending})
	default:
		w.WriteHeader(404)
	}
}

func historyInt(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || strconv.Itoa(n) != raw {
		return 0, &WriteError{Code: "invalid", Reason: "number"}
	}
	return n, nil
}

var historyRuntimes sync.Map

type historyRuntime struct {
	mu       sync.Mutex
	manager  *SpillManager
	dir      string
	closed   bool
	captures int
	reads    map[string]*historyRead
}

type historyRead struct {
	spill     *Spill
	revision  int
	scope     Scope
	limit     int
	key       string
	at        time.Time
	markdown  bool
	assetURLs map[string]string
}

type historyQuery struct {
	Page     json.RawMessage `json:"page,omitempty"`
	Database json.RawMessage `json:"database,omitempty"`
	View     json.RawMessage `json:"view,omitempty"`
	Sort     []Sort          `json:"sort,omitempty"`
	Filter   []Filter        `json:"filter,omitempty"`
	Format   string          `json:"format,omitempty"`
	Limit    int             `json:"limit,omitempty"`
	Cursor   string          `json:"cursor,omitempty"`
}

// useHistorySpillManager is called during instance setup, before admission.
// The transport owner retains directory cleanup for a shared manager.
func (in *Instance) useHistorySpillManager(manager *SpillManager) {
	state := &historyRuntime{dir: manager.dir, manager: manager, reads: map[string]*historyRead{}}
	historyRuntimes.Store(in, state)
	context.AfterFunc(in.ctx, func() {
		state.mu.Lock()
		state.closed = true
		state.reads = nil
		state.mu.Unlock()
		manager.CancelScope(func(Scope) bool { return true })
		manager.mu.Lock()
		delete(manager.owners, manager)
		manager.scheduleLocked()
		manager.mu.Unlock()
		historyRuntimes.CompareAndDelete(in, state)
	})
}

func (in *Instance) historyRuntime() (*historyRuntime, error) {
	if existing, ok := historyRuntimes.Load(in); ok {
		return existing.(*historyRuntime), nil
	}
	return nil, unavailable("stopped")
}

func historySelector(raw json.RawMessage, kind Kind) (ID, error) {
	var id ID
	if len(raw) == 0 {
		return id, nil
	}
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &id); err != nil {
			return "", err
		}
	} else {
		var ref Ref
		if err := strictDecode(raw, &ref); err != nil {
			return "", err
		}
		id = ref.ID
	}
	return id, id.ValidateKind(kind)
}

func (runtime *historyRuntime) captureRevision(ctx context.Context, src RevisionSource, q historyQuery, scope Scope, selectFile func(revisionPath) bool) (spill *Spill, source *revisionStore, resultErr error) {
	files, err := checkedRevisionFiles(src)
	if err != nil {
		return nil, nil, err
	}
	var reservation int64
	for _, f := range files {
		p, _ := parseRevisionPath(f.Path)
		if p.kind == "notebook" || selectFile(p) {
			n, err := quotaAdd(reservation, f.Size)
			if err != nil {
				return nil, nil, err
			}
			reservation = n
		}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	s, err := runtime.manager.acquire(ctx, reservation, scope)
	if err != nil {
		cancel(err)
		return nil, nil, err
	}
	m := s.manager
	m.mu.Lock()
	s.cancel = cancel
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
			m.mu.Lock()
			m.removeLocked(s, errSpillUnavailable)
			m.mu.Unlock()
			_ = os.RemoveAll(s.dir)
			if source != nil {
				_ = source.close()
				source = nil
			}
		}
		cancel(nil)
	}()
	if err := os.Mkdir(s.dir, 0700); err != nil {
		return nil, nil, unavailable("scratch")
	}
	for _, f := range []struct {
		name   string
		target **os.File
	}{{"data", &s.data}, {"records", &s.records}, {"parts", &s.index}} {
		*f.target, err = os.OpenFile(filepath.Join(s.dir, f.name), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, nil, unavailable("scratch")
		}
		m.mu.Lock()
		s.handles[*f.target] = true
		m.mu.Unlock()
	}
	identityFile, err := os.OpenFile(filepath.Join(s.dir, "ids"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	defer identityFile.Close()
	c := &spillCapture{s: s, writer: captureWriter{s: s, out: s.data}, root: -1, order: OrderRows}
	configured := false
	configure := func(tx *sql.Tx) error {
		if configured || scope.Database == "" {
			return nil
		}
		configured = true
		r, err := loadRecord(ctx, tx, scope.Database)
		if errors.Is(err, sql.ErrNoRows) {
			return &WriteError{Code: "not_found", Reason: "database"}
		}
		if err != nil {
			return err
		}
		d := r.Item.Data.(Database)
		if q.Format != "markdown" {
			var first ID
			if err := tx.QueryRowContext(ctx, `SELECT id FROM history_orders WHERE owner=? AND kind='property_order' ORDER BY ordinal LIMIT 1`, scope.Database).Scan(&first); err != nil {
				return invalidStructure()
			}
			d.PropertyOrder = []ID{first}
		}
		if len(d.PropertyOrder) == 0 {
			return invalidStructure()
		}
		c.schema = Schema{Database: scope.Database, Options: map[ID][]Option{}}
		c.schema.optionRank = func(property, option ID) int {
			var rank int
			err := tx.QueryRowContext(ctx, `SELECT ordinal FROM history_orders WHERE owner=? AND kind='option_order' AND id=?`, property, option).Scan(&rank)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				c.queryError = err
			}
			return rank
		}
		if len(q.View) > 0 {
			id, err := historySelector(q.View, ViewKind)
			if err != nil {
				return err
			}
			v, err := loadRecord(ctx, tx, id)
			if err != nil {
				return err
			}
			view := v.Item.Data.(View)
			if view.Database != scope.Database {
				return &WriteError{Code: "invalid", Reason: "view_owner"}
			}
			c.view, err = view.Definition.Wire(func(id ID) (Ref, error) { return Ref{ID: id, Inc: 1}, nil })
			if err != nil {
				return err
			}
			if c.view.Display.Layout == "table" && len(c.view.Display.Visible) == 0 {
				c.view.Display.Visible = []Target{{Ref: &Ref{ID: d.PropertyOrder[0], Inc: 1}}}
			}
		} else {
			c.view = ViewDef{Name: "History", Display: Display{Layout: "table", Visible: []Target{{Ref: &Ref{ID: d.PropertyOrder[0], Inc: 1}}}, Widths: []Width{}}, Sort: append([]Sort{}, q.Sort...), Filter: append([]Filter{}, q.Filter...)}
		}
		wanted := map[ID]bool{}
		for _, key := range c.view.Sort {
			if key.Property.Ref != nil {
				wanted[key.Property.Ref.ID] = true
			}
		}
		for _, key := range c.view.Filter {
			if key.Property.Ref != nil {
				wanted[key.Property.Ref.ID] = true
			}
		}
		for id := range wanted {
			p, err := loadRecord(ctx, tx, id)
			if err != nil {
				return err
			}
			c.schema.PropertyIDs = append(c.schema.PropertyIDs, id)
			c.schema.Properties = append(c.schema.Properties, p.Item.Data.(Property))
		}
		e := &edit{ctx: ctx, tx: tx}
		_, _, err = e.schemaView(scope.Database, c.view, false, false)
		return err
	}
	var sourceBytes int64
	charge := func(dir string) error {
		var size int64
		for _, suffix := range []string{"", "-wal", "-shm"} {
			info, err := os.Stat(filepath.Join(dir, dbName) + suffix)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			size += info.Size()
		}
		if size > sourceBytes {
			if err := s.charge(size - sourceBytes); err != nil {
				return err
			}
			sourceBytes = size
		}
		return nil
	}
	consumeRange := func(tx *sql.Tx, item Item, fields map[string]revisionRange) error {
		include := false
		row := item.Kind() == RowKind
		switch {
		case scope.Tree:
			include = item.Kind() == PageKind
		case scope.Page != "":
			switch d := item.Data.(type) {
			case Page:
				include = item.ID() == scope.Page
			case Block:
				include = d.Page == scope.Page
			default:
				include = true
			}
		case scope.Database != "":
			include = item.Kind() != PageKind && item.Kind() != BlockKind
		}
		if item.Kind() == RowKind && scope.Page != "" && q.Format != "markdown" {
			include = false
		}
		if !include {
			return nil
		}
		if row {
			if err := configure(tx); err != nil {
				return err
			}
			c.schemaNodes = c.nodes - c.rowCount
		}
		before := c.nodes
		add := func() error { return c.add(item, row && scope.Database != "") }
		if fields != nil {
			if row && scope.Database != "" {
				var err error
				item, err = c.revisionQueryItem(ctx, tx, item, fields)
				if err != nil {
					return err
				}
			}
			add = func() error {
				if captured, ok := fields["$capture"]; ok {
					return c.addRevisionCaptured(item, row && scope.Database != "", captured)
				}
				return c.addEncoded(item, row && scope.Database != "", func(w io.Writer) error { return writeRevisionRangeItem(w, item, fields) })
			}
		}
		if err := add(); err != nil {
			return err
		}
		if c.nodes > before {
			var slot [32]byte
			copy(slot[:], string(item.ID()))
			if err := s.charge(32); err != nil {
				return err
			}
			_, err := identityFile.Write(slot[:])
			return err
		}
		return nil
	}
	if q.Format == "markdown" {
		source, err = streamRevisionStore(ctx, src, selectFile, func(tx *sql.Tx, item Item) error { return consumeRange(tx, item, nil) }, charge)
	} else {
		source, err = streamRevisionCaptureStore(ctx, src, selectFile, c, consumeRange, charge)
	}
	if err != nil {
		return nil, nil, err
	}
	if !configured && scope.Database != "" {
		if err := source.store.transaction(ctx, configure); err != nil {
			return nil, source, err
		}
	}
	if scope.Database == "" || c.rowCount == 0 {
		c.schemaNodes = c.nodes
	}
	s.Sequence = 1
	if err := charge(source.dir); err != nil {
		return nil, source, err
	}
	if q.Format == "markdown" {
		out, err := os.OpenFile(filepath.Join(s.dir, "markdown"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, source, err
		}
		writer := &captureWriter{s: s, out: out}
		warnings, err := os.OpenFile(filepath.Join(s.dir, "warnings"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			_ = out.Close()
			return nil, source, err
		}
		warningsWriter := &captureWriter{s: s, out: warnings}
		err = source.store.transaction(ctx, func(tx *sql.Tx) error { return writeHistoryMarkdown(ctx, tx, scope.Page, writer, warningsWriter) })
		err = errors.Join(err, warnings.Close())
		err = errors.Join(err, out.Close())
		if err != nil {
			return nil, source, err
		}
	}
	if err := c.finish(ctx); err != nil {
		return nil, source, err
	}
	if err := ctx.Err(); err != nil {
		return nil, source, err
	}
	selected := scope.Page
	if selected == "" {
		selected = scope.Database
	}
	if selected != "" {
		if _, err := source.store.loadItem(ctx, selected); err != nil {
			return nil, source, &WriteError{Code: "not_found", Reason: "item"}
		}
	}
	if err := source.close(); err != nil {
		return nil, source, err
	}
	var actual int64
	for _, name := range []string{"data", "records", "parts", "ids", "markdown", "warnings"} {
		info, err := os.Stat(filepath.Join(s.dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, source, err
		}
		actual += info.Size()
	}
	m.mu.Lock()
	m.expireLocked()
	if s.removed {
		m.mu.Unlock()
		return nil, source, unavailable("history_capture")
	}
	if s.over {
		if actual <= m.budget-m.used {
			s.over = false
			m.over = nil
			m.used += actual
			m.admitLocked()
		}
	} else {
		m.used += actual - s.reserved
	}
	s.reserved = actual
	s.stored = actual
	s.capturing = false
	s.ended = m.now()
	s.last = s.ended
	m.contentionLocked()
	m.mu.Unlock()
	success = true
	return s, source, nil
}

// ReadRevision captures retained content into the same disk-backed scope
// reader used for current content. Cursor delivery never reopens the source.
func (in *Instance) ReadRevision(ctx context.Context, src RevisionSource, req []byte) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(in.ctx, cancel)
	defer stop()
	if in.ctx.Err() != nil {
		return nil, unavailable("stopped")
	}
	var q historyQuery
	if err := strictDecode(req, &q); err != nil {
		return nil, &WriteError{Code: "invalid", Reason: "query"}
	}
	var fields map[string]json.RawMessage
	if err := strictDecode(req, &fields); err != nil || fields == nil {
		return nil, &WriteError{Code: "invalid", Reason: "query"}
	}
	runtime, err := in.historyRuntime()
	if err != nil {
		return nil, err
	}
	if q.Cursor != "" {
		if len(fields) != 1 {
			return nil, &WriteError{Code: "invalid", Reason: "cursor_query"}
		}
		return in.historyCursor(ctx, runtime, src.Number, q.Cursor)
	}
	if _, ok := fields["cursor"]; ok {
		return nil, &WriteError{Code: "invalid", Reason: "cursor"}
	}
	if q.Limit == 0 {
		if _, ok := fields["limit"]; ok {
			return nil, &WriteError{Code: "invalid", Reason: "limit"}
		}
		q.Limit = 500
	}
	if q.Limit < 1 || q.Limit > 2000 || q.Format != "" && q.Format != "structured" && q.Format != "markdown" {
		return nil, &WriteError{Code: "invalid", Reason: "query"}
	}
	page, err := historySelector(q.Page, PageKind)
	if err != nil {
		return nil, &WriteError{Code: "invalid", Reason: "page"}
	}
	database, err := historySelector(q.Database, DatabaseKind)
	if err != nil {
		return nil, &WriteError{Code: "invalid", Reason: "database"}
	}
	if page != "" && database != "" || q.Format == "markdown" && page == "" || database == "" && (len(q.View) > 0 || q.Sort != nil || q.Filter != nil) || len(q.View) > 0 && (q.Sort != nil || q.Filter != nil) {
		return nil, &WriteError{Code: "invalid", Reason: "selector"}
	}
	scope := Scope{Tree: page == "" && database == "", Page: page, Database: database}
	if database != "" {
		scope.View = &ViewDef{}
	}
	selectFile := func(p revisionPath) bool {
		if scope.Tree {
			return p.kind == "page"
		}
		if database != "" {
			return p.database == database
		}
		return p.page == page && (p.kind != "row" || q.Format == "markdown")
	}
	runtime.mu.Lock()
	if runtime.closed || runtime.captures >= 4 {
		runtime.mu.Unlock()
		return nil, unavailable("history_read_limit")
	}
	runtime.captures++
	if len(runtime.reads)+runtime.captures > 4 {
		var oldest string
		for token, r := range runtime.reads {
			if oldest == "" || r.at.Before(runtime.reads[oldest].at) {
				oldest = token
			}
		}
		if oldest != "" {
			_ = runtime.reads[oldest].spill.Close()
			delete(runtime.reads, oldest)
		}
	}
	runtime.mu.Unlock()
	defer func() { runtime.mu.Lock(); runtime.captures--; runtime.mu.Unlock() }()
	assets := map[string]string{}
	hashes := []string{}
	for _, f := range src.Files {
		p, err := parseRevisionPath(f.Path)
		if err == nil && p.kind == "asset" {
			assets[f.Hash] = "/_notebook/a/" + filepath.Base(f.Path)
			hashes = append(hashes, f.Hash)
		}
	}
	release := sync.OnceFunc(in.b.Lease(hashes))
	retained := false
	defer func() {
		if !retained {
			release()
		}
	}()
	spill, source, err := runtime.captureRevision(ctx, src, q, scope, selectFile)
	if err != nil {
		return nil, permanentWriteError(err)
	}
	defer source.close()
	m := spill.manager
	m.mu.Lock()
	if spill.removed {
		m.mu.Unlock()
		return nil, expiredRead()
	}
	cancelCapture := spill.cancel
	spill.cancel = func(cause error) {
		if cancelCapture != nil {
			cancelCapture(cause)
		}
		release()
	}
	retained = true
	m.mu.Unlock()
	key, err := randomNotebookToken()
	if err != nil {
		_ = spill.Close()
		return nil, err
	}
	capture := &historyRead{spill: spill, revision: src.Number, scope: scope, limit: q.Limit, key: key, at: in.now(), markdown: q.Format == "markdown", assetURLs: assets}
	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		_ = spill.Close()
		return nil, unavailable("stopped")
	}
	// Four immutable reads per notebook; eviction is by capture age, not use.
	if len(runtime.reads) >= 4 {
		var oldest string
		for token, r := range runtime.reads {
			if oldest == "" || r.at.Before(runtime.reads[oldest].at) {
				oldest = token
			}
		}
		_ = runtime.reads[oldest].spill.Close()
		delete(runtime.reads, oldest)
	}
	runtime.reads[spill.Cursor()] = capture
	runtime.mu.Unlock()
	return in.historyPage(ctx, runtime, capture, 0, 0)
}

func (in *Instance) historyAsset(hash string) (string, bool) {
	state, ok := historyRuntimes.Load(in)
	if !ok {
		return "", false
	}
	runtime := state.(*historyRuntime)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	for token, read := range runtime.reads {
		if _, live := runtime.manager.Lookup(token); !live {
			continue
		}
		if url, ok := read.assetURLs[hash]; ok {
			mime := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}[filepath.Ext(url)]
			return mime, mime != ""
		}
	}
	return "", false
}

func writeHistoryMarkdown(ctx context.Context, tx *sql.Tx, page ID, out io.Writer, warnings io.Writer) error {
	root, err := loadRecord(ctx, tx, page)
	if err != nil {
		return err
	}
	var forest func(*ID, string, int) error
	forest = func(parent *ID, indent string, depth int) error {
		if depth > 6 {
			return invalidStructure()
		}
		var position string
		var after ID
		for {
			var id ID
			err := tx.QueryRowContext(ctx, `SELECT id FROM blocks WHERE page=? AND parent IS ? AND (position>? OR (position=? AND id>?)) ORDER BY position,id LIMIT 1`, page, parent, position, position, after).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			r, err := loadRecord(ctx, tx, id)
			if err != nil {
				return err
			}
			block := r.Item.Data.(Block)
			position, after = block.Position, id
			if block.Content.Kind == InlineDatabase {
				if _, err := fmt.Fprintf(out, "%s<!-- flats:block %s -->\n", indent, id); err != nil {
					return err
				}
				var lossy bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM properties p JOIN items i ON i.id=p.id WHERE p.database_id=? AND json_extract(i.data,'$.definition.type') NOT IN ('title','text'))`, block.Content.Database).Scan(&lossy); err != nil {
					return err
				}
				if lossy {
					if err := json.NewEncoder(warnings).Encode(Warning{Code: "database_display_text", Path: string(id), Message: "Non-text database cells become display text in a Markdown table."}); err != nil {
						return err
					}
				}
				if err := writeHistoryTable(ctx, tx, block.Content.Database, indent, out); err != nil {
					return err
				}
				if _, err := io.WriteString(out, "\n\n"); err != nil {
					return err
				}
			} else {
				local := r.Item
				block.Parent = nil
				local.Data = block
				exporter := markdownExporter{page: root.Item, children: map[ID][]Item{"": {local}}, seen: map[ID]bool{}}
				if _, err := io.WriteString(out, exporter.forest("", indent)); err != nil {
					return err
				}
				for _, warning := range exporter.warnings {
					if err := json.NewEncoder(warnings).Encode(warning); err != nil {
						return err
					}
				}
			}
			childIndent := indent
			if block.Content.Kind.HoldsChildren() && block.Content.Kind != Toggle {
				childIndent += "  "
				if block.Content.Kind == Numbered {
					childIndent += " "
				}
			}
			var children int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM blocks WHERE parent=?`, id).Scan(&children); err != nil {
				return err
			}
			if children > 0 {
				if err := forest(&id, childIndent, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return forest(nil, "", 1)
}

func writeHistoryTable(ctx context.Context, tx *sql.Tx, database ID, indent string, out io.Writer) error {
	r, err := loadRecord(ctx, tx, database)
	if err != nil {
		return err
	}
	d := r.Item.Data.(Database)
	properties := []Property{}
	options := map[ID][]Option{}
	header, separator := []string{}, []string{}
	for _, id := range d.PropertyOrder {
		r, err := loadRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		p := r.Item.Data.(Property)
		properties = append(properties, p)
		header = append(header, markdownEscape(p.Definition.Name))
		separator = append(separator, "---")
		for _, oid := range p.OptionOrder {
			r, err := loadRecord(ctx, tx, oid)
			if err != nil {
				return err
			}
			options[id] = append(options[id], r.Item.Data.(Option))
		}
	}
	writeRow := func(cells []string) error {
		_, err := io.WriteString(out, indent+"| "+strings.Join(cells, " | ")+" |\n")
		return err
	}
	if err := writeRow(header); err != nil {
		return err
	}
	if err := writeRow(separator); err != nil {
		return err
	}
	var position string
	var after ID
	for {
		var id ID
		err := tx.QueryRowContext(ctx, `SELECT id FROM rows WHERE database_id=? AND (position>? OR (position=? AND id>?)) ORDER BY position,id LIMIT 1`, database, position, position, after).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r, err := loadRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		row := r.Item.Data.(Row)
		position, after = row.Position, id
		cells := make([]string, len(properties))
		for i, p := range properties {
			pid := d.PropertyOrder[i]
			cells[i] = strings.ReplaceAll(markdownEscape(markdownCell(row.Cells[pid], p, options[pid])), "\n", " ")
		}
		if err := writeRow(cells); err != nil {
			return err
		}
	}
}

func historyCursorValue(read *historyRead, record, offset int64) string {
	body := read.spill.Cursor() + ":" + strconv.FormatInt(record, 10) + ":" + strconv.FormatInt(offset, 10)
	mac := hmac.New(sha256.New, []byte(read.key))
	_, _ = mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString([]byte(body)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (in *Instance) historyCursor(ctx context.Context, runtime *historyRuntime, revision int, cursor string) ([]byte, error) {
	expired := func() ([]byte, error) {
		return nil, &WriteError{Code: "conflict", Reason: "read_expired", Detail: "cursor expired; read again"}
	}
	p := strings.Split(cursor, ".")
	if len(p) != 2 || len(cursor) > 2048 {
		return expired()
	}
	raw, err := base64.RawURLEncoding.DecodeString(p[0])
	if err != nil {
		return expired()
	}
	fields := strings.Split(string(raw), ":")
	if len(fields) != 3 {
		return expired()
	}
	runtime.mu.Lock()
	read := runtime.reads[fields[0]]
	runtime.mu.Unlock()
	if read == nil || revision != 0 && read.revision != revision {
		return expired()
	}
	record, e1 := strconv.ParseInt(fields[1], 10, 64)
	offset, e2 := strconv.ParseInt(fields[2], 10, 64)
	if e1 != nil || e2 != nil || record < 0 || offset < 0 || !hmac.Equal([]byte(historyCursorValue(read, record, offset)), []byte(cursor)) {
		return expired()
	}
	return in.historyPage(ctx, runtime, read, record, offset)
}

func historyNodeID(s *Spill, c *spillCapture, node spillNode, total int64) (ID, error) {
	lo, hi := int64(0), total
	for lo < hi {
		mid := (lo + hi) / 2
		n, err := c.node(mid)
		if err != nil {
			return "", err
		}
		if n.start < node.start {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	f, err := os.Open(filepath.Join(s.dir, "ids"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	var slot [32]byte
	if _, err := f.ReadAt(slot[:], lo*32); err != nil {
		return "", err
	}
	id := ID(bytes.TrimRight(slot[:], "\x00"))
	return id, id.Validate()
}

func (in *Instance) historyPage(ctx context.Context, runtime *historyRuntime, read *historyRead, record, offset int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := read.spill
	m := s.manager
	m.mu.Lock()
	m.expireLocked()
	if s.removed {
		m.mu.Unlock()
		return nil, &WriteError{Code: "conflict", Reason: "read_expired", Detail: "cursor expired; read again"}
	}
	f, err := os.Open(filepath.Join(s.dir, "data"))
	if err != nil {
		m.mu.Unlock()
		return nil, unavailable("scratch")
	}
	s.handles[f] = true
	records, err := os.Open(filepath.Join(s.dir, "records"))
	if err != nil {
		delete(s.handles, f)
		_ = f.Close()
		m.mu.Unlock()
		return nil, unavailable("scratch")
	}
	s.handles[records] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(s.handles, f)
		delete(s.handles, records)
		_ = f.Close()
		_ = records.Close()
		m.mu.Unlock()
	}()
	c := &spillCapture{s: &Spill{data: f, records: records, scope: s.scope}, root: s.root, schemaNodes: s.schemaNodes}
	total := s.schemaNodes
	if read.scope.Database != "" {
		node, err := c.summary(c.root)
		if err != nil {
			return nil, err
		}
		total += node.rows
	}
	result := map[string]any{"state": State{Store: in.storeID, Seq: 1}, "revision": read.revision, "pending": false, "next": nil, "complete": false, "warnings": []Warning{}}
	const partBytes = 128 << 10
	nextRecord, nextOffset := record, offset
	markdownComplete := true
	if read.markdown {
		markdown, err := os.Open(filepath.Join(s.dir, "markdown"))
		if err != nil {
			return nil, unavailable("scratch")
		}
		defer markdown.Close()
		info, err := markdown.Stat()
		if err != nil {
			return nil, err
		}
		if offset > info.Size() {
			return nil, &WriteError{Code: "invalid", Reason: "cursor"}
		}
		data := make([]byte, min(int64(partBytes+4), info.Size()-offset))
		if len(data) > 0 {
			if _, err := markdown.ReadAt(data, offset); err != nil {
				return nil, err
			}
		}
		n := min(partBytes, len(data))
		for n < len(data) && !utf8.RuneStart(data[n]) {
			n++
		}
		result["markdown"] = string(data[:n])
		result["offset"] = offset
		result["total_bytes"] = info.Size()
		nextOffset = offset + int64(n)
		identities, err := os.Open(filepath.Join(s.dir, "ids"))
		if err != nil {
			return nil, unavailable("scratch")
		}
		defer identities.Close()
		references := []map[string]any{}
		referenceIDs := map[string]bool{}
		for nextRecord < total && len(references) < read.limit {
			var slot [32]byte
			if _, err := identities.ReadAt(slot[:], nextRecord*32); err != nil {
				return nil, err
			}
			id := ID(bytes.TrimRight(slot[:], "\x00"))
			if err := id.Validate(); err != nil {
				return nil, err
			}
			marker := string(id)
			if id.Kind() == BlockKind {
				marker = "<!-- flats:block " + string(id) + " -->"
			}
			references = append(references, map[string]any{"marker": marker, "ref": Ref{ID: id, Inc: 1}, "ver": Version(1)})
			referenceIDs[string(id)] = true
			nextRecord++
		}
		result["references"] = references
		warnings, err := os.Open(filepath.Join(s.dir, "warnings"))
		if err != nil {
			return nil, unavailable("scratch")
		}
		defer warnings.Close()
		decoder := json.NewDecoder(warnings)
		pageWarnings := []Warning{}
		for {
			var warning Warning
			err := decoder.Decode(&warning)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if referenceIDs[warning.Path] {
				pageWarnings = append(pageWarnings, warning)
			}
		}
		result["warnings"] = pageWarnings
		markdownComplete = nextOffset == info.Size()
	} else {
		items := []json.RawMessage{}
		schema := []json.RawMessage{}
		rows := []json.RawMessage{}
		size := 0
		for nextRecord < total && len(items)+len(schema)+len(rows) < read.limit {
			row := read.scope.Database != "" && nextRecord >= s.schemaNodes
			index := nextRecord
			if row {
				index -= s.schemaNodes
			}
			node, err := c.at(row, index)
			if err != nil {
				return nil, err
			}
			if node.length > (1<<20)-4096 {
				if len(items)+len(schema)+len(rows) > 0 {
					break
				}
				end := min(nextOffset+partBytes, node.length)
				data := make([]byte, end-nextOffset+min(int64(4), node.length-end))
				if _, err := f.ReadAt(data, node.start+nextOffset); err != nil {
					return nil, err
				}
				n := int(end - nextOffset)
				for n < len(data) && !utf8.RuneStart(data[n]) {
					n++
				}
				id, err := historyNodeID(s, c, node, total)
				if err != nil {
					return nil, err
				}
				result["continuation"] = map[string]any{"record": id, "part": nextOffset / partBytes, "parts": (node.length + partBytes - 1) / partBytes, "data": string(data[:n])}
				nextOffset += int64(n)
				if nextOffset == node.length {
					nextRecord++
					nextOffset = 0
				}
				break
			}
			if size+int(node.length) > (1<<20)-4096 {
				break
			}
			data := make([]byte, node.length)
			if _, err := f.ReadAt(data, node.start); err != nil {
				return nil, err
			}
			if read.scope.Database != "" {
				if row {
					rows = append(rows, data)
				} else {
					schema = append(schema, data)
				}
			} else {
				items = append(items, data)
			}
			var block struct {
				Content struct {
					Kind  string `json:"kind"`
					Asset string `json:"asset"`
				} `json:"content"`
			}
			if err := json.Unmarshal(data, &block); err != nil {
				return nil, err
			}
			if block.Content.Kind == "image" {
				assets, ok := result["assets"].(map[string]string)
				if !ok {
					assets = map[string]string{}
					result["assets"] = assets
				}
				assets[block.Content.Asset] = read.assetURLs[block.Content.Asset]
			}
			size += len(data)
			nextRecord++
		}
		if read.scope.Tree {
			result["tree"] = items
		} else if read.scope.Page != "" {
			result["page"] = map[string]any{"items": items}
		} else {
			result["schema"] = schema
			result["rows"] = rows
		}
	}
	complete := nextRecord >= total && markdownComplete
	result["complete"] = complete
	if !complete {
		result["next"] = historyCursorValue(read, nextRecord, nextOffset)
	}
	// The shared spill's forward-part clock also covers record pagination.
	progress := int(nextRecord*(1<<20) + nextOffset/partBytes)
	m.mu.Lock()
	if progress > s.high {
		s.high = progress
		s.last = m.now()
	}
	if !s.over {
		s.last = m.now()
	}
	if complete {
		s.traversed = true
	}
	if s.over && s.traversed && m.waitingLocked() {
		m.removeLocked(s, errSpillExpired)
	}
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}
