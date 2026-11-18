package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/sheet"
)

type sheetService interface {
	GetSheet(context.Context, string, int, []byte) (json.RawMessage, error)
	UpdateSheet(context.Context, string, []byte, core.Via) (core.SheetWriteResult, error)
}

// SheetMaxReadCells is the greatest page size of a sheet read, in cells.
const SheetMaxReadCells = 10000

// SheetQuery admits management read selectors and returns the core query
// without its transport-only revision. The CLI and the MCP tools use the
// same admission rules.
func SheetQuery(q url.Values) (int, []byte, error) {
	allowed := map[string]bool{"tab": true, "range": true, "format": true, "include": true, "limit": true, "cursor": true, "revision": true}
	for k, vs := range q {
		if !allowed[k] || len(vs) != 1 || vs[0] == "" {
			return 0, nil, fmt.Errorf("invalid or repeated sheet query field %q", k)
		}
	}
	rev := 0
	if q.Has("revision") {
		n, err := strconv.Atoi(q.Get("revision"))
		if err != nil || n <= 0 {
			return 0, nil, errors.New("revision must be a positive integer")
		}
		rev = n
	}
	var query sheet.ReadQuery
	if q.Has("cursor") {
		if len(q) != 1 && !(len(q) == 2 && q.Has("revision")) {
			return 0, nil, errors.New("cursor accepts no other selector")
		}
		query.Cursor = q.Get("cursor")
		raw, err := json.Marshal(query)
		return rev, raw, err
	}
	query.Tab, query.Range = q.Get("tab"), q.Get("range")
	if query.Range != "" && query.Tab == "" {
		return 0, nil, errors.New("range requires tab")
	}
	if q.Has("format") {
		query.Format = q.Get("format")
		if query.Format != "json" && query.Format != "csv" {
			return 0, nil, errors.New("format must be json or csv")
		}
		if query.Format == "csv" && query.Tab == "" {
			return 0, nil, errors.New("format csv requires tab")
		}
	}
	if q.Has("include") {
		for _, part := range strings.Split(q.Get("include"), ",") {
			if part != "shown" && part != "formats" && part != "widths" {
				return 0, nil, errors.New("include lists shown, formats or widths")
			}
			query.Include = append(query.Include, part)
		}
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > SheetMaxReadCells {
			return 0, nil, fmt.Errorf("limit must be between 1 and %d", SheetMaxReadCells)
		}
		query.Limit = n
	}
	raw, err := json.Marshal(query)
	return rev, raw, err
}

func (s *Server) getSheet(w http.ResponseWriter, r *http.Request, _ core.Via) {
	getSheet(w, r, s.Svc)
}

func getSheet(w http.ResponseWriter, r *http.Request, svc sheetService) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		failSheet(w, &sheet.WriteError{Code: "invalid", Detail: err.Error()})
		return
	}
	rev, query, err := SheetQuery(q)
	if err == nil && r.Body != nil {
		var raw []byte
		raw, err = io.ReadAll(io.LimitReader(r.Body, 1))
		if err == nil && len(raw) != 0 {
			err = errors.New("GET sheet does not accept a body")
		}
	}
	if err != nil {
		failSheet(w, &sheet.WriteError{Code: "invalid", Detail: err.Error()})
		return
	}
	out, err := svc.GetSheet(r.Context(), r.PathValue("slug"), rev, query)
	if err != nil {
		failSheet(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) updateSheet(w http.ResponseWriter, r *http.Request, via core.Via) {
	updateSheet(w, r, via, s.Svc)
}

func updateSheet(w http.ResponseWriter, r *http.Request, via core.Via, svc sheetService) {
	if r.URL.RawQuery != "" {
		failSheet(w, &sheet.WriteError{Code: "invalid", Detail: "POST sheet does not accept query fields"})
		return
	}
	limit := sheet.MaxUpdateBodyBytes()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sheet.MaxUpdateReadBytes()))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			// The body is over the bytes of one call: the refusal the sheet
			// gives for its call_bytes limit. The size is known only when
			// the request declares it.
			refusal := &sheet.WriteError{Code: "sheet_capacity", Limit: "call_bytes", Max: limit}
			if r.ContentLength > limit {
				refusal.Value = r.ContentLength
			}
			failSheet(w, refusal)
			return
		}
		failSheet(w, &sheet.WriteError{Code: "invalid", Detail: err.Error()})
		return
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		failSheet(w, &sheet.WriteError{Code: "invalid", Detail: "body must be an update_sheet object without slug"})
		return
	}
	out, err := svc.UpdateSheet(r.Context(), r.PathValue("slug"), raw, via)
	if err != nil {
		failSheet(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// sheetCategory is the refusal category of a sheet error, or "" for an
// error that is not a sheet refusal.
func sheetCategory(err error) string {
	var refusal *sheet.WriteError
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	if errors.Is(err, core.ErrNotSheet) {
		return "not_sheet"
	}
	return ""
}

func sheetStatus(err error) int {
	switch sheetCategory(err) {
	case "":
		return 0
	case "invalid", "not_sheet":
		return http.StatusBadRequest
	case "conflict":
		return http.StatusConflict
	case "sheet_capacity":
		return http.StatusUnprocessableEntity
	case "unavailable":
		return http.StatusServiceUnavailable
	case "not_found":
		return http.StatusNotFound
	case "forbidden":
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func failSheet(w http.ResponseWriter, err error) {
	var refusal *sheet.WriteError
	if !errors.As(err, &refusal) && errors.Is(err, core.ErrUnavailable) {
		err = &sheet.WriteError{Code: "unavailable", Reason: "sheet_unavailable", Retry: true, Detail: err.Error()}
	}
	if errors.As(err, &refusal) {
		writeJSON(w, statusOf(err), struct {
			ErrorBody
			Reason string `json:"reason,omitempty"`
			Limit  string `json:"limit,omitempty"`
			Max    int64  `json:"max,omitempty"`
			Value  int64  `json:"value,omitempty"`
			Name   string `json:"name,omitempty"`
			Retry  bool   `json:"retry"`
			Detail string `json:"detail,omitempty"`
		}{errorBody(err), refusal.Reason, refusal.Limit, refusal.Max, refusal.Value, refusal.Name, refusal.Retry, refusal.Detail})
		return
	}
	fail(w, err)
}
