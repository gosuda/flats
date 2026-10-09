package core

import (
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// ErrorCategories must list exactly the values ErrorCategory returns, so
// every category has refusal documentation.
func TestErrorCategoriesListEveryCategory(t *testing.T) {
	errs := []error{
		ErrStaleApproval, ErrPublicStopUnconfirmed, ErrProviderNotPermitted, ErrProviderUnavailable,
		ErrRuntimeUnavailable, ErrUnavailable, ErrProviderInUse, ErrNotDeployed, ErrUnchangedContent,
		ErrProviderNotReady, ErrConfigOverridden, ErrConfigChanged, &DeployError{Cause: ErrInvalid},
		&DeployError{}, ErrConflict, ErrForbidden, ErrNotDocs, ErrDocumentNotFound, store.ErrNotFound, ErrInvalid,
	}
	var got []string
	for _, err := range errs {
		got = append(got, ErrorCategory(err))
	}
	if !slices.Equal(got, ErrorCategories) {
		t.Fatalf("categories = %v, want %v", got, ErrorCategories)
	}
}
