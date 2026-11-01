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
	"regexp"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/notebook"
)

type notebookService interface {
	GetNotebook(context.Context, string, int, []byte) (json.RawMessage, error)
	UpdateNotebook(context.Context, string, []byte, core.Via) (core.NotebookWriteResult, error)
	UploadLimit() int64
}

var notebookOpID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// NotebookQuery admits management read selectors and returns the core query
// without its transport-only revision. The CLI uses the same admission rules.
func NotebookQuery(q url.Values) (int, []byte, error) {
	allowed := map[string]bool{"page": true, "database": true, "view": true, "sort": true, "filter": true, "format": true, "limit": true, "cursor": true, "op_id": true, "revision": true, "retry_hint": true}
	for k, vs := range q {
		if !allowed[k] || len(vs) != 1 || vs[0] == "" {
			return 0, nil, fmt.Errorf("invalid or repeated notebook query field %q", k)
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
	body := map[string]any{}
	if q.Has("cursor") {
		if len(q) != 1 && !(len(q) == 2 && q.Has("op_id")) {
			return 0, nil, errors.New("cursor accepts only op_id, not content selectors or revision")
		}
		body["cursor"] = q.Get("cursor")
		if q.Has("op_id") {
			if !notebookOpID.MatchString(q.Get("op_id")) {
				return 0, nil, errors.New("invalid op_id")
			}
			body["op_id"] = q.Get("op_id")
		}
		raw, err := json.Marshal(body)
		return 0, raw, err
	}
	if q.Has("op_id") {
		return 0, nil, errors.New("op_id requires a mapping cursor")
	}
	if q.Has("page") && q.Has("database") {
		return 0, nil, errors.New("select page or database, not both")
	}
	for _, field := range []struct {
		name string
		kind notebook.Kind
	}{{"page", notebook.PageKind}, {"database", notebook.DatabaseKind}, {"view", notebook.ViewKind}} {
		if !q.Has(field.name) {
			continue
		}
		v := q.Get(field.name)
		if strings.HasPrefix(v, "{") {
			var ref notebook.Ref
			if err := json.Unmarshal([]byte(v), &ref); err != nil || ref.Inc == 0 || ref.ValidateKind(field.kind) != nil {
				return 0, nil, fmt.Errorf("invalid %s reference", field.name)
			}
			body[field.name] = ref
		} else {
			id := notebook.ID(v)
			if err := id.ValidateKind(field.kind); err != nil {
				return 0, nil, fmt.Errorf("invalid %s id: %w", field.name, err)
			}
			body[field.name] = id
		}
	}
	if (q.Has("view") || q.Has("sort") || q.Has("filter")) && !q.Has("database") {
		return 0, nil, errors.New("view, sort and filter require database")
	}
	if q.Has("view") && (q.Has("sort") || q.Has("filter")) {
		return 0, nil, errors.New("view cannot be combined with sort or filter")
	}
	if q.Has("format") {
		f := q.Get("format")
		if f != "structured" && f != "markdown" || f == "markdown" && !q.Has("page") {
			return 0, nil, errors.New("format must be structured, or markdown with a page")
		}
		body["format"] = f
	}
	if q.Has("retry_hint") {
		body["retry_hint"] = q.Get("retry_hint")
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > notebook.MaxReadRecords {
			return 0, nil, fmt.Errorf("limit must be between 1 and %d", notebook.MaxReadRecords)
		}
		body["limit"] = n
	}
	if q.Has("sort") {
		var sorts []notebook.Sort
		if err := json.Unmarshal([]byte(q.Get("sort")), &sorts); err != nil || sorts == nil {
			return 0, nil, errors.New("sort must be an array of typed sort definitions")
		}
		for _, s := range sorts {
			if s.Property.Ref == nil || s.Property.Ref.Inc == 0 {
				return 0, nil, errors.New("sort requires live property references")
			}
		}
		body["sort"] = sorts
	}
	if q.Has("filter") {
		var filters []notebook.Filter
		if err := json.Unmarshal([]byte(q.Get("filter")), &filters); err != nil || filters == nil {
			return 0, nil, errors.New("filter must be an array of typed filter definitions")
		}
		for _, f := range filters {
			if f.Property.Ref == nil || f.Property.Ref.Inc == 0 {
				return 0, nil, errors.New("filter requires live property references")
			}
			value := bytes.TrimSpace(f.Value)
			var targets []notebook.Target
			if len(value) > 0 && value[0] == '{' {
				var target notebook.Target
				if err := json.Unmarshal(value, &target); err != nil {
					return 0, nil, err
				}
				targets = append(targets, target)
			} else if len(value) > 0 && value[0] == '[' {
				if err := json.Unmarshal(value, &targets); err != nil {
					return 0, nil, err
				}
			}
			for _, target := range targets {
				if target.Ref == nil || target.Ref.Inc == 0 {
					return 0, nil, errors.New("filter requires live option references")
				}
			}
		}
		body["filter"] = filters
	}
	raw, err := json.Marshal(body)
	return rev, raw, err
}

func (s *Server) getNotebook(w http.ResponseWriter, r *http.Request, _ core.Via) {
	getNotebook(w, r, s.Svc)
}

func getNotebook(w http.ResponseWriter, r *http.Request, svc notebookService) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		failNotebook(w, &notebook.WriteError{Code: "invalid", Detail: err.Error()})
		return
	}
	rev, query, err := NotebookQuery(q)
	if err == nil && r.Body != nil {
		var raw []byte
		raw, err = io.ReadAll(io.LimitReader(r.Body, 1))
		if err == nil && len(raw) != 0 {
			err = errors.New("GET notebook does not accept a body")
		}
	}
	if err != nil {
		failNotebook(w, &notebook.WriteError{Code: "invalid", Detail: err.Error()})
		return
	}
	out, err := svc.GetNotebook(r.Context(), r.PathValue("slug"), rev, query)
	if err != nil {
		failNotebook(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) updateNotebook(w http.ResponseWriter, r *http.Request, via core.Via) {
	updateNotebook(w, r, via, s.Svc)
}

func updateNotebook(w http.ResponseWriter, r *http.Request, via core.Via, svc notebookService) {
	if r.URL.RawQuery != "" {
		failNotebook(w, &notebook.WriteError{Code: "invalid", Detail: "POST notebook does not accept query fields"})
		return
	}
	uploadLimit := svc.UploadLimit()
	limit, err := notebook.MaxUpdateBodyBytes(uploadLimit)
	if err != nil {
		failNotebook(w, &notebook.WriteError{Code: "unavailable", Detail: err.Error()})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("%w: body exceeds update_notebook limit", core.ErrInvalid))
			return
		}
		failNotebook(w, &notebook.WriteError{Code: "invalid", Detail: err.Error()})
		return
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		failNotebook(w, &notebook.WriteError{Code: "invalid", Detail: "body must be an update_notebook object without slug"})
		return
	}
	slug, _ := json.Marshal(r.PathValue("slug"))
	withSlug := append([]byte(`{"slug":`), slug...)
	if len(bytes.TrimSpace(raw[1:len(raw)-1])) > 0 {
		withSlug = append(withSlug, ',')
	}
	withSlug = append(withSlug, raw[1:]...)
	update, err := notebook.DecodeUpdate(withSlug)
	if err != nil {
		failNotebook(w, &notebook.WriteError{Code: "invalid", Detail: err.Error()})
		return
	}
	remaining := uploadLimit
	for _, asset := range update.Assets {
		if int64(len(asset.Data)) > remaining {
			failNotebook(w, &notebook.WriteError{Code: "invalid", Reason: "asset_limit", Detail: "total decoded image bytes exceed upload_max_bytes"})
			return
		}
		remaining -= int64(len(asset.Data))
	}
	out, err := svc.UpdateNotebook(r.Context(), r.PathValue("slug"), raw, via)
	if err != nil {
		failNotebook(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func notebookStatus(err error) int {
	if errors.Is(err, core.ErrNotNotebook) {
		return http.StatusBadRequest
	}
	var refusal *notebook.WriteError
	if !errors.As(err, &refusal) {
		return 0
	}
	switch refusal.Code {
	case "invalid", "not_notebook":
		return http.StatusBadRequest
	case "conflict":
		return http.StatusConflict
	case "notebook_capacity":
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

func failNotebook(w http.ResponseWriter, err error) {
	var refusal *notebook.WriteError
	if !errors.As(err, &refusal) && errors.Is(err, core.ErrUnavailable) {
		err = &notebook.WriteError{Code: "unavailable", Reason: "notebook_unavailable", Retry: true, Detail: err.Error()}
	}
	if errors.As(err, &refusal) {
		writeJSON(w, statusOf(err), struct {
			ErrorBody
			Reason string `json:"reason,omitempty"`
			Retry  bool   `json:"retry"`
			Detail string `json:"detail,omitempty"`
		}{errorBody(err), refusal.Reason, refusal.Retry, refusal.Detail})
		return
	}
	fail(w, err)
}
