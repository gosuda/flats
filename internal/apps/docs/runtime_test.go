package docs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
	flatsruntime "github.com/gosuda/flats/internal/runtime"
)

var cacheRoot string
var managerOnce sync.Once
var sharedManager *flatsruntime.Manager

// Tests call t.Parallel unless their result depends on elapsed time or the
// process environment: t.Setenv, wall-clock latency or activation budgets,
// rate limits that refill over time, and requests that take a large share of
// the request timeout. Go runs those serial tests first and starts the
// parallel tests after them, so no other test of this package competes with
// them. A parallel test may bound a server-side timeout only with seconds of
// slack.
func TestMain(m *testing.M) {
	if os.Getenv("FLATS_DOCS_TEST_WORKER") == "1" {
		// Reported once the worker has its flat, so that a pre-started worker
		// reports it to its instance rather than to the Manager log.
		afterSpec(func() {
			fmt.Fprintf(os.Stderr, "{\"t\":\"log\",\"level\":\"info\",\"msg\":\"docs test worker pid:%d\"}\n", os.Getpid())
		})
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
	if sharedManager != nil {
		sharedManager.Close()
	}
	os.RemoveAll(cacheRoot)
	os.Exit(code)
}

// afterSpec runs fn in a test worker just before WorkerMain reads the spec
// line from stdin, which follows the prewarm line of a pre-started worker.
func afterSpec(fn func()) {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	in := os.Stdin
	os.Stdin = r
	go func() {
		defer w.Close()
		br := bufio.NewReader(in)
		done := false
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				if !done && !strings.HasPrefix(string(line), `{"prewarm":`) {
					done = true
					fn()
				}
				if _, werr := w.Write(line); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

// docsWorkerEnv is the environment of test document workers: the dispatch
// variable and the suite's document image switches (FLATS_NO_IMAGES,
// FLATS_FORCE_IMAGES), which the worker reads.
func docsWorkerEnv() []string {
	env := []string{"FLATS_DOCS_TEST_WORKER=1"}
	for _, k := range []string{"FLATS_NO_IMAGES", "FLATS_FORCE_IMAGES"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func manager(t *testing.T) *flatsruntime.Manager {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	managerOnce.Do(func() {
		sharedManager = &flatsruntime.Manager{DataDir: cacheRoot, Exe: exe, Env: docsWorkerEnv(), Logf: func(string, ...any) {}}
	})
	return sharedManager
}
func TestYjsQuickJSFeasibility(t *testing.T) {
	t.Parallel()
	start := time.Now()
	srv := startYjsTestModule(t, "spike.js")
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
	t.Logf("Yjs loaded, applied full/differential updates and state vector without TextEncoder/TextDecoder; elapsed %v; response %d bytes", time.Since(start), len(body))
}

// startYjsTestModule runs the committed, bundled Yjs test module in QuickJS.
func startYjsTestModule(t *testing.T, module string) *httptest.Server {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", module))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "server.js"), b, 0600); err != nil {
		t.Fatal(err)
	}
	inst, err := manager(t).Start(context.Background(), core.RuntimeSpec{Flat: "spike", Version: 1, Dir: dir, Entry: Entry, DataDir: t.TempDir(), Log: func(level, msg string) { t.Log(level, msg) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inst.Stop() })
	srv := httptest.NewServer(inst)
	t.Cleanup(srv.Close)
	return srv
}
