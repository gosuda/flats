package notebook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const notebookAppPrefix = "/_notebook/app/"
const notebookCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' blob:; font-src 'none'; frame-src 'none'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"

var notebookApp = sync.OnceValues(func() (*notebookBundle, error) { return loadNotebookApp(webFS) })

type notebookBundle struct {
	version string
	shell   []byte
	files   map[string][]byte
}

func loadNotebookApp(source fs.FS) (*notebookBundle, error) {
	b := &notebookBundle{files: map[string][]byte{}}
	hash := sha256.New()
	err := fs.WalkDir(source, "web", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name != "web" {
				return fs.SkipDir
			}
			return nil
		}
		ext := path.Ext(name)
		if ext != ".js" && ext != ".css" && name != "web/index.html" {
			return nil
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		name = strings.TrimPrefix(name, "web/")
		fmt.Fprintf(hash, "%d:%s:%d:", len(name), name, len(data))
		hash.Write(data)
		if name == "index.html" {
			b.shell = data
		} else {
			b.files[name] = data
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(b.shell) == 0 {
		return nil, fmt.Errorf("notebook shell is missing")
	}
	b.version = hex.EncodeToString(hash.Sum(nil)[:16])
	prefix := notebookAppPrefix + b.version + "/"
	b.shell = bytes.ReplaceAll(b.shell, []byte(notebookAppPrefix), []byte(prefix))
	for name, data := range b.files {
		b.files[name] = bytes.ReplaceAll(data, []byte(notebookAppPrefix), []byte(prefix))
	}
	return b, nil
}

// ServeHTTP serves the notebook on a host-authorized flat origin.
func (in *Instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := r.Context().Err(); err != nil {
		writeNotebookError(w, err)
		return
	}
	if in.ctx.Err() != nil {
		writeNotebookError(w, unavailable("stopped"))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(in.ctx, cancel)
	defer stop()
	r = r.WithContext(ctx)
	public := !notebookPrivate(r)
	p := r.URL.Path
	switch {
	case p == "/":
		in.serveNotebookApp(w, r, "")
	case strings.HasPrefix(p, notebookAppPrefix):
		in.serveNotebookApp(w, r, strings.TrimPrefix(p, notebookAppPrefix))
	case strings.HasPrefix(p, "/_notebook/a/"):
		in.serveNotebookAsset(w, r, public)
	case p == "/_notebook/ws":
		if !notebookMethod(w, r, http.MethodGet) {
			return
		}
		if !notebookSameOrigin(r) {
			notebookForbidden(w)
			return
		}
		in.serveLive(w, r, public)
	case p == "/_notebook/api/assets" || p == "/_notebook/api/paste" || p == "/_notebook/api/conversion-preview":
		if !notebookWrite(w, r) {
			return
		}
		switch p {
		case "/_notebook/api/assets":
			in.serveNotebookUpload(w, r)
		case "/_notebook/api/paste":
			in.serveNotebookPaste(w, r)
		case "/_notebook/api/conversion-preview":
			in.serveNotebookPreview(w, r)
		}
	case p == "/_notebook/api/revisions" || strings.HasPrefix(p, "/_notebook/api/revisions/"):
		if !notebookPrivate(r) {
			notebookForbidden(w)
			return
		}
		if notebookReadMethod(w, r) {
			in.serveHistory(w, r)
		}
	case p == "/_notebook/api/recover" || p == "/_notebook/api/pin" || p == "/_notebook/api/rollback":
		if notebookWrite(w, r) {
			in.serveHistory(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func notebookPrivate(r *http.Request) bool {
	values := r.Header.Values("X-Flats-Access")
	return len(values) == 1 && values[0] == "private"
}

func notebookSameOrigin(r *http.Request) bool {
	if len(r.Header.Values("Origin")) > 1 || len(r.Header.Values("Sec-Fetch-Site")) > 1 {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin == scheme+"://"+r.Host
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

func notebookMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func notebookReadMethod(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func notebookWrite(w http.ResponseWriter, r *http.Request) bool {
	if !notebookMethod(w, r, http.MethodPost) {
		return false
	}
	header := r.Header.Values("X-Flats-Notebook")
	if !notebookPrivate(r) || len(header) != 1 || header[0] != "1" || !notebookSameOrigin(r) {
		notebookForbidden(w)
		return false
	}
	return true
}

func notebookForbidden(w http.ResponseWriter) {
	writeNotebookJSON(w, http.StatusForbidden, map[string]any{"code": "forbidden", "retry": false})
}

func writeNotebookJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeNotebookError(w http.ResponseWriter, err error) {
	var problem *WriteError
	if !errors.As(err, &problem) {
		var large *http.MaxBytesError
		switch {
		case errors.As(err, &large):
			problem = &WriteError{Code: "notebook_capacity", Reason: "body_limit"}
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			problem = &WriteError{Code: "unavailable", Reason: "cancelled", Retry: true}
		default:
			var failure *editFailure
			if errors.As(err, &failure) {
				problem = permanentWriteError(err).(*WriteError)
			} else {
				problem = &WriteError{Code: "unavailable", Reason: "storage", Retry: true}
			}
		}
	}
	status := http.StatusBadRequest
	switch problem.Code {
	case "unavailable":
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "1")
	case "conflict":
		status = http.StatusConflict
	case "notebook_capacity":
		status = http.StatusRequestEntityTooLarge
	case "not_found":
		status = http.StatusNotFound
	case "forbidden":
		status = http.StatusForbidden
	}
	writeNotebookJSON(w, status, problem)
}

func readNotebookJSON(w http.ResponseWriter, r *http.Request, value any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return &WriteError{Code: "invalid", Reason: "json"}
	}
	if err := strictDecode(data, value); err != nil {
		return &WriteError{Code: "invalid", Reason: "json", Detail: err.Error()}
	}
	return nil
}

func (in *Instance) serveNotebookApp(w http.ResponseWriter, r *http.Request, name string) {
	if !notebookReadMethod(w, r) {
		return
	}
	app, err := notebookApp()
	if err != nil {
		writeNotebookError(w, unavailable("app_missing"))
		return
	}
	app.serve(w, r, name)
}

func (app *notebookBundle) serve(w http.ResponseWriter, r *http.Request, name string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if name == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", notebookCSP)
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(app.shell))
		return
	}
	version, name, ok := strings.Cut(name, "/")
	if !ok || version != app.version {
		http.NotFound(w, r)
		return
	}
	data, ok := app.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	if path.Ext(name) == ".js" {
		ctype = "text/javascript; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+app.version+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func (in *Instance) serveNotebookPaste(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HTML     *string `json:"html,omitempty"`
		Markdown *string `json:"markdown,omitempty"`
	}
	if err := readNotebookJSON(w, r, &body); err != nil {
		writeNotebookError(w, err)
		return
	}
	if (body.HTML == nil) == (body.Markdown == nil) {
		writeNotebookError(w, &WriteError{Code: "invalid", Reason: "paste", Detail: "provide exactly one of html or markdown"})
		return
	}
	var markdown string
	warnings := []Warning{}
	if body.HTML != nil {
		root, err := html.Parse(strings.NewReader(*body.HTML))
		if err != nil {
			writeNotebookError(w, &WriteError{Code: "invalid", Reason: "paste"})
			return
		}
		paste := htmlPaste{warnings: []Warning{}}
		markdown = strings.TrimSpace(paste.render(root, 0))
		warnings = append(warnings, paste.warnings...)
	} else {
		markdown = *body.Markdown
	}
	ops, losses, err := ParseMarkdown(markdown, Placement{}, nil)
	if err != nil {
		writeNotebookError(w, &WriteError{Code: "invalid", Reason: "paste", Detail: err.Error()})
		return
	}
	type creation struct {
		Ref  string `json:"ref"`
		Kind Kind   `json:"kind"`
	}
	refs := []creation{}
	for _, op := range ops {
		items, err := operationCreates(op)
		if err != nil {
			writeNotebookError(w, err)
			return
		}
		for _, item := range items {
			refs = append(refs, creation{item.Name, item.Kind})
		}
	}
	writeNotebookJSON(w, http.StatusOK, map[string]any{"markdown": markdown, "refs": refs, "warnings": append(warnings, losses...)})
}

func (in *Instance) serveNotebookPreview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Property   Target      `json:"property"`
		Definition PropertyDef `json:"definition"`
	}
	if err := readNotebookJSON(w, r, &body); err != nil {
		writeNotebookError(w, err)
		return
	}
	if body.Property.Ref == nil || body.Property.ValidateKind(PropertyKind) != nil || body.Definition.Validate() != nil {
		writeNotebookError(w, &WriteError{Code: "invalid", Reason: "conversion_preview"})
		return
	}
	var preview propertyConversionPreview
	err := in.ReadTx(r.Context(), func(tx *sql.Tx, _ Sequence) error {
		e := &edit{ctx: r.Context(), tx: tx, refs: map[string]Ref{}}
		record, err := e.resolve(body.Property, PropertyKind)
		if err != nil {
			return err
		}
		property := record.Item.Data.(Property)
		if (property.Definition.Type == TitleType || body.Definition.Type == TitleType) && property.Definition.Type != body.Definition.Type {
			return invalidKind()
		}
		preview, err = e.previewProperty(&SetPropertyOp{Property: body.Property, Definition: body.Definition})
		return err
	})
	if err != nil {
		writeNotebookError(w, err)
		return
	}
	writeNotebookJSON(w, http.StatusOK, map[string]any{"preview": preview.Token, "emptied": preview.EmptiedValues, "options_created": preview.OptionsCreated})
}
