package jsengine

import (
	"bytes"
	"context"
	"errors"
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
	eng, err := Open(Spec{Prelude: "globalThis.gc = () => {};"}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer eng.Close()

	if eng.GCDrained() {
		t.Fatal("expected GCDrained false before collections")
	}

	for range 32 {
		_, err = eng.Call(ctx, "gc", "")
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
