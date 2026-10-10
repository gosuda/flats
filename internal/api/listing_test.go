package api_test

import (
	"strings"
	"testing"
)

func TestPortalListingIsConsoleOnly(t *testing.T) {
	srv, _ := setup(t)
	saveAndPublish(t, srv, "site", "ok")
	// The agent API has no route for it.
	if code, _ := req(t, "PUT", srv.URL+"/api/flats/site/listing", strings.NewReader(`{"listing":"listed"}`), nil); code != 404 && code != 405 {
		t.Fatalf("agent listing change = %d", code)
	}
	code, out := req(t, "PUT", srv.URL+"/console/api/flats/site/listing", strings.NewReader(`{"listing":"hidden"}`), consoleHdr(t, srv))
	if code != 200 || out["portal_listing"] != "hidden" || out["portal_hidden"] != true {
		t.Fatalf("console listing change: %d %v", code, out)
	}
	if code, _ := req(t, "PUT", srv.URL+"/console/api/flats/site/listing", strings.NewReader(`{"listing":"unlisted"}`), consoleHdr(t, srv)); code != 400 {
		t.Fatalf("invalid listing = %d", code)
	}
	if code, out := req(t, "GET", srv.URL+"/api/flats/site", nil, nil); code != 200 || out["portal_listing"] != "hidden" {
		t.Fatalf("agent view: %d %v", code, out)
	}
	code, out = req(t, "PUT", srv.URL+"/console/api/settings", strings.NewReader(`{"portal_hide":"true"}`), consoleHdr(t, srv))
	if code != 200 {
		t.Fatalf("settings: %d %v", code, out)
	}
	applied, _ := out["applied"].([]any)
	if len(applied) != 1 || applied[0] != "portal_hide" {
		t.Fatalf("portal_hide should apply without restart: %v", out)
	}
}
