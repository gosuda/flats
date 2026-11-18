package sheet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/sheet/formula"
)

// serveHistory serves the History routes of section 17: the revision list,
// a revision's tabs in the snapshot frame form, the typed content of one of
// its cells, tab recovery, pin and rollback. The caller has checked access,
// origin and method.
func (in *Instance) serveHistory(w http.ResponseWriter, r *http.Request) {
	b, ok := in.b.(HistoryBackend)
	if !ok {
		writeError(w, unavailable("history_backend"))
		return
	}
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/_sheet/api/")
	if r.Method == http.MethodPost {
		in.serveHistoryPost(w, r, b, path)
		return
	}
	if path == "revisions" {
		query := r.URL.Query()
		before, limit := 0, 50
		for key, values := range query {
			n, err := strconv.Atoi(values[0])
			if len(values) != 1 || err != nil || n < 0 || (key != "before" && key != "limit") {
				writeError(w, invalidf("query", "the list takes before and limit"))
				return
			}
			if key == "before" {
				before = n
			} else {
				limit = min(max(n, 1), 200)
			}
		}
		page, err := b.Revisions(ctx, before, limit)
		if err != nil {
			writeError(w, err)
			return
		}
		if page.Revisions == nil {
			page.Revisions = []RevisionInfo{}
		}
		writeJSON(w, http.StatusOK, page)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "revisions/"), "/")
	n, err := strconv.Atoi(parts[0])
	wholeTab := len(parts) == 3 && parts[1] == "tabs"
	oneCell := len(parts) == 4 && parts[1] == "tabs" && parts[3] == "cell"
	if err != nil || n < 1 || (len(parts) != 1 && !wholeTab && !oneCell) {
		http.NotFound(w, r)
		return
	}
	var row, col uint64
	if oneCell {
		query := r.URL.Query()
		var errRow, errCol error
		row, errRow = strconv.ParseUint(query.Get("row"), 10, 64)
		col, errCol = strconv.ParseUint(query.Get("col"), 10, 64)
		if len(query) != 2 || len(query["row"]) != 1 || len(query["col"]) != 1 || errRow != nil || errCol != nil {
			writeError(w, invalidf("query", "a cell read takes one row id and one column id"))
			return
		}
	}
	files, date, release, err := b.RevisionFiles(ctx, n)
	if err != nil {
		writeError(w, err)
		return
	}
	defer release()
	wb, err := in.revision(ctx, n, files, date)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(parts) == 1 {
		writeJSON(w, http.StatusOK, map[string]any{"date": date, "tabs": json.RawMessage(appendTabInfo(nil, wb))})
		return
	}
	id, err := strconv.ParseUint(parts[2], 10, 64)
	t := wb.byID[TabID(id)]
	if err != nil || t == nil {
		writeError(w, &WriteError{Code: "not_found", Reason: "tab"})
		return
	}
	if oneCell {
		writeJSON(w, http.StatusOK, revisionCellText(t, RowID(row), ColID(col)))
		return
	}
	// A revision workbook is read-only once it is calculated, so its
	// capture needs no lock.
	c, head, err := wb.captureTab(t)
	if err != nil {
		writeError(w, err)
		return
	}
	defer c.release()
	// The frames are those of a live snapshot, each inside the server frame
	// limit, one to a line, so the browser parses one frame at a time.
	enc, err := newSnapEncoder(c, head)
	if err != nil {
		writeError(w, err)
		return
	}
	// Each frame is written as it is encoded, so the response is never
	// held whole. Core cancels the request when the route is withdrawn.
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	out := &cancelWriter{ctx: ctx, w: w}
	enc.run(func(kind string, rest []byte) bool {
		for _, piece := range []string{`{"type":"`, kind, `","generation":0,`} {
			if _, err := io.WriteString(out, piece); err != nil {
				return false
			}
		}
		if _, err := out.Write(rest); err != nil {
			return false
		}
		_, err := io.WriteString(out, "\n")
		return err == nil
	})
}

// revisionCellText is the content of one cell of a revision as it would be
// typed, in the form of a peek answer (section 12.4): the A1 text of a
// formula at the position the cell has in that revision. Snapshot frames
// carry no formula text, so the History view asks for it cell by cell. A
// cell the revision does not hold reads as empty.
func revisionCellText(t *Tab, row RowID, col ColID) map[string]any {
	edit, isFormula := "", false
	if cell := t.cell(row, col); cell != nil {
		if pos, ok := t.posOf(row, col); ok {
			if cell.f != nil {
				edit, isFormula = cell.f.Text(pos), true
			} else if !cell.blank() {
				edit = formula.EditText(cell.content, pos)
			}
		}
	}
	return map[string]any{"edit": edit, "formula": isFormula}
}

func (in *Instance) serveHistoryPost(w http.ResponseWriter, r *http.Request, b HistoryBackend, path string) {
	ctx := r.Context()
	// The body is read against the shape of its route: pin takes revision
	// and pinned, rollback revision, recover revision and tab. A rollback
	// and a recovery may also name the session.
	var body historyBody
	if err := readHistoryBody(w, r, path, &body); err != nil {
		writeError(w, err)
		return
	}
	switch path {
	case "pin":
		if err := b.Pin(ctx, body.Revision, body.Pinned); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"revision": body.Revision, "pinned": body.Pinned})
	case "rollback":
		label, s, err := in.requestLabel(ctx, r, body.Session)
		if err != nil {
			writeError(w, err)
			return
		}
		if s != nil {
			in.release(s)
		}
		n, err := b.Rollback(ctx, body.Revision, label)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"revision": n})
	case "recover":
		label, s, err := in.requestLabel(ctx, r, body.Session)
		if err != nil {
			writeError(w, err)
			return
		}
		if s != nil {
			// The session's time without activity starts when the request
			// ends, also when nothing else connects it.
			defer in.release(s)
		}
		files, _, release, err := b.RevisionFiles(ctx, body.Revision)
		if err != nil {
			writeError(w, err)
			return
		}
		defer release()
		id, err := in.recoverTab(ctx, s, label, files, TabID(body.Tab))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tab": uint64(id)})
	}
}

// requestLabel names the person behind a History request: the session the
// body names, else the trusted identity of the route, else "Browser". The
// writer finds the session and holds it for the caller, who releases it. When it cannot now, the request is refused
// with that reason and nothing is done under another name; while the sheet
// calculates without an edit that is the refusal calculating, at once.
func (in *Instance) requestLabel(ctx context.Context, r *http.Request, token string) (string, *session, error) {
	if token != "" {
		s, err := in.findSession(ctx, token, true)
		if err == nil {
			label := s.label
			if label == "" {
				label = "Browser"
			}
			return label, s, nil
		}
		if asWriteError(err).Code == "unavailable" {
			return "", nil, err
		}
	}
	if login := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); login != "" {
		if name := r.Header.Get("Tailscale-User-Name"); name != "" {
			return name, nil, nil
		}
		return login, nil, nil
	}
	return "Browser", nil, nil
}
