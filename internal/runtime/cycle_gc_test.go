package runtime

import (
	"fmt"
	"testing"
)

// A bounded live heap must remain usable when dead request-local graphs contain
// cycles. The acyclic control separates reference counting from cycle collection.
func TestRequestCycleCollection(t *testing.T) {
	for _, cyclic := range []bool{false, true} {
		t.Run(fmt.Sprintf("cyclic=%t", cyclic), func(t *testing.T) {
			link := ""
			if cyclic {
				link = "graph.self=graph;"
			}
			// Guest replacement cannot disable or impersonate the host collector.
			code := `globalThis.gc=()=>{throw new Error("guest collector called")}; export default {async fetch(){const graph={text:"x".repeat(1048576)};` + link + `return new Response(String(graph.text.length));}};`
			f := mustStart(t, newManager(t), "cycle-gc", map[string]string{"index.js": code}, "index.js", nil)
			for i := 0; i < 200; i++ {
				r := f.do(t, "GET", "/", "")
				if r.status != 200 || r.body != "1048576" {
					t.Fatalf("GET %d: %d %s %s", i, r.status, r.body, f.logs)
				}
			}
			t.Log("200 GETs allocating request-local 1 MiB graph, zero failures")
		})
	}
}
