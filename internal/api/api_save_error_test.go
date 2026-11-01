package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/core"
)

func TestSavedDraftErrorPreservesCauseResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cause    error
		status   int
		category string
		health   bool
	}{
		{"version quota", fmt.Errorf("%w: %w", core.ErrConflict, core.ErrVersionQuota), 409, "conflict", false},
		{"disk quota", fmt.Errorf("%w: %w", core.ErrConflict, core.ErrDiskQuota), 409, "conflict", false},
		{"health", &core.DeployError{Health: core.HealthResult{Path: "/healthz", Status: 503, BodyHead: "not ready"}}, 422, "health_check_failed", true},
		{"start", &core.DeployError{Cause: errors.New("worker could not start")}, 422, "runtime_start_failed", false},
		{"snapshot", &core.DeployError{Cause: core.ErrSnapshotFailed}, 422, "snapshot_failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			fail(rr, &core.SavedDraftError{Revision: 7, Cause: tc.cause})
			var body ErrorBody
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rr.Code != tc.status || body.Category != tc.category || (body.Health != nil) != tc.health || !strings.Contains(body.Error, "Draft revision 7 was saved but not published") || !strings.Contains(body.Error, "Draft was kept") {
				t.Fatalf("cause was lost: %d %+v", rr.Code, body)
			}
			if tc.health && (body.Health.Status != 503 || body.Health.BodyHead != "not ready") {
				t.Fatalf("health was lost: %+v", body.Health)
			}
		})
	}
}

func TestDraftOnlySaveResponseBytes(t *testing.T) {
	out := SaveResponse{Draft: &SavedDraft{Type: "website", Flat: "site", Revision: 3, Hash: "abc", Files: 1, Kind: "static", SavedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}}
	rr := httptest.NewRecorder()
	writeJSON(rr, 201, out)
	const want = "{\n  \"draft\": {\n    \"type\": \"website\",\n    \"flat\": \"site\",\n    \"revision\": 3,\n    \"hash\": \"abc\",\n    \"size\": 0,\n    \"files\": 1,\n    \"kind\": \"static\",\n    \"git_dirty\": false,\n    \"saved_at\": \"2026-01-02T03:04:05Z\"\n  }\n}\n"
	if got := rr.Body.String(); got != want {
		t.Fatalf("Draft-only bytes changed:\n%s", got)
	}
}
