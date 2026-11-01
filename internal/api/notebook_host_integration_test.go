package api_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/flats/internal/api"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/expose/local"
	"github.com/gosuda/flats/internal/store"
)

func TestHostNotebookActualUnavailableResponses(t *testing.T) {
	ctx := context.Background()
	for _, failure := range []string{"startup", "measurement"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.Open(filepath.Join(dir, "host.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			priv, err := local.Listen("127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer priv.Close()
			cfg := core.Config{DataDir: dir, Store: st, Private: priv, Logf: t.Logf}
			svc, err := core.New(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if svc != nil {
					svc.Close()
				}
			}()
			if _, err := svc.CreateNotebook(ctx, "notes", "Notes", core.ViaAPI); err != nil {
				t.Fatal(err)
			}
			if failure == "startup" {
				if err := svc.Close(); err != nil {
					t.Fatal(err)
				}
				svc = nil
				if err := os.WriteFile(filepath.Join(dir, "flats", "notes", "notebook", "notebook.db"), []byte("not a database"), 0o600); err != nil {
					t.Fatal(err)
				}
				svc, err = core.New(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				view, err := svc.GetFlat(ctx, "notes")
				if err != nil || view.Running {
					t.Fatalf("corrupt notebook unexpectedly started: %+v, %v", view, err)
				}
			} else {
				blocked := filepath.Join(dir, "flats", "notes", "unmeasurable")
				if err := os.Mkdir(blocked, 0o000); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(blocked, 0o700)
				if _, err := os.ReadDir(blocked); err == nil {
					t.Skip("filesystem permits reading mode-000 directories")
				}
			}
			srv := httptest.NewServer((&api.Server{Svc: svc}).Handler())
			defer srv.Close()
			methods := []string{"POST"}
			if failure == "startup" {
				methods = append(methods, "GET")
			}
			for _, method := range methods {
				body := ""
				if method == "POST" {
					body = `{"op_id":"retry-unavailable","ops":[{"op":"create_page","ref":"page","parent":null,"before":null,"title":"Notes"}]}`
				}
				code, out := req(t, method, srv.URL+"/api/flats/notes/notebook", strings.NewReader(body), map[string]string{"X-Flats-Client": "cli", "Content-Type": "application/json"})
				if code != 503 || out["category"] != "unavailable" || out["retry"] != true || out["detail"] == "" {
					t.Fatalf("%s %s unavailable = %d %+v", failure, method, code, out)
				}
			}
		})
	}
}
