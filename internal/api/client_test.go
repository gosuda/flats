package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gosuda/flats/internal/clientstate"
)

func TestClientManifest(t *testing.T) {
	srv, _ := setup(t)
	resp, err := http.Get(srv.URL + "/api/client")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m clientstate.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if err := clientstate.ValidateManifest(m); err != nil {
		t.Fatal(err)
	}
	if m.ServerURL != srv.URL || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("manifest must identify request origin and forbid stale cache")
	}
}
