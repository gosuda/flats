package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	flatsruntime "github.com/gosuda/flats/internal/runtime"
)

var cacheRoot string
var managerOnce sync.Once
var sharedManager *flatsruntime.Manager

func TestMain(m *testing.M) {
	if os.Getenv("FLATS_DOCS_TEST_WORKER") == "1" {
		fmt.Fprintf(os.Stderr, "{\"t\":\"log\",\"level\":\"info\",\"msg\":\"docs test worker pid:%d\"}\n", os.Getpid())
		if err := flatsruntime.WorkerMain(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	var err error
	cacheRoot, err = os.MkdirTemp("/tmp", "flats-docs-test-")
	if err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(cacheRoot)
	os.Exit(code)
}
func manager(t *testing.T) *flatsruntime.Manager {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	managerOnce.Do(func() {
		sharedManager = &flatsruntime.Manager{DataDir: cacheRoot, Exe: exe, Env: []string{"FLATS_DOCS_TEST_WORKER=1"}, Logf: func(string, ...any) {}}
	})
	return sharedManager
}
func TestYjsQuickJSFeasibility(t *testing.T) {
	b, err := os.ReadFile("testdata/spike.js")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "server.js"), b, 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	inst, err := manager(t).Start(context.Background(), core.RuntimeSpec{Flat: "spike", Version: 1, Dir: dir, Entry: Entry, DataDir: t.TempDir(), Log: func(level, msg string) { t.Log(level, msg) }})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop()
	srv := httptest.NewServer(inst)
	defer srv.Close()
	r, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	var result struct {
		Text, Encoder, Decoder string
		First, SV, Diff        []int
	}
	if err = json.Unmarshal(body, &result); err != nil {
		t.Fatalf("%d %s: %v", r.StatusCode, body, err)
	}
	if result.Text != "한국어 🌍\nsecond" || len(result.SV) == 0 || len(result.Diff) == 0 || result.Encoder != "undefined" || result.Decoder != "undefined" {
		t.Fatalf("unexpected: %s", body)
	}
	t.Logf("Yjs loaded, applied full/differential updates and state vector without TextEncoder/TextDecoder; elapsed %v; bundle %d bytes", time.Now().Sub(start), len(b))
}
