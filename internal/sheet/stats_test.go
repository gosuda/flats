package sheet

import (
	"context"
	"testing"
)

// TestRollbackCounts checks that an applied rollback is counted.
func TestRollbackCounts(t *testing.T) {
	e := start(t)
	e.mustAgent(numbers)
	rev := e.b.importAll(t)
	e.mustAgent(`{"ops":[{"op":"clear","tab":"Sheet1","ranges":["A1:B6"]}]}`)
	e.b.importAll(t)
	before := e.in.Stats()
	if _, err := e.b.Rollback(context.Background(), rev, "Alice"); err != nil {
		t.Fatal(err)
	}
	if after := e.in.Stats(); after.Applied != before.Applied+1 || after.Rejected != before.Rejected {
		t.Fatalf("a rollback moved the counters from %+v to %+v", before, after)
	}
	e.want("A1", "1")
}
