package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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

func TestBoardCommands(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "flats.db"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := local.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := core.New(context.Background(), core.Config{DataDir: filepath.Join(dir, "data"), Store: st, Private: priv, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Svc: svc}).Handler())
	t.Cleanup(func() { srv.Close(); svc.Close(); priv.Close(); st.Close() })

	ops := filepath.Join(dir, "ops.json")
	if err := os.WriteFile(ops, []byte(`{"ops":[{"op":"add","ref":"$a","el":{"type":"sticky","text":"alpha"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, srv.URL, "", "board", "apply", "plan", ops, "--message", "first")
	if r.code != 0 || !strings.Contains(r.stdout, "Applied new board plan: revision 2") || !strings.Contains(r.stdout, "$a") {
		t.Fatalf("apply: %+v", r)
	}
	r = run(t, srv.URL, "", "board", "get", "plan", "--outline")
	if r.code != 0 || !strings.Contains(r.stdout, "Board plan at seq") || !strings.Contains(r.stdout, `"text":"alpha"`) {
		t.Fatalf("get: %+v", r)
	}
	r = run(t, srv.URL, "", "--json", "board", "get", "plan")
	var page struct {
		Elements []map[string]any `json:"elements"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &page) != nil || len(page.Elements) != 1 || page.Elements[0]["ver"] == nil {
		t.Fatalf("get --json: %+v", r)
	}
	f, _ := svc.GetFlat(context.Background(), "plan")
	resp, err := http.Get(f.PrivateURL + "/_board/export.zip")
	if err != nil {
		t.Fatal(err)
	}
	zipped, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	file := filepath.Join(dir, "plan.zip")
	if err := os.WriteFile(file, zipped, 0o600); err != nil {
		t.Fatal(err)
	}
	r = run(t, srv.URL, "", "board", "import", "plan", file, "--replace")
	if r.code != 0 || !strings.Contains(r.stdout, "Imported "+file+" into board plan: revision 3") {
		t.Fatalf("import: %+v", r)
	}
	// A retry with the same --op-id after a timeout imports the file once.
	r = run(t, srv.URL, "", "board", "import", "plan", file, "--op-id", "retry-import-1")
	if r.code != 0 || !strings.Contains(r.stdout, "board plan: revision 4") || !strings.Contains(r.stdout, "op_id: retry-import-1") {
		t.Fatalf("import with --op-id: %+v", r)
	}
	r = run(t, srv.URL, "", "board", "import", "plan", file, "--op-id", "retry-import-1")
	if r.code != 0 || !strings.Contains(r.stdout, "board plan: revision 4") || !strings.Contains(r.stdout, "already applied") {
		t.Fatalf("resent import: %+v", r)
	}
	if r = run(t, srv.URL, "", "board", "import", "plan", file, "--op-id", "short"); r.code != ExitUsage {
		t.Fatalf("short --op-id: %+v", r)
	}
	r = run(t, srv.URL, "", "board", "get", "site")
	if r.code != 1 || !strings.Contains(r.stderr, "error:") {
		t.Fatalf("get of a missing flat: %+v", r)
	}
	if r = run(t, srv.URL, "", "board", "draw", "plan"); r.code != ExitUsage {
		t.Fatalf("unknown subcommand: %+v", r)
	}
}
