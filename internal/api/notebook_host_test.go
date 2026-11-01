package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

func TestHostNotebookUnavailableStatus(t *testing.T) {
	w := httptest.NewRecorder()
	failNotebook(w, fmt.Errorf("%w: notebook is not running; inspect get_logs", core.ErrUnavailable))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 503 || body["category"] != "unavailable" || body["retry"] != true || body["detail"] == "" {
		t.Fatalf("notebook failure = %d %s", w.Code, w.Body.String())
	}
	if got := statusOf(core.ErrUnavailable); got != 409 {
		t.Fatalf("shared unavailable mapping changed: %d", got)
	}
}
