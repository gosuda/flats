package runtime

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// Measure complete HTTP requests through a real, reused QuickJS worker. Include
// enough calls to include several collections; a percentile alone hides pauses.
func TestLargeModuleRequestCost(t *testing.T) {
	for _, entries := range []int{0, 50000, 200000} {
		t.Run(fmt.Sprint(entries), func(t *testing.T) {
			code := fmt.Sprintf(`const state=Array.from({length:%d},(_,i)=>({id:i,text:"entry "+i}));
export default {fetch(){return new Response(String(state.length))}};`, entries)
			f := mustStart(t, newManager(t), "gc-cost", map[string]string{"index.js": code}, "index.js", nil)
			for range 8 {
				f.get(t, "/")
			}
			const count = 192
			durations := make([]time.Duration, 0, count)
			var total time.Duration
			for range count {
				start := time.Now()
				r := f.get(t, "/")
				d := time.Since(start)
				if r.status != 200 || r.body != fmt.Sprint(entries) {
					t.Fatalf("request: %d %s; %s", r.status, r.body, f.logs)
				}
				durations = append(durations, d)
				total += d
			}
			slices.Sort(durations)
			mean := total / count
			t.Logf("entries=%d n=%d p50=%s p95=%s mean=%s max=%s", entries, count,
				durations[count/2], durations[count*95/100], mean, durations[count-1])
			// Loose enough for slower CI, but rejects full live-heap scanning
			// on every request (200k entries previously averaged ~100 ms).
			if entries == 200000 && mean > 40*time.Millisecond {
				t.Fatalf("large-state amortized request cost %s exceeds 40ms", mean)
			}
		})
	}
}
