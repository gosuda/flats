package core

import (
	"errors"
	"fmt"
)

// kindError tags err with one of the sentinel kinds (ErrInvalid, ErrForbidden,
// ...) so callers can map it with errors.Is while the message stays err's own.
type kindError struct {
	kind error
	err  error
}

func (e *kindError) Error() string   { return e.err.Error() }
func (e *kindError) Unwrap() []error { return []error{e.kind, e.err} }

func withKind(kind, err error) error {
	if err == nil || errors.Is(err, kind) {
		return err
	}
	return &kindError{kind: kind, err: err}
}

func invalid(err error) error { return withKind(ErrInvalid, err) }

func invalidf(format string, args ...any) error { return invalid(fmt.Errorf(format, args...)) }

func forbiddenf(format string, args ...any) error {
	return withKind(ErrForbidden, fmt.Errorf(format, args...))
}

func unavailablef(format string, args ...any) error {
	return withKind(ErrUnavailable, fmt.Errorf(format, args...))
}
