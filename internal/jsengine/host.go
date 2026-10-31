package jsengine

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/gosuda/flats/internal/qjs"
)

type HostFunc func(ctx context.Context, op string, args []byte) ([]byte, error)

type lineWriter struct {
	mu      sync.Mutex
	sink    io.Writer
	buf     []byte
	omitted int
}

func newLineWriter(sink io.Writer) *lineWriter {
	return &lineWriter{sink: sink}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.sink == nil {
		return n, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.appendFragment(p)
			break
		}
		w.appendFragment(p[:i])
		w.emit()
		p = p[i+1:]
	}
	return n, nil
}

func (w *lineWriter) appendFragment(p []byte) {
	if w.omitted > 0 {
		w.omitted += len(p)
		return
	}
	if len(p) > maxLogLine+1-len(w.buf) {
		w.omitted = len(w.buf) + len(p)
		w.buf = w.buf[:0]
		return
	}
	w.buf = append(w.buf, p...)
}

func (w *lineWriter) emit() {
	if w.sink == nil {
		w.buf = w.buf[:0]
		w.omitted = 0
		return
	}
	if w.omitted > 0 {
		msg := fmt.Sprintf("log line omitted: %d bytes is more than the %d KiB limit\n", w.omitted, maxLogLine>>10)
		_, _ = w.sink.Write([]byte(msg))
	} else if len(w.buf) > 0 {
		line := strings.TrimRight(string(w.buf), "\r")
		if line != "" {
			_, _ = w.sink.Write([]byte(line + "\n"))
		}
	}
	w.buf = w.buf[:0]
	w.omitted = 0
}

func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emit()
}

func (e *Engine) hostCall(this *qjs.This) (*qjs.Value, error) {
	e.hostTainted.Store(true)
	if e.rt != nil {
		e.rt.Taint("host operation")
	}
	args := this.Args()
	defer func() {
		for _, a := range args {
			a.Free()
		}
	}()
	if len(args) > 2 {
		return nil, ErrTooLarge
	}
	if len(args) != 2 {
		return nil, errors.New("bad host call")
	}

	maxHost := cmp.Or(e.spec.MaxHostArgBytes, DefaultMaxHostArgBytes)

	op := args[0].String()
	raw := args[1].String()
	if len(op) > maxHost || len(raw) > maxHost {
		return nil, ErrTooLarge
	}

	e.gcBytes += uint64(len(op) + len(raw))

	if e.host == nil {
		return nil, errors.New("host function not configured")
	}

	var (
		res []byte
		err error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				e.hostPanicked.Store(true)
				e.hostPanicVal.Store(&r)
				panic(r)
			}
		}()
		res, err = e.host(e.swapCtx.current(), op, []byte(raw))
	}()

	if err != nil {
		return nil, err
	}

	e.gcBytes += uint64(len(res))
	c := this.Context()
	return c.NewString(string(res)), nil
}

func (e *Engine) codecCall(this *qjs.This) (*qjs.Value, error) {
	args := this.Args()
	defer func() {
		for _, a := range args {
			a.Free()
		}
	}()
	if len(args) > 2 {
		return nil, ErrTooLarge
	}
	if len(args) != 2 {
		return nil, errors.New("invalid docs codec arguments")
	}

	maxHost := cmp.Or(e.spec.MaxHostArgBytes, DefaultMaxHostArgBytes)

	op := args[0].String()
	if len(op) > maxHost {
		return nil, ErrTooLarge
	}

	if e.spec.RawCodec != nil {
		inVal := Value{raw: args[1], ctx: this.Context()}
		if inVal.IsByteArray() {
			if inVal.ByteLen() > int64(maxHost) {
				return nil, ErrTooLarge
			}
			e.gcBytes += uint64(inVal.ByteLen())
		} else {
			s := inVal.String()
			if len(s) > maxHost {
				return nil, ErrTooLarge
			}
			e.gcBytes += uint64(len(s))
		}

		outVal, err := e.spec.RawCodec(op, inVal)
		if err != nil {
			return nil, err
		}
		return e.toQjsValue(this.Context(), outVal, args[1])
	}

	if e.spec.DocsCodec != nil {
		var in []byte
		if args[1].IsByteArray() {
			in = args[1].ToByteArray()
		} else {
			in = []byte(args[1].String())
		}
		if len(in) > maxHost {
			return nil, ErrTooLarge
		}
		e.gcBytes += uint64(len(in))
		res, err := e.spec.DocsCodec(e.swapCtx.current(), op, in)
		if err != nil {
			return nil, err
		}
		e.gcBytes += uint64(len(res))
		return this.Context().NewArrayBuffer(res), nil
	}

	return nil, errors.New("docs codec not configured")
}

func (e *Engine) toQjsValue(c *qjs.Context, v Value, inArg *qjs.Value) (*qjs.Value, error) {
	if v.raw != nil {
		if v.raw.IsByteArray() {
			e.gcBytes += uint64(v.raw.ByteLen())
		} else if v.raw.IsString() {
			e.gcBytes += uint64(len(v.raw.String()))
		} else {
			e.gcBytes += 4
		}
		if inArg != nil && v.raw == inArg {
			return v.raw.Clone(), nil
		}
		return v.raw, nil
	}
	switch v.kind {
	case valString:
		return c.NewString(v.str), nil
	case valInt32:
		e.gcBytes += 4
		return c.NewInt32(v.i32), nil
	case valArrayBuffer:
		e.gcBytes += uint64(len(v.buf))
		return c.NewArrayBuffer(v.buf), nil
	default:
		return c.NewUndefined(), nil
	}
}

func (e *Engine) setupHostFunctions(c *qjs.Context) {
	c.SetFunc("__flats_host", e.hostCall)
	if e.hasCodec() {
		c.SetFunc("__flats_docs_codec", e.codecCall)
	}
}
