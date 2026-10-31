package jsengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func validContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestNormalEvalAndCall(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	out, err := eng.Eval(ctx, "expr.js", "1 + 2", false)
	if err != nil || out != "3" {
		t.Fatalf("Eval expr: %q, %v", out, err)
	}

	out, err = eng.Eval(ctx, "async.js", "Promise.resolve(40 + 2)", false)
	if err != nil || out != "42" {
		t.Fatalf("Eval async: %q, %v", out, err)
	}

	_, err = eng.Eval(ctx, "mod.js", "export const value = 100;", true)
	if err != nil {
		t.Fatalf("Eval module: %v", err)
	}

	_, err = eng.Eval(ctx, "setup.js", "globalThis.greet = (name) => 'hello ' + name;", false)
	if err != nil {
		t.Fatalf("Eval setup: %v", err)
	}

	out, err = eng.Call(ctx, "greet", "world")
	if err != nil || out != "hello world" {
		t.Fatalf("Call greet: %q, %v", out, err)
	}

	_, err = eng.Eval(ctx, "setupAsync.js", "globalThis.asyncAdd = async (n) => String(Number(n) + 1);", false)
	if err != nil {
		t.Fatalf("Eval setupAsync: %v", err)
	}

	out, err = eng.Call(ctx, "asyncAdd", "5")
	if err != nil || out != "6" {
		t.Fatalf("Call asyncAdd: %q, %v", out, err)
	}
}

func TestErrBusyConcurrent(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})

	host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
		if op == "block" {
			close(started)
			<-unblock
			return []byte("done"), nil
		}
		return []byte("fast"), nil
	}

	eng, err := Open(Spec{}, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, err = eng.Eval(ctx, "setup.js", "globalThis.callHost = (op) => __flats_host(op, '');", false)
	if err != nil {
		t.Fatalf("Eval setup: %v", err)
	}

	var (
		res1 string
		err1 error
		wg   sync.WaitGroup
	)
	wg.Go(func() {
		res1, err1 = eng.Call(ctx, "callHost", "block")
	})

	<-started

	_, errBusyCall := eng.Call(ctx, "callHost", "fast")
	if !errors.Is(errBusyCall, ErrBusy) {
		t.Fatalf("concurrent Call: got %v, want ErrBusy", errBusyCall)
	}

	_, errBusyEval := eng.Eval(ctx, "t.js", "1 + 1", false)
	if !errors.Is(errBusyEval, ErrBusy) {
		t.Fatalf("concurrent Eval: got %v, want ErrBusy", errBusyEval)
	}

	close(unblock)
	wg.Wait()

	if err1 != nil || res1 != "done" {
		t.Fatalf("call1: %q, %v", res1, err1)
	}

	resAfter, errAfter := eng.Eval(ctx, "after.js", "20 + 22", false)
	if errAfter != nil || resAfter != "42" {
		t.Fatalf("after call: %q, %v", resAfter, errAfter)
	}
}

func TestFatalPanickingHostFunction(t *testing.T) {
	host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
		if op == "panic" {
			panic("host exploded")
		}
		return []byte("ok"), nil
	}

	eng, err := Open(Spec{}, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, err = eng.Eval(ctx, "setup.js", "globalThis.trigger = () => __flats_host('panic', '');", false)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err = eng.Call(ctx, "trigger", "")
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("trigger panic: got %v, want ErrPoisoned", err)
	}
	var f *Fatal
	if !errors.As(err, &f) {
		t.Fatalf("trigger panic: got %T, want *Fatal", err)
	}

	_, errAfter := eng.Eval(ctx, "t.js", "1", false)
	if !errors.Is(errAfter, ErrPoisoned) {
		t.Fatalf("call after poison: got %v, want ErrPoisoned", errAfter)
	}
}

func TestFatalPendingExceptionLeak(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	eng.setTestHookPendingException(true)

	_, err = eng.Eval(ctx, "t.js", "42", false)
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("pending exception leak: got %v, want ErrPoisoned", err)
	}
	var f *Fatal
	if !errors.As(err, &f) {
		t.Fatalf("pending exception leak: got %T, want *Fatal", err)
	}

	_, errAfter := eng.Eval(ctx, "t.js", "1", false)
	if !errors.Is(errAfter, ErrPoisoned) {
		t.Fatalf("call after poison: got %v, want ErrPoisoned", errAfter)
	}
}

func TestFatalHandleLedgerLeak(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	eng.setTestHookLeakHandle(true)

	_, err = eng.Eval(ctx, "t.js", "42", false)
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("handle leak: got %v, want ErrPoisoned", err)
	}
	var f *Fatal
	if !errors.As(err, &f) {
		t.Fatalf("handle leak: got %T, want *Fatal", err)
	}

	_, errAfter := eng.Eval(ctx, "t.js", "1", false)
	if !errors.Is(errAfter, ErrPoisoned) {
		t.Fatalf("call after poison: got %v, want ErrPoisoned", errAfter)
	}
}

func TestFatalWasmTrapOrDeepRecursion(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, err = eng.Eval(ctx, "overflow.js", "function recurse() { return recurse(); } recurse();", false)
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("recursion overflow: got %v, want ErrPoisoned", err)
	}
	var f *Fatal
	if !errors.As(err, &f) {
		t.Fatalf("recursion overflow: got %T, want *Fatal", err)
	}

	_, errAfter := eng.Eval(ctx, "t.js", "1", false)
	if !errors.Is(errAfter, ErrPoisoned) {
		t.Fatalf("call after poison: got %v, want ErrPoisoned", errAfter)
	}
}

func TestErrNoDeadline(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	_, err = eng.Call(context.Background(), "greet", "arg")
	if !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("Call without deadline: got %v, want ErrNoDeadline", err)
	}

	_, err = eng.Eval(context.Background(), "t.js", "1", false)
	if !errors.Is(err, ErrNoDeadline) {
		t.Fatalf("Eval without deadline: got %v, want ErrNoDeadline", err)
	}

	ctx := validContext(t)
	out, err := eng.Eval(ctx, "t.js", "1 + 1", false)
	if err != nil || out != "2" {
		t.Fatalf("Eval after ErrNoDeadline: %q, %v", out, err)
	}
}

func TestBoundaryMaxInputBytes(t *testing.T) {
	const n = 10
	eng, err := Open(Spec{MaxInputBytes: n, Prelude: "globalThis.echo = (x) => x;"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)

	inNMinus1 := strings.Repeat("x", n-1)
	inN := strings.Repeat("x", n)
	inNPlus1 := strings.Repeat("x", n+1)

	out, err := eng.Call(ctx, "echo", inNMinus1)
	if err != nil || out != inNMinus1 {
		t.Fatalf("Call N-1: %q, %v", out, err)
	}

	out, err = eng.Call(ctx, "echo", inN)
	if err != nil || out != inN {
		t.Fatalf("Call N: %q, %v", out, err)
	}

	_, err = eng.Call(ctx, "echo", inNPlus1)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Call N+1: got %v, want ErrTooLarge", err)
	}

	srcNMinus1 := "/*" + strings.Repeat("a", n-5) + "*/"
	srcN := "/*" + strings.Repeat("a", n-4) + "*/"
	srcNPlus1 := "/*" + strings.Repeat("a", n-3) + "*/"

	_, err = eng.Eval(ctx, "evalNMinus1.js", srcNMinus1, false)
	if err != nil {
		t.Fatalf("Eval N-1: %v", err)
	}

	_, err = eng.Eval(ctx, "evalN.js", srcN, false)
	if err != nil {
		t.Fatalf("Eval N: %v", err)
	}

	_, err = eng.Eval(ctx, "evalNPlus1.js", srcNPlus1, false)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Eval N+1: got %v, want ErrTooLarge", err)
	}

	out, err = eng.Eval(ctx, "alive.js", "'ok'", false)
	if err != nil || out != "ok" {
		t.Fatalf("engine unusable after ErrTooLarge: %q, %v", out, err)
	}
}

func TestBoundaryMaxOutputBytes(t *testing.T) {
	const n = 10
	eng, err := Open(Spec{MaxOutputBytes: n}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)

	out, err := eng.Eval(ctx, "outNMinus1.js", "'x'.repeat(9)", false)
	if err != nil || out != strings.Repeat("x", 9) {
		t.Fatalf("Eval out N-1: %q, %v", out, err)
	}

	out, err = eng.Eval(ctx, "outN.js", "'x'.repeat(10)", false)
	if err != nil || out != strings.Repeat("x", 10) {
		t.Fatalf("Eval out N: %q, %v", out, err)
	}

	_, err = eng.Eval(ctx, "outNPlus1.js", "'x'.repeat(11)", false)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Eval out N+1: got %v, want ErrTooLarge", err)
	}

	out, err = eng.Eval(ctx, "alive.js", "'ok'", false)
	if err != nil || out != "ok" {
		t.Fatalf("engine unusable after ErrTooLarge: %q, %v", out, err)
	}
}

func TestDoubleCloseAndUseAfterClose(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := eng.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	ctx := validContext(t)
	_, err = eng.Call(ctx, "greet", "world")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Call after Close: got %v, want ErrClosed", err)
	}

	_, err = eng.Eval(ctx, "t.js", "1 + 1", false)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Eval after Close: got %v, want ErrClosed", err)
	}
}

func TestGuestExceptionLeavesEngineUsable(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, err = eng.Eval(ctx, "throw.js", "throw new Error('guest boom')", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrPoisoned) {
		t.Fatalf("guest exception poisoned engine: %v", err)
	}
	var se *ScriptError
	if !errors.As(err, &se) {
		t.Fatalf("expected *ScriptError, got %T: %v", err, err)
	}

	out, err := eng.Eval(ctx, "recover.js", "40 + 2", false)
	if err != nil || out != "42" {
		t.Fatalf("Eval after script error: %q, %v", out, err)
	}
}

func TestPrewarmShell(t *testing.T) {
	shell, err := NewShell(Spec{})
	if err != nil {
		t.Fatalf("NewShell: %v", err)
	}
	if !shell.Compatible(Spec{}) {
		t.Fatal("shell should be compatible with matching Spec")
	}

	eng, err := Open(Spec{Shell: shell}, nil)
	if err != nil {
		t.Fatalf("Open with Shell: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	out, err := eng.Eval(ctx, "t.js", "123", false)
	if err != nil || out != "123" {
		t.Fatalf("Eval with shell: %q, %v", out, err)
	}
}

func TestImageSnapshot(t *testing.T) {
	eng, err := Open(Spec{ImagePath: "/tmp/fake-image"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	snap, err := eng.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap) == 0 {
		t.Fatal("empty snapshot")
	}

	img, err := UnmarshalImage(snap)
	if err != nil {
		t.Fatalf("UnmarshalImage: %v", err)
	}
	if !img.Compatible(Spec{}) {
		t.Fatal("unmarshaled image not compatible")
	}

	plainEng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open plain: %v", err)
	}
	defer plainEng.Close()

	_, err = plainEng.Snapshot()
	if !errors.Is(err, ErrSnapshotNotAllowed) {
		t.Fatalf("plain snapshot: got %v, want ErrSnapshotNotAllowed", err)
	}
}

func TestRawCodecOpDispatch(t *testing.T) {
	ctx := validContext(t)
	spec := Spec{
		RawCodec: func(op string, in Value) (Value, error) {
			switch op {
			case "echo":
				return in.NewString("echo:" + in.String()), nil
			case "length":
				return in.NewInt32(int32(len(in.String()))), nil
			case "reverse":
				if !in.IsByteArray() {
					return Value{}, errors.New("not byte array")
				}
				b := in.ToByteArray()
				slices.Reverse(b)
				return in.NewArrayBuffer(b), nil
			default:
				return Value{}, errors.New("unknown op: " + op)
			}
		},
	}
	eng, err := Open(spec, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	out, err := eng.Eval(ctx, "test.js", `__flats_docs_codec("echo", "hello")`, false)
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if out != "echo:hello" {
		t.Fatalf("got %q, want %q", out, "echo:hello")
	}

	out, err = eng.Eval(ctx, "test.js", `__flats_docs_codec("length", "testing")`, false)
	if err != nil {
		t.Fatalf("length: %v", err)
	}
	if out != "7" {
		t.Fatalf("got %q, want 7", out)
	}

	out, err = eng.Eval(ctx, "test.js", `
		const u8 = new Uint8Array([1, 2, 3]);
		const res = __flats_docs_codec("reverse", u8.buffer);
		Array.from(new Uint8Array(res)).join(",")
	`, false)
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if out != "3,2,1" {
		t.Fatalf("got %q, want %q", out, "3,2,1")
	}
}

func TestStdoutStderrSplit(t *testing.T) {
	ctx := validContext(t)
	var stdout, stderr bytes.Buffer
	spec := Spec{
		Stdout: &stdout,
		Stderr: &stderr,
	}
	eng, err := Open(spec, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	_, err = eng.Eval(ctx, "test.js", `print("stdout output\n"); "ok"`, false)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if stdout.String() != "stdout output\n" {
		t.Fatalf("stdout: got %q, want %q", stdout.String(), "stdout output\n")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr not empty: %q", stderr.String())
	}
}

func TestImageBindingCompatibility(t *testing.T) {
	eng, err := Open(Spec{ImagePath: "/tmp/fake-image", ImageBinding: "binding-alpha"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	snap, err := eng.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	img, err := UnmarshalImage(snap)
	if err != nil {
		t.Fatalf("UnmarshalImage: %v", err)
	}

	if !img.Compatible(Spec{ImageBinding: "binding-alpha"}) {
		t.Fatal("compatible binding rejected")
	}
	if img.Compatible(Spec{ImageBinding: "binding-beta"}) {
		t.Fatal("different binding accepted")
	}
	if img.Compatible(Spec{}) {
		t.Fatal("empty binding accepted for bound image")
	}
}

func TestSnapshotTaintRefusal(t *testing.T) {
	ctx := validContext(t)
	hostCalled := false
	host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
		hostCalled = true
		return []byte("ok"), nil
	}
	eng, err := Open(Spec{ImagePath: "/tmp/fake-image"}, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	if err := eng.ImageEligible(); err != nil {
		t.Fatalf("expected eligible before host call: %v", err)
	}

	_, err = eng.Eval(ctx, "test.js", `__flats_host("test", "arg")`, false)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !hostCalled {
		t.Fatal("host was not called")
	}

	if err := eng.ImageEligible(); err == nil {
		t.Fatal("expected ImageEligible error after host call")
	}
	_, err = eng.Snapshot()
	if err == nil {
		t.Fatal("expected Snapshot error after host call")
	}
	if !errors.Is(err, ErrTainted) && !strings.Contains(err.Error(), "tainted") {
		t.Fatalf("Snapshot: got %v, want ErrTainted", err)
	}
}

func TestReleasedShellOpenFailure(t *testing.T) {
	shell, err := NewShell(Spec{})
	if err != nil {
		t.Fatalf("NewShell: %v", err)
	}
	if err := shell.Close(); err != nil {
		t.Fatalf("shell.Close: %v", err)
	}
	_, err = Open(Spec{Shell: shell}, nil)
	if err == nil {
		t.Fatal("expected Open to fail on released shell")
	}
}

func TestLineWriterReturnsN(t *testing.T) {
	var buf bytes.Buffer
	w := newLineWriter(&buf)

	p1 := []byte("first line\n")
	n, err := w.Write(p1)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(p1) {
		t.Fatalf("got n=%d, want %d", n, len(p1))
	}

	p2 := []byte("partial")
	n, err = w.Write(p2)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(p2) {
		t.Fatalf("got n=%d, want %d", n, len(p2))
	}

	p3 := []byte(" second line\n")
	n, err = w.Write(p3)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(p3) {
		t.Fatalf("got n=%d, want %d", n, len(p3))
	}

	if buf.String() != "first line\npartial second line\n" {
		t.Fatalf("got %q, want %q", buf.String(), "first line\npartial second line\n")
	}
}

func TestBareHostGlobalAbsent(t *testing.T) {
	ctx := validContext(t)
	host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
		return []byte("ok"), nil
	}
	eng, err := Open(Spec{}, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	out, err := eng.Eval(ctx, "test.js", "typeof host", false)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if out != "undefined" {
		t.Fatalf("got %q, want %q", out, "undefined")
	}
}

func TestCaptureAndRestoreWithCodec(t *testing.T) {
	ctx := validContext(t)
	spec := Spec{
		ImagePath: "/tmp/fake-image",
		RawCodec: func(op string, in Value) (Value, error) {
			if op == "ping" {
				return in.NewString("pong:" + in.String()), nil
			}
			return Value{}, errors.New("unknown op")
		},
	}
	eng, err := Open(spec, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	snap, err := eng.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	img, err := UnmarshalImage(snap)
	if err != nil {
		t.Fatalf("UnmarshalImage: %v", err)
	}

	restoreSpec := Spec{
		Image: &img,
		RawCodec: func(op string, in Value) (Value, error) {
			if op == "ping" {
				return in.NewString("restored:" + in.String()), nil
			}
			return Value{}, errors.New("unknown op")
		},
	}
	restoredEng, err := Open(restoreSpec, nil)
	if err != nil {
		t.Fatalf("Open restored: %v", err)
	}
	defer restoredEng.Close()

	if !restoredEng.Restored() {
		t.Fatal("expected Restored() to be true")
	}

	out, err := restoredEng.Eval(ctx, "test.js", `__flats_docs_codec("ping", "work")`, false)
	if err != nil {
		t.Fatalf("Eval on restored: %v", err)
	}
	if out != "restored:work" {
		t.Fatalf("got %q, want %q", out, "restored:work")
	}
}

func TestGCDrained(t *testing.T) {
	ctx := validContext(t)
	eng, err := Open(Spec{Prelude: "globalThis.step = () => {};"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	if eng.GCDrained() {
		t.Fatal("expected GCDrained false before collections")
	}

	for range 32 {
		_, err = eng.Call(ctx, "step", "")
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
	}
	if !eng.GCDrained() {
		t.Fatal("expected GCDrained true after 32 calls")
	}
}

func TestCensus(t *testing.T) {
	ctx := validContext(t)
	census := &Census{}
	eng, err := Open(Spec{Census: census}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	_, err = eng.Eval(ctx, "test.js", `print("census test\n"); "ok"`, false)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if census.OutputBytes.Load() == 0 {
		t.Fatal("expected OutputBytes to be > 0")
	}
}

func TestSnapshotExclusion(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})
	host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
		close(started)
		<-unblock
		return []byte("ok"), nil
	}
	eng, err := Open(Spec{ImagePath: "/tmp/fake-snap", Host: host}, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	callDone := make(chan error, 1)
	go func() {
		_, err := eng.Eval(ctx, "run.js", `__flats_host("block", "")`, false)
		callDone <- err
	}()

	<-started
	_, snapErr := eng.Snapshot()
	close(unblock)
	<-callDone

	if !errors.Is(snapErr, ErrBusy) {
		t.Fatalf("Snapshot during Call: got %v, want ErrBusy", snapErr)
	}

	_, _ = eng.Eval(ctx, "recurse.js", `function r() { return r(); } r();`, false)
	_, snapAfterPoison := eng.Snapshot()
	if !errors.Is(snapAfterPoison, ErrPoisoned) {
		t.Fatalf("Snapshot on poisoned engine: got %v, want ErrPoisoned", snapAfterPoison)
	}

	_ = eng.Close()
	_, snapAfterClose := eng.Snapshot()
	if !errors.Is(snapAfterClose, ErrClosed) {
		t.Fatalf("Snapshot on closed engine: got %v, want ErrClosed", snapAfterClose)
	}
}

func TestCloseWaitsForInFlightCall(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})
	host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
		close(started)
		<-unblock
		return []byte("ok"), nil
	}
	eng, err := Open(Spec{Host: host}, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx := validContext(t)
	evalDone := make(chan error, 1)
	go func() {
		_, err := eng.Eval(ctx, "run.js", `__flats_host("block", "")`, false)
		evalDone <- err
	}()

	<-started
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- eng.Close()
	}()

	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before in-flight call finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(unblock)

	if err := <-evalDone; err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := eng.Close(); err != nil {
		t.Fatalf("subsequent Close: %v", err)
	}
	_, err = eng.Eval(ctx, "t.js", "1", false)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Eval after Close: got %v, want ErrClosed", err)
	}
}

func TestCloseFromHostRefusesBusy(t *testing.T) {
	var closeErr error
	var eng *Engine
	host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
		closeErr = eng.Close()
		return []byte("ok"), nil
	}
	var err error
	eng, err = Open(Spec{Host: host}, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, err = eng.Eval(ctx, "run.js", `__flats_host("close", "")`, false)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !errors.Is(closeErr, ErrBusy) {
		t.Fatalf("Close from host: got %v, want ErrBusy", closeErr)
	}
}

func TestPostCallChecksOnErrorPath(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	eng.setTestHookPendingException(true)

	_, err = eng.Eval(ctx, "t.js", `throw new Error("guest error")`, false)
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("error path with pending exception: got %v, want ErrPoisoned", err)
	}
	if _, ok := errors.AsType[*Fatal](err); !ok {
		t.Fatalf("error path with pending exception: got %T, want *Fatal", err)
	}

	_, errAfter := eng.Eval(ctx, "t.js", "1", false)
	if !errors.Is(errAfter, ErrPoisoned) {
		t.Fatalf("call after poison: got %v, want ErrPoisoned", errAfter)
	}

	eng2, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open eng2: %v", err)
	}
	defer eng2.Close()

	eng2.setTestHookLeakHandle(true)
	_, err = eng2.Eval(ctx, "t.js", `throw new Error("guest error")`, false)
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("error path with handle leak: got %v, want ErrPoisoned", err)
	}
	if _, ok := errors.AsType[*Fatal](err); !ok {
		t.Fatalf("error path with handle leak: got %T, want *Fatal", err)
	}
}

func TestCountImageFuncs(t *testing.T) {
	eng, err := Open(Spec{ImagePath: "/tmp/fake-image"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	snap, err := eng.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	img, err := UnmarshalImage(snap)
	if err != nil {
		t.Fatalf("UnmarshalImage: %v", err)
	}
	if img.numFuncs != 1 {
		t.Fatalf("numFuncs: got %d, want 1", img.numFuncs)
	}
}

func TestHostResultOverLimit(t *testing.T) {
	maxHost := 64
	host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
		return bytes.Repeat([]byte("a"), maxHost+1), nil
	}
	eng, err := Open(Spec{MaxHostArgBytes: maxHost, Host: host}, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, err = eng.Eval(ctx, "t.js", `__flats_host("op", "x")`, false)
	if err == nil {
		t.Fatal("expected error for host result over limit")
	}
	if errors.Is(err, ErrPoisoned) {
		t.Fatalf("host result over limit poisoned engine: %v", err)
	}
	if _, ok := errors.AsType[*ScriptError](err); !ok {
		t.Fatalf("got %T, want *ScriptError", err)
	}

	out, errAfter := eng.Eval(ctx, "t.js", "1 + 1", false)
	if errAfter != nil || out != "2" {
		t.Fatalf("engine not reusable after host limit: %v", errAfter)
	}
}

func TestImageEligiblePoisoned(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, _ = eng.Eval(ctx, "recurse.js", `function r() { return r(); } r();`, false)
	err = eng.ImageEligible()
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("ImageEligible on poisoned engine: got %v, want ErrPoisoned", err)
	}
}

func TestCallMissingFuncNoHandleLeak(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, err = eng.Call(ctx, "nonexistent", "arg")
	if err == nil {
		t.Fatal("expected error calling missing func")
	}
	if eng.liveHandles.Load() != 0 {
		t.Fatalf("live handles after missing func call: got %d, want 0", eng.liveHandles.Load())
	}
}

func TestCompatibleWithDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/data.txt", []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	spec := Spec{
		ImagePath: "/tmp/fake-dir-image",
		Dir:       dir,
	}
	eng, err := Open(spec, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	snap, err := eng.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	img, err := UnmarshalImage(snap)
	if err != nil {
		t.Fatalf("UnmarshalImage: %v", err)
	}

	if !img.Compatible(spec) {
		t.Fatal("img.Compatible(spec) returned false with matching Dir")
	}

	noDirSpec := spec
	noDirSpec.Dir = ""
	if img.Compatible(noDirSpec) {
		t.Fatal("img.Compatible(noDirSpec) returned true with empty Dir")
	}
}

func TestDeadlineClosedModuleFatal(t *testing.T) {
	eng, err := Open(Spec{TimeoutMS: 50}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	_, err = eng.Eval(ctx, "loop.js", `while (true) {}`, false)
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("deadline exceeded: got %v, want ErrPoisoned", err)
	}
	if _, ok := errors.AsType[*Fatal](err); !ok {
		t.Fatalf("got %T, want *Fatal", err)
	}

	ctx2 := validContext(t)
	_, errAfter := eng.Eval(ctx2, "t.js", "1", false)
	if !errors.Is(errAfter, ErrPoisoned) {
		t.Fatalf("call after poison: got %v, want ErrPoisoned", errAfter)
	}
}

func TestOutOfMemoryFatal(t *testing.T) {
	eng, err := Open(Spec{MemoryPages: 256}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx := validContext(t)
	_, err = eng.Eval(ctx, "oom.js", `
		let arr = [];
		while (true) {
			arr.push(new Uint8Array(1024 * 1024));
		}
	`, false)
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("oom: got %v, want ErrPoisoned", err)
	}
	if _, ok := errors.AsType[*Fatal](err); !ok {
		t.Fatalf("got %T, want *Fatal", err)
	}

	_, errAfter := eng.Eval(ctx, "t.js", "1", false)
	if !errors.Is(errAfter, ErrPoisoned) {
		t.Fatalf("call after poison: got %v, want ErrPoisoned", errAfter)
	}
}

func TestMaxHostArgBytesBoundaries(t *testing.T) {
	const limit = 64
	var lastLen int
	host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
		lastLen = len(args)
		switch op {
		case "echo":
			return args, nil
		case "ret":
			return bytes.Repeat([]byte("b"), len(args)), nil
		}
		return []byte("ok"), nil
	}
	eng, err := Open(Spec{MaxHostArgBytes: limit, Host: host}, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)

	for _, n := range []int{limit - 1, limit} {
		arg := strings.Repeat("x", n)
		out, err := eng.Eval(ctx, "t.js", fmt.Sprintf(`__flats_host("echo", %q)`, arg), false)
		if err != nil || out != arg {
			t.Fatalf("arg len %d failed: %v", n, err)
		}
		if lastLen != n {
			t.Fatalf("host received len %d, want %d", lastLen, n)
		}
	}

	argOver := strings.Repeat("x", limit+1)
	_, err = eng.Eval(ctx, "t.js", fmt.Sprintf(`__flats_host("echo", %q)`, argOver), false)
	if err == nil {
		t.Fatalf("arg len %d succeeded, want ErrTooLarge", limit+1)
	}
	if errors.Is(err, ErrPoisoned) {
		t.Fatalf("arg limit poisoned engine: %v", err)
	}

	for _, n := range []int{limit - 1, limit} {
		arg := strings.Repeat("y", n)
		out, err := eng.Eval(ctx, "t.js", fmt.Sprintf(`__flats_host("ret", %q)`, arg), false)
		if err != nil || len(out) != n {
			t.Fatalf("ret len %d failed: %v", n, err)
		}
	}

	argRetOver := strings.Repeat("y", limit+1)
	_, err = eng.Eval(ctx, "t.js", fmt.Sprintf(`__flats_host("ret", %q)`, argRetOver), false)
	if err == nil {
		t.Fatalf("ret len %d succeeded, want ErrTooLarge", limit+1)
	}
	if errors.Is(err, ErrPoisoned) {
		t.Fatalf("ret limit poisoned engine: %v", err)
	}

	out, errAfter := eng.Eval(ctx, "t.js", "1 + 1", false)
	if errAfter != nil || out != "2" {
		t.Fatalf("engine not reusable: %v", errAfter)
	}
}

func TestDeadlineClamping(t *testing.T) {
	eng, err := Open(Spec{TimeoutMS: 100}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	longCtx, longCancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer longCancel()

	start := time.Now()
	_, err = eng.Eval(longCtx, "loop.js", `while (true) {}`, false)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("long context loop: got %v, want ErrPoisoned", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("clamped timeout took %v, want <= 2s", elapsed)
	}

	eng2, err := Open(Spec{TimeoutMS: 10000}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng2.Close()

	shortCtx, shortCancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer shortCancel()

	start = time.Now()
	_, err = eng2.Eval(shortCtx, "loop.js", `while (true) {}`, false)
	elapsed = time.Since(start)
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("short context loop: got %v, want ErrPoisoned", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("short timeout took %v, want <= 2s", elapsed)
	}
}

func TestGuestExceptionSubstringDoesNotPoison(t *testing.T) {
	cases := []string{
		"throw new Error('unreachable')",
		"throw new Error('out of memory')",
		"throw new Error('InternalError')",
		"throw new Error('stack overflow')",
		"throw new Error('context canceled')",
		"throw new Error('module is closed')",
	}

	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			eng, err := Open(Spec{}, nil)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer eng.Close()

			ctx := validContext(t)
			_, err = eng.Eval(ctx, "sub.js", tc, false)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if errors.Is(err, ErrPoisoned) {
				t.Fatalf("guest exception poisoned engine: %v", err)
			}
			se, ok := errors.AsType[*ScriptError](err)
			if !ok {
				t.Fatalf("expected *ScriptError, got %T: %v", err, err)
			}
			if se.Name != "Error" {
				t.Fatalf("se.Name = %q, want Error", se.Name)
			}

			out, err := eng.Eval(ctx, "recover.js", "'ok'", false)
			if err != nil || out != "ok" {
				t.Fatalf("Eval after script error: %q, %v", out, err)
			}
		})
	}

	t.Run("RealInternalErrorPoisons", func(t *testing.T) {
		eng, err := Open(Spec{}, nil)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer eng.Close()

		ctx := validContext(t)
		_, err = eng.Eval(ctx, "internal.js", "throw new InternalError('real fatal')", false)
		if !errors.Is(err, ErrPoisoned) {
			t.Fatalf("InternalError must poison engine: got %v, want ErrPoisoned", err)
		}
		if _, ok := errors.AsType[*Fatal](err); !ok {
			t.Fatalf("expected *Fatal, got %T", err)
		}
	})
}

func TestSnapshotRefusedAfterUserCodeOrCodec(t *testing.T) {
	t.Run("UserCodeRefusesSnapshot", func(t *testing.T) {
		eng, err := Open(Spec{ImagePath: "/tmp/fake-image"}, nil)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer eng.Close()

		ctx := validContext(t)
		_, err = eng.Eval(ctx, "user.js", `globalThis.userSecret=function(){return "user code"}`, false)
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}

		_, err = eng.Snapshot()
		if !errors.Is(err, ErrTainted) {
			t.Fatalf("Snapshot after user code: got %v, want ErrTainted", err)
		}
	})

	t.Run("CodecRefusesSnapshot", func(t *testing.T) {
		spec := Spec{
			ImagePath: "/tmp/fake-image",
			Prelude:   "globalThis.runCodec = () => __flats_docs_codec('test', 'data');",
			RawCodec: func(op string, in Value) (Value, error) {
				return in.NewString("ok"), nil
			},
		}
		eng, err := Open(spec, nil)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer eng.Close()

		ctx := validContext(t)
		_, err = eng.Call(ctx, "runCodec", "")
		if err != nil {
			t.Fatalf("Call: %v", err)
		}

		_, err = eng.Snapshot()
		if !errors.Is(err, ErrTainted) {
			t.Fatalf("Snapshot after codec call: got %v, want ErrTainted", err)
		}
	})
}

func TestOpenRefusesHostPanicOrTaintDuringStartup(t *testing.T) {
	t.Run("HostPanicDuringPrelude", func(t *testing.T) {
		host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
			panic("host exploded")
		}
		spec := Spec{
			Prelude: "try { __flats_host('test', 'arg'); } catch(e) {}",
		}
		eng, err := Open(spec, host)
		if err == nil {
			eng.Close()
			t.Fatal("Open must refuse if host panicked during prelude")
		}
		if !errors.Is(err, ErrTainted) {
			t.Fatalf("Open error: got %v, want ErrTainted", err)
		}
	})

	t.Run("HostTaintDuringPrelude", func(t *testing.T) {
		host := func(ctx context.Context, op string, args []byte) ([]byte, error) {
			return []byte("ok"), nil
		}
		spec := Spec{
			Prelude: "try { __flats_host('test', 'arg'); } catch(e) {}",
		}
		eng, err := Open(spec, host)
		if err == nil {
			eng.Close()
			t.Fatal("Open must refuse if host called during prelude")
		}
		if !errors.Is(err, ErrTainted) {
			t.Fatalf("Open error: got %v, want ErrTainted", err)
		}
	})
}

func TestExpiredContextRejectedBeforeEntry(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err = eng.Call(expiredCtx, "test", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call with expired ctx: got %v, want context.DeadlineExceeded", err)
	}

	_, err = eng.Eval(expiredCtx, "t.js", "1 + 1", false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Eval with expired ctx: got %v, want context.DeadlineExceeded", err)
	}

	validCtx := validContext(t)
	out, err := eng.Eval(validCtx, "t.js", "20 + 22", false)
	if err != nil || out != "42" {
		t.Fatalf("Eval after expired ctx rejection: %q, %v", out, err)
	}
}

func TestCodecHandleLeakTripsLedger(t *testing.T) {
	spec := Spec{
		Prelude: "globalThis.leakCodec = () => __flats_docs_codec('leak', 'data');",
		RawCodec: func(op string, in Value) (Value, error) {
			_ = in.NewString("leaked string handle")
			return in.NewString("ok"), nil
		},
	}
	eng, err := Open(spec, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, err = eng.Call(ctx, "leakCodec", "")
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("Call with leaking codec: got %v, want ErrPoisoned", err)
	}
	f, ok := errors.AsType[*Fatal](err)
	if !ok {
		t.Fatalf("expected *Fatal, got %T: %v", err, err)
	}
	if !strings.Contains(f.Error(), "live handle leak") {
		t.Fatalf("Fatal message: got %q, want 'live handle leak'", f.Error())
	}
}

func TestRawCodecValueOwnershipNoUAF(t *testing.T) {
	var saved Value
	spec := Spec{
		Prelude: "globalThis.saveCodec = () => __flats_docs_codec('save', 'hello world');",
		RawCodec: func(op string, in Value) (Value, error) {
			saved = in
			return in.NewString("ok"), nil
		},
	}
	eng, err := Open(spec, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	out, err := eng.Call(ctx, "saveCodec", "")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out != "ok" {
		t.Fatalf("got %q, want 'ok'", out)
	}

	if saved.String() != "hello world" {
		t.Fatalf("saved.String() = %q, want 'hello world'", saved.String())
	}
	if saved.IsByteArray() {
		t.Fatal("expected saved to not be byte array")
	}
	if !saved.IsString() {
		t.Fatal("expected saved to be string")
	}
}

func TestCallGCForbidden(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, err = eng.Call(ctx, "gc", "")
	if err == nil {
		t.Fatal("Call('gc') must be refused")
	}

	out, err := eng.Eval(ctx, "check.js", "typeof gc", false)
	if err != nil || out != "undefined" {
		t.Fatalf("typeof gc = %q, %v; want 'undefined'", out, err)
	}

	out, err = eng.Eval(ctx, "check2.js", "typeof globalThis.gc", false)
	if err != nil || out != "undefined" {
		t.Fatalf("typeof globalThis.gc = %q, %v; want 'undefined'", out, err)
	}
}

func TestScriptErrorStructuredFields(t *testing.T) {
	eng, err := Open(Spec{}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	ctx := validContext(t)
	_, err = eng.Eval(ctx, "type.js", "throw new TypeError('invalid type argument')", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	se, ok := errors.AsType[*ScriptError](err)
	if !ok {
		t.Fatalf("expected *ScriptError, got %T: %v", err, err)
	}
	if se.Name != "TypeError" {
		t.Fatalf("se.Name = %q, want 'TypeError'", se.Name)
	}
	if se.Message != "invalid type argument" {
		t.Fatalf("se.Message = %q, want 'invalid type argument'", se.Message)
	}
	if se.Stack == "" || !strings.Contains(se.Stack, "type.js") {
		t.Fatalf("se.Stack = %q, want non-empty stack with 'type.js'", se.Stack)
	}
	if !strings.HasPrefix(se.Error(), "TypeError: invalid type argument") {
		t.Fatalf("se.Error() = %q, want prefix 'TypeError: invalid type argument'", se.Error())
	}
}

func TestCollectCyclesFailureSurfacesError(t *testing.T) {
	eng, err := Open(Spec{Prelude: "globalThis.step = () => {};"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	if eng.gc != nil {
		eng.gc.Free()
	}

	ctx := validContext(t)
	var lastErr error
	for range 32 {
		_, lastErr = eng.Call(ctx, "step", "")
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("expected collectCycles failure to surface as error")
	}
	if !errors.Is(lastErr, ErrPoisoned) {
		t.Fatalf("expected ErrPoisoned, got %v", lastErr)
	}
}
