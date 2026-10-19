package api

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/core"
)

// getBoard reads a board: GET /flats/{slug}/board?revision=&frame=&rect=x,y,w,h&ids=a,b&format=&cursor=&limit=.
func (s *Server) getBoard(w http.ResponseWriter, r *http.Request, _ core.Via) {
	q := r.URL.Query()
	var rect []float64
	if v := q.Get("rect"); v != "" {
		for _, part := range strings.Split(v, ",") {
			f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
			if err != nil {
				writeErr(w, http.StatusBadRequest, errors.New("rect is x,y,w,h"))
				return
			}
			rect = append(rect, f)
		}
	}
	var ids []string
	if v := q.Get("ids"); v != "" {
		ids = strings.Split(v, ",")
	}
	rev, limit := 0, 0
	for _, p := range []struct {
		name string
		dst  *int
	}{{"revision", &rev}, {"limit", &limit}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeErr(w, http.StatusBadRequest, errors.New(p.name+" must be a nonnegative integer"))
				return
			}
			*p.dst = n
		}
	}
	query, err := core.BoardQuery(q.Get("frame"), rect, ids, q.Get("format"), q.Get("cursor"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	page, err := s.Svc.GetBoard(r.Context(), r.PathValue("slug"), rev, query)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// boardBodyMax bounds an update_board body: its images (base64, a third
// larger than the upload limit) plus 16 MiB of operations.
func (s *Server) boardBodyMax() int64 {
	return max(s.Svc.UploadLimit(), 0)*4/3 + 16<<20
}

// updateBoard applies an update_board body: POST /flats/{slug}/board.
func (s *Server) updateBoard(w http.ResponseWriter, r *http.Request, via core.Via) {
	var in core.BoardUpdate
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.boardBodyMax()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, errors.New("the body is larger than the update_board limit"))
			return
		}
		writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body: "+err.Error()))
		return
	}
	out, err := s.Svc.UpdateBoard(r.Context(), r.PathValue("slug"), in, via)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// importBoard imports a board data file sent as the raw body:
// POST /flats/{slug}/board/import?mode=add|replace&op_id=. The body, up to
// upload_max_bytes × 4, is streamed to a temporary file.
func (s *Server) importBoard(w http.ResponseWriter, r *http.Request, via core.Via) {
	q := r.URL.Query()
	mode := q.Get("mode")
	if mode == "" {
		mode = "add"
	}
	opID := q.Get("op_id")
	if opID == "" {
		opID = "import-" + rand.Text()
	}
	f, err := os.CreateTemp("", "flats-board-import-*.zip")
	if err != nil {
		fail(w, err)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	size, err := io.Copy(f, http.MaxBytesReader(w, r.Body, max(s.Svc.UploadLimit(), 1)*4))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, errors.New("the file is larger than the import limit (upload_max_bytes × 4)"))
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.Svc.ImportBoard(r.Context(), r.PathValue("slug"), core.BoardImport{OpID: opID, Mode: mode, Archive: f, Size: size}, via)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
