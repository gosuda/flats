package runtime

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
)

func TestRuntimeGenerationHostInternalsAndRecovery(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"server.js": `
function metadata(value) {
 const d = Object.getOwnPropertyDescriptor(value, "runtimeGeneration");
 return {generation: value.runtimeGeneration, enumerable: d.enumerable, writable: d.writable,
   legacy: typeof value.runtimeVersion, sendLimitsEnumerable: Object.keys(Object.getPrototypeOf(value)).includes("setSendLimits")};
}
export default {fetch(request) { return Response.json(metadata(request)); },
 websocket: {open(ws) { ws.send(JSON.stringify(metadata(ws))); }}};`})
	var next atomic.Int64
	next.Store(40)
	inst, err := newManager(t).Start(t.Context(), core.RuntimeSpec{Flat: "generation", Version: 0, Generation: 40,
		NextGeneration: func(context.Context) (int64, error) { return next.Add(1), nil }, Dir: dir, Entry: "server.js", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop()
	srv := httptest.NewServer(inst)
	defer srv.Close()
	check := func(raw string, generation int64) {
		t.Helper()
		var value struct {
			Generation                                 int64
			Enumerable, Writable, SendLimitsEnumerable bool
			Legacy                                     string
		}
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err, raw)
		}
		if value.Generation != generation || value.Enumerable || value.Writable || value.SendLimitsEnumerable || value.Legacy != "undefined" {
			t.Fatal(raw, generation)
		}
	}
	f := &flat{srv: srv, inst: inst, logs: &logs{}}
	check(f.get(t, "/").body, 40)
	addr := strings.TrimPrefix(srv.URL, "http://")
	dial := func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", addr) }
	ws, err := dialWS(t.Context(), dial, addr, "/")
	if err != nil {
		t.Fatal(err)
	}
	ws.c.SetDeadline(time.Now().Add(5 * time.Second))
	raw, err := ws.readMessage()
	if err != nil {
		t.Fatal(err)
	}
	check(raw, 40)
	ws.c.Close()
	in := inst.(*instance)
	if err := syscall.Kill(in.pid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for in.pid() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	check(f.get(t, "/").body, 41)
}
