package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gosuda/flats/internal/core"
)

func TestTypedProviderFailureHTTPMapping(t *testing.T) {
	for _, tc := range []struct {
		err      error
		code     int
		category string
	}{
		{core.ErrProviderNotPermitted, 409, "provider_not_permitted"},
		{core.ErrProviderNotReady, 409, "provider_not_ready"},
		{errors.Join(core.ErrProviderNotReady, core.ErrProviderUnavailable), 409, "provider_unavailable"},
		{errors.Join(core.ErrUnchangedContent, core.ErrProviderNotReady), 409, "unchanged_content"},
		{core.ErrRuntimeUnavailable, 409, "runtime_unavailable"},
		{core.ErrProviderInUse, 409, "provider_in_use"},
		{core.ErrNotDeployed, 409, "not_deployed"},
		{core.ErrUnavailable, 409, "unavailable"},
		{core.ErrProviderUnavailable, 409, "provider_unavailable"},
		{core.ErrPublicStopUnconfirmed, 409, "public_stop_unconfirmed"},
		{core.ErrStaleApproval, 409, "stale_approval"},
		{core.ErrConfigOverridden, 409, "config_overridden"},
		{core.ErrConfigChanged, 412, "config_changed"},
		{errors.New("storage write failed"), 500, ""},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			w := httptest.NewRecorder()
			fail(w, fmt.Errorf("apply failed: %w", tc.err))
			var body ErrorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.code || body.Category != tc.category {
				t.Fatalf("got %d %+v", w.Code, body)
			}
		})
	}
}
