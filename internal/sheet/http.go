package sheet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/sheet/formula"
)

const (
	appPrefix = "/_sheet/app/"
	// The shell loads scripts, styles and connections from itself only, and
	// no images, fonts or frames (technical design, section 17).
	shellCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'none'; font-src 'none'; frame-src 'none'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"
)

var sheetApp = sync.OnceValues(func() (*appBundle, error) { return loadSheetApp(webFS) })

type appBundle struct {
	version string
	shell   []byte
	files   map[string][]byte
}

// functionsJSON lists the name, group and argument names of every function
// of the formula registry, sorted by name.
func functionsJSON() []byte {
	type entry struct {
		Name  string   `json:"name"`
		Group string   `json:"group"`
		Args  []string `json:"args"`
	}
	list := []entry{}
	for _, f := range formula.Functions() {
		args := f.Args
		if args == nil {
			args = []string{}
		}
		list = append(list, entry{f.Name, f.Group, args})
	}
	data, _ := json.Marshal(list)
	return data
}

// loadSheetApp reads the embedded client, adds functions.json from the
// registry, hashes all files into a version and rewrites /_sheet/app/ to
// /_sheet/app/<version>/, so that every file is immutable under its address.
func loadSheetApp(source fs.FS) (*appBundle, error) {
	b := &appBundle{files: map[string][]byte{}}
	hash := sha256.New()
	add := func(name string, data []byte) {
		fmt.Fprintf(hash, "%d:%s:%d:", len(name), name, len(data))
		hash.Write(data)
		if name == "index.html" {
			b.shell = data
		} else {
			b.files[name] = data
		}
	}
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
		add(strings.TrimPrefix(name, "web/"), data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(b.shell) == 0 {
		return nil, fmt.Errorf("sheet shell is missing")
	}
	add("functions.json", functionsJSON())
	b.version = hex.EncodeToString(hash.Sum(nil)[:16])
	prefix := appPrefix + b.version + "/"
	b.shell = bytes.ReplaceAll(b.shell, []byte(appPrefix), []byte(prefix))
	for name, data := range b.files {
		b.files[name] = bytes.ReplaceAll(data, []byte(appPrefix), []byte(prefix))
	}
	return b, nil
}

// ServeHTTP serves the sheet on a host-authorized flat origin. Only core's
// exact X-Flats-Access: private grants more than reading (spec rule 55).
func (in *Instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// A request counts as use from here to its end, a connection for as
	// long as it is open.
	if err := in.enter(); err != nil {
		writeError(w, err)
		return
	}
	defer in.leave()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(in.ctx, cancel)
	defer stop()
	r = r.WithContext(ctx)
	private := accessPrivate(r)
	p := r.URL.Path
	switch {
	case p == "/":
		in.serveApp(w, r, "")
	case strings.HasPrefix(p, appPrefix):
		in.serveApp(w, r, strings.TrimPrefix(p, appPrefix))
	case p == "/_sheet/ws":
		if !allowMethod(w, r, http.MethodGet) {
			return
		}
		if !sameOrigin(r) {
			forbidden(w)
			return
		}
		in.serveLive(w, r, !private)
	case p == "/_sheet/csv":
		if readMethod(w, r) {
			in.serveCSV(w, r)
		}
	case p == "/_sheet/api/revisions" || strings.HasPrefix(p, "/_sheet/api/revisions/"):
		if !private {
			forbidden(w)
			return
		}
		if readMethod(w, r) {
			in.serveHistory(w, r)
		}
	case p == "/_sheet/api/recover" || p == "/_sheet/api/pin" || p == "/_sheet/api/rollback":
		if !allowMethod(w, r, http.MethodPost) {
			return
		}
		header := r.Header.Values("X-Flats-Sheet")
		if !private || len(header) != 1 || header[0] != "1" || !sameOrigin(r) {
			forbidden(w)
			return
		}
		in.serveHistory(w, r)
	default:
		http.NotFound(w, r)
	}
}

func sameOrigin(r *http.Request) bool {
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

func allowMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func readMethod(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func forbidden(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, &WriteError{Code: "forbidden"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeError answers with a refusal as {code, reason, limit, ...} under the
// status of its category (section 18.3).
func writeError(w http.ResponseWriter, err error) {
	var werr *WriteError
	if !errors.As(err, &werr) {
		var large *http.MaxBytesError
		switch {
		case errors.As(err, &large):
			werr = capacity("call_bytes", large.Limit, large.Limit+1)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			werr = unavailable("cancelled")
		default:
			werr = unavailable("storage")
		}
	}
	status := http.StatusBadRequest
	switch werr.Code {
	case "unavailable":
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "1")
	case "conflict":
		status = http.StatusConflict
	case "sheet_capacity":
		status = http.StatusUnprocessableEntity
	case "not_found":
		status = http.StatusNotFound
	case "forbidden":
		status = http.StatusForbidden
	}
	writeJSON(w, status, werr)
}

// readHistoryBody reads the body of a History route that changes the
// sheet, by the shape of the route (protocol.go), as a frame is read.
func readHistoryBody(w http.ResponseWriter, r *http.Request, route string, body *historyBody) error {
	s := historyShapes[route]
	if s == nil {
		return &WriteError{Code: "not_found", Reason: "route"}
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return err
	}
	if _, fault := walk(data, s, body); fault != nil {
		return invalidf(route, "%v", fault)
	}
	if body.Revision < 1 {
		return invalidf("revision", "revision is a revision number")
	}
	if body.bad {
		return invalidf(route, "tab is an id: a positive integer")
	}
	return nil
}

func (in *Instance) serveApp(w http.ResponseWriter, r *http.Request, name string) {
	if !readMethod(w, r) {
		return
	}
	app, err := sheetApp()
	if err != nil {
		writeError(w, unavailable("app_missing"))
		return
	}
	app.serve(w, r, name)
}

func (app *appBundle) serve(w http.ResponseWriter, r *http.Request, name string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if name == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", shellCSP)
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(app.shell))
		return
	}
	version, name, ok := strings.Cut(name, "/")
	data, found := app.files[name]
	if !ok || version != app.version || !found {
		http.NotFound(w, r)
		return
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	switch path.Ext(name) {
	case ".js":
		ctype = "text/javascript; charset=utf-8"
	case ".json":
		ctype = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+app.version+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// serveCSV downloads one tab as a CSV file of its values (spec rule 60),
// from a capture of its used range. The file starts with the byte order
// mark.
func (in *Instance) serveCSV(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.URL.Query().Get("tab"), 10, 64)
	if err != nil {
		writeError(w, invalidf("tab", "tab is a tab id"))
		return
	}
	if err := in.await(r.Context(), agentPatience); err != nil {
		writeError(w, err)
		return
	}
	// With no connection open nothing else moves the sheet to the host's
	// date, so the download does, as an agent read does. While a
	// connection is open the maintenance tick does, and the download has
	// what the open screens have (section 10.6).
	if in.hub.count() == 0 {
		if err := in.syncDate(r.Context()); err != nil {
			writeError(w, err)
			return
		}
	}
	in.mu.RLock()
	wb := in.wb
	t := wb.byID[TabID(id)]
	if t == nil {
		in.mu.RUnlock()
		writeError(w, &WriteError{Code: "not_found", Reason: "tab"})
		return
	}
	rows, cols := t.used()
	c, err := wb.captureRange(t, formula.Range{Last: formula.Pos{Row: rows - 1, Col: cols - 1}}, true)
	in.mu.RUnlock()
	if err != nil {
		writeError(w, err)
		return
	}
	defer c.release()
	if !c.reserveCSV() {
		writeError(w, unavailable("capture_budget"))
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": c.name + ".csv"}))
	if r.Method == http.MethodHead {
		return
	}
	// Core cancels the request when the route is withdrawn; the response
	// then ends at the next row.
	out := &cancelWriter{ctx: r.Context(), w: w}
	if _, err := io.WriteString(out, csvBOM); err != nil {
		return
	}
	_ = c.writeCSV(out)
}

// cancelWriter ends a streamed response at its first write after the
// request is cancelled, and gives each write responseWriteWait, so that a
// client that stops reading cannot hold the response, its capture and the
// sheet's use for longer.
type cancelWriter struct {
	ctx context.Context
	w   http.ResponseWriter
	rc  *http.ResponseController
}

func (cw *cancelWriter) Write(p []byte) (int, error) {
	if err := cw.ctx.Err(); err != nil {
		return 0, err
	}
	if cw.rc == nil {
		cw.rc = http.NewResponseController(cw.w)
	}
	// A writer that takes no deadline writes without one.
	_ = cw.rc.SetWriteDeadline(time.Now().Add(responseWriteWait))
	return cw.w.Write(p)
}
