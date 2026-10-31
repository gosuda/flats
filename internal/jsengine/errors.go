package jsengine

import (
	"errors"
	"strings"
)

var (
	ErrBusy               = errors.New("engine busy")
	ErrPoisoned           = errors.New("engine poisoned")
	ErrClosed             = errors.New("engine closed")
	ErrNoDeadline         = errors.New("context has no deadline")
	ErrTooLarge           = errors.New("input or output exceeds size limit")
	ErrSnapshotNotAllowed = errors.New("snapshot not allowed without image path")
	ErrWasmMismatch       = errors.New("qjs.wasm sha-256 mismatch")
	ErrImageCorrupt       = errors.New("corrupt image")
	ErrImageKey           = errors.New("image key mismatch")
	ErrTainted            = errors.New("engine tainted")
)

type ScriptError struct {
	Name    string
	Message string
	Stack   string
}

func (e *ScriptError) Error() string {
	s := e.Message
	if e.Name != "" && !strings.HasPrefix(s, e.Name+":") {
		s = e.Name + ": " + s
	}
	if e.Stack != "" {
		s += "\n" + strings.TrimRight(e.Stack, "\n")
	}
	return s
}

type Fatal struct {
	Cause   error
	Message string
	Stack   string
}

func (e *Fatal) Error() string {
	s := e.Message
	if s == "" && e.Cause != nil {
		s = e.Cause.Error()
	}
	if e.Stack != "" {
		s += "\n" + strings.TrimRight(e.Stack, "\n")
	}
	return "fatal: " + s
}

func (e *Fatal) Unwrap() error {
	return e.Cause
}

func (e *Fatal) Is(target error) bool {
	return target == ErrPoisoned || errors.Is(e.Cause, target)
}
