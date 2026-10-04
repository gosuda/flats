package runtime

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func nearCapCount(full int) int {
	if os.Getenv("FLATS_FULL_SOAK") == "1" {
		return full
	}
	// Reach the near-cap threshold and sustain collection there in routine CI.
	return 120
}

func TestNearCapCycleCollection(t *testing.T) {
	for _, caught := range []bool{false, true} {
		t.Run(fmt.Sprintf("catch=%t", caught), func(t *testing.T) {
			body := `const graph={text:"x".repeat(3*1024*1024)};graph.self=graph;return new Response(String(++calls));`
			if caught {
				body = `try {` + body + `} catch(e) {return new Response("caught "+e.name);}`
			}
			code := `let calls=0;export default {fetch(){` + body + `}};`
			f := mustStart(t, newManager(t), "near-cap", map[string]string{"index.js": code}, "index.js", nil)
			count := nearCapCount(600)
			for i := 1; i <= count; i++ {
				r := f.get(t, "/")
				if r.status != 200 || r.body != fmt.Sprint(i) {
					t.Fatalf("request %d: %d %s; %s", i, r.status, r.body, f.logs)
				}
			}
			t.Logf("%d requests, 3 MiB cyclic garbage each, zero failures or replacements", count)
		})
	}
}

func TestNearCapLiveStateCycleCollection(t *testing.T) {
	code := `const state=Array.from({length:36},()=>"s".repeat(1024*1024));let calls=0;
export default {fetch(){const graph={text:"x".repeat(1024*1024)};graph.self=graph;
return new Response(state.length+":"+state[0].length+":"+state[35].length+":"+(++calls));}};`
	f := mustStart(t, newManager(t), "near-cap-live", map[string]string{"index.js": code}, "index.js", nil)
	count := nearCapCount(900)
	for i := 1; i <= count; i++ {
		r := f.get(t, "/")
		want := fmt.Sprintf("36:1048576:1048576:%d", i)
		if r.status != 200 || r.body != want {
			t.Fatalf("request %d: %d %s; %s", i, r.status, r.body, f.logs)
		}
	}
	t.Logf("%d requests, 36 MiB live state + 1 MiB cyclic garbage each, zero failures", count)
}

func TestNearCapWebSocketCycleCollection(t *testing.T) {
	code := `let calls=0;export default {websocket:{message(ws){const graph={text:"x".repeat(3*1024*1024)};graph.self=graph;ws.send(String(++calls));}}};`
	f := mustStart(t, newManager(t), "near-cap-ws", map[string]string{"index.js": code}, "index.js", nil)
	addr := strings.TrimPrefix(f.srv.URL, "http://")
	dial := func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", addr) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, err := dialWS(ctx, dial, addr, "/")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.c.Close()
	count := nearCapCount(600)
	for i := 1; i <= count; i++ {
		ws.c.SetDeadline(time.Now().Add(10 * time.Second))
		if err := ws.writeFrame(opText, []byte("next")); err != nil {
			t.Fatal(err)
		}
		got, err := ws.readMessage()
		if err != nil || got != fmt.Sprint(i) {
			t.Fatalf("message %d: %q %v; %s", i, got, err, f.logs)
		}
	}
	t.Logf("%d messages, 3 MiB cyclic garbage each, zero closures or reconnects", count)
}
