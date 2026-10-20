package board

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/board/engine"
	"github.com/gosuda/flats/internal/board/model"
)

const shellCSP = "default-src 'self'; img-src 'self' blob: data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status, code, msg := http.StatusInternalServerError, "error", "internal error"
	var be *Error
	switch {
	case errors.As(err, &be):
		code, msg = be.Category, be.Msg
		switch be.Category {
		case CategoryInvalid:
			status = http.StatusBadRequest
		case CategoryConflict:
			status = http.StatusConflict
		case CategoryCapacity:
			status = http.StatusRequestEntityTooLarge
		case CategoryNotFound:
			status = http.StatusNotFound
		case CategoryUnavailable:
			status = http.StatusServiceUnavailable
		}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, code, msg = http.StatusServiceUnavailable, CategoryUnavailable, "the request was cancelled"
	}
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}

func forbidden(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusForbidden, map[string]string{"code": "forbidden", "message": msg})
}

// checkWrite enforces POST, private access, the board header and the
// same origin (7.1).
func checkWrite(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case !private(r):
		forbidden(w, "this board is read-only here")
	case r.Header.Get("X-Flats-Board") != "1" || !sameOrigin(r):
		forbidden(w, "request refused: not from this board's canvas")
	default:
		return true
	}
	return false
}

func readOnlyMethod(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func (in *Instance) serveShell(w http.ResponseWriter, r *http.Request) {
	if !readOnlyMethod(w, r) {
		return
	}
	a, err := app()
	if err != nil {
		http.Error(w, "the board app is missing", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", a.shellCSP)
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", a.shellETag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.shell))
}

// serveApp serves an app file. A request for the file's current version
// (?v=, the URLs the shell names) may be kept forever. A request for
// another version comes from a shell of another binary; it gets a 404, so
// that page reloads (boot.js) instead of mixing files of two versions. The
// exception is boot.js itself, which imports nothing and must run for that
// reload: any version of it gets the current file, never stored. A request
// without a version is revalidated every time, because another binary can
// serve other content under the same name.
func (in *Instance) serveApp(w http.ResponseWriter, r *http.Request, name string) {
	if !readOnlyMethod(w, r) {
		return
	}
	a, err := app()
	if err != nil {
		http.Error(w, "the board app is missing", http.StatusInternalServerError)
		return
	}
	f, ok := a.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	// A query that does not parse cleanly counts as another version:
	// URL.Query would drop the pairs it cannot read.
	q, qerr := url.ParseQuery(r.URL.RawQuery)
	switch v, versioned := q["v"]; {
	case qerr == nil && !versioned:
		h.Set("Cache-Control", "no-cache")
	case qerr == nil && len(v) == 1 && v[0] == f.version:
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	case name == "boot.js":
		h.Set("Cache-Control", "no-store")
	default:
		h.Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	h.Set("ETag", f.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.data))
}

// serveAsset serves an image under the 7.2 access rules.
func (in *Instance) serveAsset(w http.ResponseWriter, r *http.Request, hash string) {
	if !readOnlyMethod(w, r) {
		return
	}
	if !model.ValidAsset(hash) {
		http.NotFound(w, r)
		return
	}
	a, ok := in.assets.get(hash)
	mimeType := a.Mime
	if private(r) {
		if !ok {
			p, inRev, err := in.b.RevisionAsset(r.Context(), hash)
			if err != nil {
				writeErr(w, err)
				return
			}
			ok, mimeType = inRev, extMime(strings.TrimPrefix(path.Ext(p), "."))
		}
	} else {
		ok = ok && a.Refs > 0
	}
	if !ok || mimeType == "" {
		http.NotFound(w, r)
		return
	}
	data, err := in.b.ReadBlob(r.Context(), hash)
	if err != nil {
		writeErr(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", mimeType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// serveUpload accepts one image (10) and protects it for an hour (5.5).
func (in *Instance) serveUpload(w http.ResponseWriter, r *http.Request) {
	if !checkWrite(w, r) {
		return
	}
	info, err := in.upload(r.Context(), http.MaxBytesReader(w, r.Body, in.b.Limits().UploadMax))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "too_large", "message": "the image is larger than the upload limit"})
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hash": info.Hash, "w": info.W, "h": info.H})
}

func (in *Instance) upload(ctx context.Context, body io.Reader) (AssetInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return AssetInfo{}, err
	}
	info, err := CheckImage(data, in.b.Limits().UploadMax)
	if err != nil {
		return AssetInfo{}, err
	}
	// The admission reserves the bytes before the write, so concurrent
	// uploads cannot both spend the same headroom (5.4).
	blobs := []newBlob{{info, data}}
	_, release, err := in.admitBlobs(ctx, blobs, nil)
	defer release()
	if err != nil {
		return AssetInfo{}, err
	}
	if err := in.putBlobs(ctx, blobs, false); err != nil {
		return AssetInfo{}, err
	}
	release()
	err = in.do(ctx, func() error {
		t, err := in.begin()
		if err != nil {
			return err
		}
		if err := in.holdAssets(t, []AssetInfo{info}, in.now().Unix()); err != nil {
			t.rollback()
			return err
		}
		return t.commit()
	})
	return info, err
}

// AddAsset checks and stores an image an agent adds (8.1 update_board
// assets) and protects it for an hour, so a later add can reference it.
func (in *Instance) AddAsset(ctx context.Context, data []byte) (AssetInfo, error) {
	return in.upload(ctx, bytes.NewReader(data))
}

func (in *Instance) serveRevisions(w http.ResponseWriter, r *http.Request, rest string) {
	if !private(r) {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	if rest == "" {
		if !readOnlyMethod(w, r) {
			return
		}
		before, _ := strconv.Atoi(r.URL.Query().Get("before"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 100 {
			limit = 50
		}
		page, err := in.b.Revisions(ctx, before, limit)
		if err != nil {
			writeErr(w, err)
			return
		}
		if page.Revisions == nil {
			page.Revisions = []RevisionInfo{}
		}
		writeJSON(w, http.StatusOK, page)
		return
	}
	num, action, _ := strings.Cut(rest, "/")
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "", "elements":
		if !readOnlyMethod(w, r) {
			return
		}
		info, c, err := in.b.RevisionContent(ctx, n)
		if err != nil {
			writeErr(w, err)
			return
		}
		if action == "" {
			writeJSON(w, http.StatusOK, map[string]any{"revision": info})
			return
		}
		els := make([]json.RawMessage, len(c.Elements))
		for i, e := range c.Elements {
			els[i] = e.CanonicalJSON()
		}
		writeJSON(w, http.StatusOK, map[string]any{"revision": info, "elements": els,
			"meta": map[string]any{"title": c.Title, "grid": c.Grid}})
	case "pin":
		if !checkWrite(w, r) {
			return
		}
		var body struct {
			Pinned bool `json:"pinned"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			writeErr(w, errorf(CategoryInvalid, "the body is {pinned}"))
			return
		}
		if err := in.b.Pin(ctx, n, body.Pinned); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "revision": n, "pinned": body.Pinned})
	case "rollback":
		if !checkWrite(w, r) {
			return
		}
		_, label := personLabel(r)
		rev, err := in.b.Rollback(ctx, n, label)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "restored", "revision": rev, "rollback_of": n})
	default:
		http.NotFound(w, r)
	}
}

// serveRecover copies elements of an earlier revision into the board with
// new ids (rule 47). The copy is an ordinary edit of the person.
func (in *Instance) serveRecover(w http.ResponseWriter, r *http.Request) {
	if !checkWrite(w, r) {
		return
	}
	var body struct {
		Revision int      `json:"revision"`
		IDs      []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.Revision <= 0 || len(body.IDs) == 0 {
		writeErr(w, errorf(CategoryInvalid, "the body is {revision, ids}"))
		return
	}
	key, label := personLabel(r)
	ids, err := in.recoverElements(r.Context(), body.Revision, body.IDs, key, label)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": ids})
}

func (in *Instance) recoverElements(ctx context.Context, rev int, ids []string, key, label string) ([]string, error) {
	// The revision's files stay leased from their selection under the flat
	// lock until the copy commits, when the assets' board.db rows (held
	// for an hour, then referenced) protect them (5.5): retention and a
	// sweep in between cannot remove an image the copy names.
	c, release, err := in.b.HeldRevisionContent(ctx, rev)
	if err != nil {
		return nil, err
	}
	defer release()
	if in.hook != nil {
		in.hook("recover-selected")
	}
	byID := map[string]model.Element{}
	for _, e := range c.Elements {
		byID[e.ID] = e
	}
	var picked []model.Element
	var assets []AssetInfo
	for _, id := range ids {
		e, ok := byID[id]
		if !ok {
			continue
		}
		picked = append(picked, e)
		if e.Type == model.TypeImage {
			if a, ok := c.Assets[e.Asset]; ok {
				assets = append(assets, AssetInfo{Hash: a.Hash, Mime: a.Mime, W: a.W, H: a.H, Bytes: a.Bytes})
			}
		}
	}
	if len(picked) == 0 {
		return nil, errorf(CategoryNotFound, "revision %d holds none of these elements", rev)
	}
	quota := in.b.Limits().DiskQuota
	if quota > 0 {
		if _, err := in.meas.wait(ctx); err != nil {
			return nil, err
		}
	}
	var newIDs []string
	err = in.do(ctx, func() error {
		st := in.st
		t, err := in.begin()
		if err != nil {
			return err
		}
		fail := func(err error) error { t.rollback(); return asError(err) }
		now := in.now().Unix()
		if err := in.holdAssets(t, assets, now); err != nil {
			return fail(err)
		}
		ops, ids := copyOps(picked, func(id string) (model.Element, bool) { e, ok := byID[id]; return e, ok }, 0, 0, topKey(st))
		out, err := st.ApplyAgent(engine.AgentCall{Ops: ops, Label: label, By: key, Unlimited: true, Now: now, NewID: newID})
		if err != nil {
			return fail(err)
		}
		newIDs = ids
		if err := t.persist(out.Changed, st.Seq()); err != nil {
			return fail(err)
		}
		if d := out.Delta; d != nil {
			d.By = &model.By{Kind: "person", Label: label}
			rec, err := t.addDelta(d, "", 0)
			if err != nil {
				return fail(err)
			}
			t.after = append(t.after, func() { in.broadcast(rec, d) })
			if err := t.touchGroup(key, label); err != nil {
				return fail(err)
			}
		}
		over, ok, err := t.overQuota(quota)
		switch {
		case err != nil:
			return fail(err)
		case !ok:
			return fail(errorf(CategoryUnavailable, "the board's disk usage cannot be measured right now; try again"))
		case over:
			return fail(errorf(CategoryCapacity, "the board is out of disk space"))
		}
		return t.commit()
	})
	return newIDs, err
}

func (in *Instance) serveExport(w http.ResponseWriter, r *http.Request) {
	if !readOnlyMethod(w, r) {
		return
	}
	// The selected assets stay leased until the archive is written (5.5).
	boardJSON, els, assets, release, err := in.exportData(r.Context(), !private(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	defer release()
	if in.hook != nil {
		in.hook("export-selected")
	}
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, in.slug))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	rc := http.NewResponseController(w)
	dw := &deadlineWriter{w: w, rc: rc, ctx: r.Context()}
	err = writeArchive(r.Context(), dw, boardJSON, els, assets,
		func(hash string) ([]byte, error) { return in.b.ReadBlob(r.Context(), hash) })
	if err == nil {
		return
	}
	if !dw.wrote {
		h.Del("Content-Disposition")
		writeErr(w, err)
		return
	}
	// The response already started: abort it, so the client sees a failed
	// transfer instead of a complete-looking, truncated file. A client
	// that left is an ordinary disconnect, not a failure worth a warning.
	if r.Context().Err() == nil {
		in.log.Warn("board export failed", "err", err)
	}
	panic(http.ErrAbortHandler)
}

// deadlineWriter bounds each write, so a slow client cannot hold a handler
// past Stop. wrote tells whether the response started.
type deadlineWriter struct {
	w     io.Writer
	rc    *http.ResponseController
	ctx   context.Context
	wrote bool
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if err := d.ctx.Err(); err != nil {
		return 0, err
	}
	_ = d.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	d.wrote = true
	return d.w.Write(p)
}

// serveImport imports a data or recovery file sent as the raw body,
// streamed to a temporary file of at most upload_max_bytes × 4 (8.2).
func (in *Instance) serveImport(w http.ResponseWriter, r *http.Request) {
	if !checkWrite(w, r) {
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "add"
	}
	lim := in.b.Limits()
	f, err := os.CreateTemp("", "flats-board-import-*.zip")
	if err != nil {
		writeErr(w, err)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	size, err := io.Copy(f, http.MaxBytesReader(w, r.Body, max(lim.UploadMax, 1)*4))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "too_large", "message": "the file is larger than the import limit"})
			return
		}
		writeErr(w, err)
		return
	}
	key, label := personLabel(r)
	req := ImportRequest{OpID: "import-" + randomToken(), Mode: mode, Archive: f, Size: size, Label: label, Person: true, Key: key}
	prep, err := in.prepareImport(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	var res ImportResult
	err = in.b.Exclusive(r.Context(), func(ctx context.Context, other int64) error {
		res, err = in.applyImport(ctx, prep, other)
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if res.IDs == nil {
		res.IDs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": res.IDs, "assets": res.Assets})
}
