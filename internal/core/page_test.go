package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
)

func pageFiles(body string) []bundle.File {
	return []bundle.File{
		{Path: bundle.ManifestName, Data: []byte(`{"type":"page","name":"Sales Report"}`)},
		{Path: "index.html", Data: []byte(`<!doctype html><title>Sales Report</title><h1>` + body + `</h1><script src="chart.js"></script>`)},
		{Path: "chart.js", Data: []byte("// chart")},
	}
}

// served fetches path from the flat's private host.
func served(t *testing.T, s *Service, host, path string) (int, string) {
	t.Helper()
	n := s.cfg.Private.(*memNet)
	n.mu.Lock()
	h := n.hosts[host]
	n.mu.Unlock()
	if h == nil {
		t.Fatalf("host %s is not served", host)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://"+host+".test"+path, nil))
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b)
}

func TestCreatePageStarterDraft(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	f, err := s.CreatePage(ctx, "report", "Q3 <Sales> Report", ViaAPI)
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != contenttype.Page || f.LiveVersion != 0 || f.Draft == nil || f.Draft.Revision != 1 || f.Name != "Q3 <Sales> Report" {
		t.Fatalf("created page: type %q live %d draft %+v name %q", f.Type, f.LiveVersion, f.Draft, f.Name)
	}
	if _, err := s.CreatePage(ctx, "report", "", ViaAPI); err == nil {
		t.Fatal("existing slug was not refused")
	}
	// The starter meets the checked parts of the page contract.
	starter := strings.ReplaceAll(pageStarter, "{{title}}", "Q3 &lt;Sales&gt; Report")
	for _, w := range bundle.Lint([]bundle.File{{Path: "index.html", Data: []byte(starter)}, {Path: bundle.ManifestName, Data: []byte(`{"type":"page"}`)}}) {
		if strings.Contains(w.Message, "title") || strings.Contains(w.Message, "viewport") {
			t.Fatalf("starter warning: %+v", w)
		}
	}
	// It follows the website lifecycle: nothing serves until an approved
	// publish, and then the page is served statically.
	if _, err := approvedInternalDeploy(t, s, ctx, "report", 0); err != nil {
		t.Fatal(err)
	}
	if code, body := served(t, s, "report", "/"); code != 200 || !strings.Contains(body, "<title>Q3 &lt;Sales&gt; Report</title>") {
		t.Fatalf("served starter: %d %s", code, body)
	}
	if code, _ := served(t, s, "report", "/missing"); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}
}

func TestPageServesWithoutRuntime(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t) // no Runtime configured
	if _, err := s.SaveVersion(ctx, "report", pageFiles("Revenue"), SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "report", 0); err != nil {
		t.Fatal(err)
	}
	if code, body := served(t, s, "report", "/"); code != 200 || !strings.Contains(body, "Revenue") {
		t.Fatalf("served page: %d %s", code, body)
	}
	if code, body := served(t, s, "report", "/chart.js"); code != 200 || body != "// chart" {
		t.Fatalf("served asset: %d %s", code, body)
	}
	f, _ := s.GetFlat(ctx, "report")
	if f.Type != contenttype.Page || f.LiveVersion != 1 || f.Live == nil || f.Live.Kind != "static" {
		t.Fatalf("published page: %+v", f.Flat)
	}
}

func TestFlatKeepsItsType(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	website := []bundle.File{{Path: "index.html", Data: []byte("<title>Site</title>")}}

	// A page Draft, not yet published, refuses other types; the refusal
	// names both types and how to update the page.
	if _, err := s.CreatePage(ctx, "report", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	_, err := s.SaveVersion(ctx, "report", website, SaveMeta{}, ViaAPI)
	if !errors.Is(err, ErrTypeChange) || !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "is a page and this save is a website") || !strings.Contains(err.Error(), `{"type":"page"}`) {
		t.Fatalf("website into page Draft: %v", err)
	}
	// Documents need the runtime this test host lacks; check the rule itself.
	pf, _ := s.st.GetFlat(ctx, "report")
	if err := s.checkTypeChange(ctx, pf, contenttype.Docs); !errors.Is(err, ErrTypeChange) || !strings.Contains(err.Error(), "this save is a document") {
		t.Fatalf("document into page Draft: %v", err)
	}
	if _, err := s.SaveVersion(ctx, "report", pageFiles("v2"), SaveMeta{}, ViaAPI); err != nil {
		t.Fatalf("page into page: %v", err)
	}

	// A website Draft refuses a page.
	if _, err := s.SaveVersion(ctx, "site", website, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, "site", pageFiles("x"), SaveMeta{}, ViaAPI); !errors.Is(err, ErrTypeChange) || !strings.Contains(err.Error(), "is a website and this save is a page; use another slug") {
		t.Fatalf("page into website Draft: %v", err)
	}

	// A live page refuses a website.
	if _, err := approvedInternalDeploy(t, s, ctx, "report", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, "report", website, SaveMeta{}, ViaAPI); !errors.Is(err, ErrTypeChange) {
		t.Fatalf("website into live page: %v", err)
	}

	// A flat created without a type takes the type of its first save.
	if _, err := s.CreateFlat(ctx, "fresh", "", ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, "fresh", pageFiles("first"), SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.GetFlat(ctx, "fresh"); f.Type != contenttype.Page {
		t.Fatalf("fresh flat type: %q", f.Type)
	}
}

// Content of another type than the live version, left by an earlier
// release, can neither be requested nor applied.
func TestMixedTypeContentNeverGoesLive(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	if _, err := s.SaveVersion(ctx, "site", []bundle.File{{Path: "index.html", Data: []byte("<title>Site</title>")}}, SaveMeta{}, ViaAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := approvedInternalDeploy(t, s, ctx, "site", 0); err != nil {
		t.Fatal(err)
	}
	// A page Draft written past the save check, as an older release could.
	files, err := bundle.FromFiles(pageFiles("legacy"), bundle.Limits{MaxBytes: s.UploadLimit()})
	if err != nil {
		t.Fatal(err)
	}
	unlock := s.lock("site")
	rev, err := s.saveDraftLocked(ctx, "site", files, SaveMeta{}, ViaAPI)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, "site", 0, "", ViaAPI); !errors.Is(err, ErrTypeChange) {
		t.Fatalf("publish request: %v", err)
	}
	if _, err := s.Deploy(ctx, "site", 0, ViaAPI); !errors.Is(err, ErrTypeChange) {
		t.Fatalf("deploy request: %v", err)
	}
	// An approval requested before this check is refused when applied.
	f, _ := s.st.GetFlat(ctx, "site")
	key, err := s.providerKey(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	unlock = s.lock("site")
	res, err := s.requestFrozen(ctx, "site", "publish", publishParams{Revision: rev.Revision, Hash: rev.Hash, BaseVersion: f.LiveVersion, Visibility: string(f.Visibility.Canonical()), Providers: key}, ViaAPI, "")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if res.Approval == nil {
		t.Fatalf("no approval: %+v", res)
	}
	a, err := s.Decide(ctx, res.Approval.ID, true)
	if a.Status == "approved" {
		t.Fatalf("mixed-type publish applied: %+v %v", a, err)
	}
	after, _ := s.GetFlat(ctx, "site")
	if after.Type != contenttype.Flat || after.LiveVersion != f.LiveVersion {
		t.Fatalf("live flat changed: type %q live %d", after.Type, after.LiveVersion)
	}
}

func TestPageRollbackHasNoData(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	for _, body := range []string{"v1", "v2"} {
		if _, err := s.SaveVersion(ctx, "report", pageFiles(body), SaveMeta{}, ViaAPI); err != nil {
			t.Fatal(err)
		}
		if _, err := approvedInternalDeploy(t, s, ctx, "report", 0); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.ListApprovals(ctx, "pending")
	if _, err := s.RollbackWithData(ctx, "report", 1, true, ViaAPI); !errors.Is(err, ErrConflict) {
		t.Fatalf("rollback with restore_data: %v", err)
	}
	if after, _ := s.ListApprovals(ctx, "pending"); len(after) != len(before) {
		t.Fatalf("restore_data created an approval: %d -> %d", len(before), len(after))
	}
	_, err := s.Rollback(ctx, "report", 1, ViaAPI)
	var p *PendingApproval
	if !errors.As(err, &p) {
		t.Fatalf("plain rollback: %v", err)
	}
	var params rollbackParams
	if json.Unmarshal(p.Approval.Params, &params) != nil || params.Version != 1 || params.RestoreData {
		t.Fatalf("rollback approval params: %s", p.Approval.Params)
	}
}
