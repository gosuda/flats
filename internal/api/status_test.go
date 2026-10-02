package api

import (
	"errors"
	"fmt"
	"testing"

	"github.com/oesni/flats/internal/core"
	"github.com/oesni/flats/internal/slug"
	"github.com/oesni/flats/internal/store"
)

func TestStatusOfUsesTypedErrors(t *testing.T) {
	wrap := func(err error, msg string) error { return fmt.Errorf("%w: %s", err, msg) }
	cases := []struct {
		err  error
		want int
	}{
		{wrap(core.ErrInvalid, "bad version"), 400},
		{wrap(slug.ErrInvalid, `"X" must start with a lowercase letter`), 400},
		{wrap(core.ErrForbidden, "agents cannot do that"), 403},
		{wrap(core.ErrUnavailable, "public exposure is disabled on this host"), 409},
		{wrap(core.ErrConflict, `flat "demo" already exists`), 409},
		{core.ErrNotDeployed, 409},
		{wrap(store.ErrNotFound, "flat x"), 404},
		{&core.DeployError{}, 422},
		// A typed error wins over words in its message that the fallback
		// would read differently.
		{wrap(core.ErrUnavailable, "the operator must enable Portal"), 409},
		{wrap(core.ErrInvalid, "name already exists in the operator's list"), 400},
		// Untyped server faults stay 500.
		{errors.New("disk I/O error"), 500},
	}
	for _, c := range cases {
		if got := statusOf(c.err); got != c.want {
			t.Errorf("statusOf(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
