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
		if s == "" {
			s = e.Name
		} else {
			s = e.Name + ": " + s
		}
	}
	if e.Stack != "" {
		s += "\n" + strings.TrimRight(e.Stack, "\n")
	}
	return s
}

func isIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if r != '_' && r != '$' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
				return false
			}
		} else {
			if r != '_' && r != '$' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

func parseScriptError(err error) *ScriptError {
	if err == nil {
		return nil
	}
	if existing, ok := errors.AsType[*ScriptError](err); ok {
		return existing
	}
	s := err.Error()
	var stack string
	if cause, stackPart, found := strings.Cut(s, "\n    at "); found {
		s = cause
		stack = "    at " + stackPart
	}
	var name, message string
	line1, rest, hasNL := strings.Cut(s, "\n")
	if candidateName, msg1, hasColon := strings.Cut(line1, ": "); hasColon && isIdentifier(candidateName) {
		name = candidateName
		if hasNL {
			message = msg1 + "\n" + rest
		} else {
			message = msg1
		}
	} else if !hasNL && isIdentifier(s) && strings.HasSuffix(s, "Error") {
		name = s
		message = ""
	} else {
		message = s
	}
	return &ScriptError{
		Name:    name,
		Message: message,
		Stack:   stack,
	}
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
