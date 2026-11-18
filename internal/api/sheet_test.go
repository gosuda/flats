package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/sheet"
	"github.com/gosuda/flats/internal/store"
)

type sheetFakeService struct {
	get    func(context.Context, string, int, []byte) (json.RawMessage, error)
	update func(context.Context, string, []byte, core.Via) (core.SheetWriteResult, error)
}

func (f sheetFakeService) GetSheet(ctx context.Context, slug string, rev int, q []byte) (json.RawMessage, error) {
	return f.get(ctx, slug, rev, q)
}

func (f sheetFakeService) UpdateSheet(ctx context.Context, slug string, raw []byte, via core.Via) (core.SheetWriteResult, error) {
	return f.update(ctx, slug, raw, via)
}

func sheetRequest(method, query, body string) *http.Request {
	r := httptest.NewRequest(method, "/api/flats/budget/sheet"+query, strings.NewReader(body))
	r.SetPathValue("slug", "budget")
	return r
}

func TestSheetQuery(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, query string
		rev         int
		want        string
	}{
		{"tabs", "", 0, `{}`},
		{"range", "tab=Budget&range=A1:D9&limit=100", 0, `{"tab":"Budget","range":"A1:D9","limit":100}`},
		{"csv of a revision", "tab=Q1+costs&format=csv&revision=7", 7, `{"tab":"Q1 costs","format":"csv"}`},
		{"include", "tab=Budget&include=shown,formats,widths", 0, `{"tab":"Budget","include":["shown","formats","widths"]}`},
		{"cursor", "cursor=c_x_1_2_0", 0, `{"cursor":"c_x_1_2_0"}`},
		{"cursor of a revision", "cursor=c_x_1_2_0&revision=3", 3, `{"cursor":"c_x_1_2_0"}`},
		{"tab list of a revision", "revision=2&format=json", 2, `{"format":"json"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q, _ := url.ParseQuery(tt.query)
			rev, raw, err := SheetQuery(q)
			if err != nil || rev != tt.rev || string(raw) != tt.want {
				t.Fatalf("query = %d %s, %v; want %d %s", rev, raw, err, tt.rev, tt.want)
			}
		})
	}
	for _, query := range []string{
		"page=p_1", "tab=", "tab=a&tab=b", "range=A1", "tab=Budget&format=xlsx", "format=csv",
		"tab=Budget&include=values", "tab=Budget&limit=0", "tab=Budget&limit=10001", "tab=Budget&limit=many",
		"revision=0", "revision=x", "cursor=c_x&tab=Budget", "cursor=c_x&limit=5", "cursor=c_x&revision=1&format=json",
	} {
		q, _ := url.ParseQuery(query)
		if _, _, err := SheetQuery(q); err == nil {
			t.Errorf("query %q was admitted", query)
		}
	}
}

func TestSheetRoutesAdmitAndMapRefusals(t *testing.T) {
	t.Parallel()
	var seen struct {
		slug, query, body string
		rev               int
	}
	svc := sheetFakeService{
		get: func(_ context.Context, slug string, rev int, q []byte) (json.RawMessage, error) {
			seen.slug, seen.rev, seen.query = slug, rev, string(q)
			return json.RawMessage(`{"state":"s_a.1","tabs":[]}`), nil
		},
		update: func(_ context.Context, slug string, raw []byte, via core.Via) (core.SheetWriteResult, error) {
			seen.slug, seen.body = slug, string(raw)
			return core.SheetWriteResult{Result: json.RawMessage(`{"state":"s_a.2"}`), Revision: 2}, nil
		},
	}
	w := httptest.NewRecorder()
	getSheet(w, sheetRequest("GET", "?tab=Budget&revision=4", ""), svc)
	if w.Code != 200 || seen.slug != "budget" || seen.rev != 4 || seen.query != `{"tab":"Budget"}` || !strings.Contains(w.Body.String(), `"s_a.1"`) {
		t.Fatalf("read = %d %s, seen %+v", w.Code, w.Body.String(), seen)
	}
	w = httptest.NewRecorder()
	updateSheet(w, sheetRequest("POST", "", ` {"state":"s_a.1","ops":[]} `), core.ViaAPI, svc)
	if w.Code != 200 || seen.body != `{"state":"s_a.1","ops":[]}` || !strings.Contains(sheetCompact(t, w.Body.Bytes()), `"revision":2`) {
		t.Fatalf("write = %d %s, seen %+v", w.Code, w.Body.String(), seen)
	}
	for _, tt := range []struct {
		name string
		run  func(http.ResponseWriter)
		code int
	}{
		{"unknown selector", func(w http.ResponseWriter) { getSheet(w, sheetRequest("GET", "?page=1", ""), svc) }, 400},
		{"read with a body", func(w http.ResponseWriter) { getSheet(w, sheetRequest("GET", "", `{}`), svc) }, 400},
		{"write with a query", func(w http.ResponseWriter) { updateSheet(w, sheetRequest("POST", "?tab=x", `{}`), core.ViaAPI, svc) }, 400},
		{"write of an array", func(w http.ResponseWriter) { updateSheet(w, sheetRequest("POST", "", `[]`), core.ViaAPI, svc) }, 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.run(w)
			if w.Code != tt.code {
				t.Fatalf("status = %d %s, want %d", w.Code, w.Body.String(), tt.code)
			}
		})
	}
	// A body longer than a call and the room beside it is the sheet's
	// call_bytes limit, with or without a declared length. A shorter one
	// goes to the sheet, which measures the call.
	limit, room := sheet.MaxUpdateBodyBytes(), sheet.MaxUpdateReadBytes()
	for _, declared := range []int64{-1, room + 1, room + 4096} {
		r := sheetRequest("POST", "", "")
		sent := max(declared, room+1)
		r.Body, r.ContentLength = io.NopCloser(io.LimitReader(zeroes{}, sent)), declared
		w := httptest.NewRecorder()
		seen.body = ""
		updateSheet(w, r, core.ViaAPI, svc)
		body := sheetCompact(t, w.Body.Bytes())
		want := fmt.Sprintf(`"limit":"call_bytes","max":%d`, limit)
		if declared > 0 {
			want += fmt.Sprintf(`,"value":%d`, declared)
		}
		if w.Code != 422 || !strings.Contains(body, `"category":"sheet_capacity"`) || !strings.Contains(body, want+`,"retry":false`) || seen.body != "" {
			t.Errorf("write over the limit (declared length %d) = %d %s, want 422 sheet_capacity with %s", declared, w.Code, body, want)
		}
	}
	for _, tt := range []struct {
		err      error
		code     int
		category string
		want     string
	}{
		{&sheet.WriteError{Code: "invalid", Reason: "at", Detail: "operation 1 (write): at is a cell address"}, 400, "invalid", `"detail":"operation 1 (write): at is a cell address"`},
		{fmt.Errorf("%w: budget is a notebook", core.ErrNotSheet), 400, "not_sheet", `"category":"not_sheet"`},
		{&sheet.WriteError{Code: "conflict", Reason: "tab_changed", Name: "Budget"}, 409, "conflict", `"reason":"tab_changed"`},
		{&sheet.WriteError{Code: "sheet_capacity", Limit: "cells", Max: 500000, Value: 500001}, 422, "sheet_capacity", `"limit":"cells","max":500000,"value":500001`},
		{&sheet.WriteError{Code: "sheet_capacity", Reason: "disk_quota"}, 422, "sheet_capacity", `"reason":"disk_quota"`},
		{&sheet.WriteError{Code: "unavailable", Reason: "calculating", Retry: true}, 503, "unavailable", `"retry":true`},
		{fmt.Errorf("%w: sheet budget is not running", core.ErrUnavailable), 503, "unavailable", `"reason":"sheet_unavailable"`},
		{&sheet.WriteError{Code: "not_found", Reason: "tab", Name: "Gone"}, 404, "not_found", `"name":"Gone"`},
		{&sheet.WriteError{Code: "forbidden"}, 403, "forbidden", `"retry":false`},
		{store.ErrNotFound, 404, "not_found", `"category":"not_found"`},
		{core.ErrSheetUnversioned, 400, "invalid", "update_sheet"},
	} {
		w := httptest.NewRecorder()
		failSheet(w, tt.err)
		body := sheetCompact(t, w.Body.Bytes())
		if w.Code != tt.code || !strings.Contains(body, `"category":"`+tt.category+`"`) || !strings.Contains(body, tt.want) {
			t.Errorf("%v = %d %s, want %d %s with %s", tt.err, w.Code, body, tt.code, tt.category, tt.want)
		}
	}
}

// sheetCompact removes the indentation of a JSON reply.
func sheetCompact(t *testing.T, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("reply is no JSON: %s", raw)
	}
	return b.String()
}

// zeroes is an endless body of white space.
type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}
