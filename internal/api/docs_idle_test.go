package api

import (
	"fmt"
	"github.com/gosuda/flats/internal/core"
	"strings"
	"testing"
)

func TestDocsIdleCommittedSaveErrorTransport(t *testing.T) {
	err := &core.DeployError{Committed: true, Document: true, Cause: fmt.Errorf("%w: worker could not become ready", core.ErrUnavailable)}
	body := errorBody(err)
	if statusOf(err) != 409 || body.Category != "unavailable" || body.Health != nil {
		t.Fatalf("status=%d body=%+v", statusOf(err), body)
	}
	for _, want := range []string{"saved and committed", "Do not repeat", "later request retries", "logs"} {
		if !strings.Contains(body.Error, want) {
			t.Fatalf("missing %q: %s", want, body.Error)
		}
	}
	if strings.Contains(body.Error, "previous text") {
		t.Fatal("committed save reported unchanged")
	}
}
