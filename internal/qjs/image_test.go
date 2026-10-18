package qjs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests for runtime images and shells (NOTICE items 13-15).

func mustEval(t *testing.T, rt *Runtime, code string) string {
	t.Helper()
	s, err := safeEval(rt, code)
	if err != nil {
		t.Fatalf("%s: %v", code, err)
	}
	return s
}

// templateRuntime builds a runtime with a Go function and some JS state.
func templateRuntime(t *testing.T, opt Option, fn Function) *Runtime {
	t.Helper()
	rt, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	rt.Context().SetFunc("hostFn", fn)
	mustEval(t, rt, `globalThis.state = {n: 41, list: [1, 2, 3]}; globalThis.add = (a, b) => a + b; "ok"`)
	return rt
}

func restore(t *testing.T, img *Image, opt Option, fns ...Function) *Runtime {
	t.Helper()
	opt.Image, opt.ImageFunctions = img, fns
	rt, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	return rt
}

func textFn(s string) Function {
	return func(this *This) (*Value, error) { return this.Context().NewString(s), nil }
}

func TestImageRoundTrip(t *testing.T) {
	rt := templateRuntime(t, Option{}, textFn("template"))
	keep := rt.Context().Global().GetPropertyStr("add")
	img, err := rt.Image(map[string]uint64{"add": keep.Raw()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := img.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	img2, err := UnmarshalImage(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := img2.MarshalBinary()
	if !bytes.Equal(b, b2) {
		t.Fatal("unmarshal/marshal is not stable")
	}
	t.Logf("image: %d KiB memory, %d KiB serialized", img.MemorySize()>>10, len(b)>>10)
	for i, im := range []*Image{img, img2} {
		r := restore(t, im, Option{}, textFn("restored"))
		if !r.Restored() {
			t.Fatal("not marked restored")
		}
		if got := mustEval(t, r, `String(state.n + 1) + "," + state.list.join("") + "," + add(2, 3)`); got != "42,123,5" {
			t.Fatalf("%d: state = %q", i, got)
		}
		if got := mustEval(t, r, `hostFn()`); got != "restored" {
			t.Fatalf("%d: Go function = %q", i, got)
		}
		v, ok := r.ImageValue("add")
		if !ok {
			t.Fatal("no extra value")
		}
		res, err := r.Context().Invoke(v, r.Context().Global(), r.Context().NewInt32(20), r.Context().NewInt32(22))
		if err != nil || res.Int32() != 42 {
			t.Fatalf("extra value call = %v, %v", res, err)
		}
		res.Free()
		v.Free()
		// The restored runtime keeps working: new functions, GC, more memory.
		r.Context().SetFunc("later", textFn("later"))
		if got := mustEval(t, r, `gc(); let a = []; for (let i = 0; i < 200000; i++) a.push({i}); later() + a.length`); got != "later200000" {
			t.Fatalf("%d: after restore = %q", i, got)
		}
		if !r.StackIntact() {
			t.Fatal("stack canary")
		}
	}
	keep.Free()
	// The template keeps working after capture.
	if got := mustEval(t, rt, `hostFn() + state.n`); got != "template41" {
		t.Fatalf("template = %q", got)
	}
}

func TestImageKeyAndCorruption(t *testing.T) {
	opt := Option{MemoryLimit: 32 << 20, ImageBinding: "binding-a"}
	rt := templateRuntime(t, opt, textFn("x"))
	img, err := rt.Image(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Option{
		"binding":      {MemoryLimit: 32 << 20, ImageBinding: "binding-b"},
		"memory limit": {MemoryLimit: 16 << 20, ImageBinding: "binding-a"},
		"pages":        {MemoryLimit: 32 << 20, ImageBinding: "binding-a", MemoryLimitPages: 512},
		"stack":        {MemoryLimit: 32 << 20, ImageBinding: "binding-a", StackSize: 2 << 20},
		"gc":           {MemoryLimit: 32 << 20, ImageBinding: "binding-a", GCThreshold: 1 << 20},
		"mount":        {MemoryLimit: 32 << 20, ImageBinding: "binding-a", ReadOnlyDir: t.TempDir()},
	} {
		o.Image, o.ImageFunctions = img, []Function{textFn("y")}
		if _, err := New(o); !errors.Is(err, ErrImageKey) {
			t.Errorf("%s: New = %v, want ErrImageKey", name, err)
		}
	}
	// Functions must match the image.
	if _, err := New(Option{MemoryLimit: 32 << 20, ImageBinding: "binding-a", Image: img}); err == nil {
		t.Error("restored without its functions")
	}
	b, _ := img.MarshalBinary()
	for _, i := range []int{0, 9, 20, 60, len(b) / 2, len(b) - 70000, len(b) - 1} {
		c := bytes.Clone(b)
		c[i] ^= 0x40
		if _, err := UnmarshalImage(c); !errors.Is(err, ErrImageCorrupt) {
			t.Errorf("flipped byte %d: %v", i, err)
		}
	}
	for _, n := range []int{0, 7, 100, len(b) - 4, len(b) - 1} {
		if _, err := UnmarshalImage(b[:n]); !errors.Is(err, ErrImageCorrupt) {
			t.Errorf("truncated to %d: %v", n, err)
		}
	}
	if _, err := UnmarshalImage(append(bytes.Clone(b), 0)); !errors.Is(err, ErrImageCorrupt) {
		t.Errorf("trailing byte: %v", err)
	}
}

func TestImageRandomStateDiscovery(t *testing.T) {
	for i := 0; i < 5; i++ {
		rt, err := New(Option{})
		if err != nil {
			t.Fatal(err)
		}
		if rt.randomErr != nil || rt.randomAddr == 0 {
			t.Fatalf("random state not found: %v", rt.randomErr)
		}
		ctx := uint32(rt.context.handle.raw)
		if rt.randomAddr < ctx || rt.randomAddr >= ctx+imageScan {
			t.Fatalf("slot %d outside the context at %d", rt.randomAddr, ctx)
		}
		// The slot is Math.random's state: writing it fixes the sequence.
		seq := func() string {
			rt.module.Memory().WriteUint64Le(rt.randomAddr, 0x1234567)
			return mustEval(t, rt, `String(Math.random()) + String(Math.random())`)
		}
		if a, b := seq(), seq(); a != b {
			t.Fatalf("writing the slot did not reset Math.random: %s %s", a, b)
		}
		if rt.ImageEligible() == nil {
			t.Fatal("eligible after Math.random")
		}
		rt.Close()
	}
}

func TestImageIndependentRandom(t *testing.T) {
	rt := templateRuntime(t, Option{}, textFn("x"))
	img, err := rt.Image(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := img.MarshalBinary()
	seen := map[string]bool{mustEval(t, rt, `String(Math.random())`): true}
	for i := 0; i < 4; i++ {
		im := img
		if i >= 2 { // separate restores from one serialized file
			if im, err = UnmarshalImage(b); err != nil {
				t.Fatal(err)
			}
		}
		r := restore(t, im, Option{}, textFn("x"))
		s := mustEval(t, r, `String(Math.random())`)
		if seen[s] {
			t.Fatalf("restored runtime %d repeats Math.random %s", i, s)
		}
		seen[s] = true
	}
}

func TestImageMonotonicContinuity(t *testing.T) {
	before := float64(hostNanotime()) / 1e6
	rt := templateRuntime(t, Option{}, textFn("x"))
	after := float64(hostNanotime()) / 1e6
	// A fresh runtime reads the host monotonic clock with offset 0.
	origin, _ := strconv.ParseFloat(mustEval(t, rt, `String(performance.timeOrigin)`), 64)
	if origin < before || origin > after {
		t.Fatalf("timeOrigin %f outside [%f, %f]", origin, before, after)
	}
	if rt.clock.offset.Load() != 0 {
		t.Fatal("fresh runtime has a clock offset")
	}
	t0, _ := strconv.ParseFloat(mustEval(t, rt, `String(performance.now())`), 64)
	img, err := rt.Image(nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	r := restore(t, img, Option{}, textFn("x"))
	vals := strings.Split(mustEval(t, r, `[performance.now(), performance.now(), performance.timeOrigin].join(" ")`), " ")
	n1, _ := strconv.ParseFloat(vals[0], 64)
	n2, _ := strconv.ParseFloat(vals[1], 64)
	o, _ := strconv.ParseFloat(vals[2], 64)
	if o != origin {
		t.Fatalf("restored timeOrigin %f, want the template's %f", o, origin)
	}
	if n1 < t0 || n2 < n1 {
		t.Fatalf("clock went backwards: %f then %f %f", t0, n1, n2)
	}
	// Continued from the capture, not from wall time since then.
	if n1-t0 > 250 {
		t.Fatalf("restored clock jumped by %f ms", n1-t0)
	}
}

func TestImageEligibility(t *testing.T) {
	cases := map[string]func(rt *Runtime){
		"host op": func(rt *Runtime) { rt.Taint("host operation") },
		"output":  func(rt *Runtime) { mustEval(t, rt, `print("hello"); "ok"`) },
		"random":  func(rt *Runtime) { mustEval(t, rt, `String(Math.random())`) },
		"go function": func(rt *Runtime) {
			v, err := rt.Eval("t.js", Code(`(() => 1)`))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Free()
			if _, err := JsFuncToGo[func() int](v); err != nil {
				t.Fatal(err)
			}
		},
		"registry gap":   func(rt *Runtime) { rt.registry.Unregister(1) },
		"registry value": func(rt *Runtime) { rt.Context().NewProxyValue(struct{}{}).Free() },
		"stack": func(rt *Runtime) {
			rt.module.Memory().Write(rt.stackLow, []byte{0})
		},
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			rt := templateRuntime(t, Option{Stdout: &bytes.Buffer{}}, textFn("x"))
			if err := rt.ImageEligible(); err != nil {
				t.Fatalf("eligible before: %v", err)
			}
			spoil(rt)
			if _, err := rt.Image(nil); err == nil {
				t.Fatal("captured")
			} else {
				t.Log(err)
			}
		})
	}
	// Clock reads and Go function calls are allowed.
	rt := templateRuntime(t, Option{}, textFn("x"))
	mustEval(t, rt, `Date.now() + performance.now() + hostFn()`)
	if _, err := rt.Image(nil); err != nil {
		t.Fatal(err)
	}
	// A restored runtime is not captured again.
	r := restore(t, mustImage(t, rt), Option{}, textFn("x"))
	if _, err := r.Image(nil); err == nil {
		t.Fatal("captured a restored runtime")
	}
}

func mustImage(t *testing.T, rt *Runtime) *Image {
	t.Helper()
	img, err := rt.Image(nil)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestImageWithModules(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "lib.js"), []byte(`export const x = 41; export const late = () => import("/late.js");`), 0o644)
	os.WriteFile(filepath.Join(dir, "late.js"), []byte(`export default "late";`), 0o644)
	census := &Census{}
	rt, err := New(Option{ReadOnlyDir: dir, Census: census})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	v, err := rt.Eval("boot.js", Code(`import {x, late} from "/lib.js"; globalThis.x = x; globalThis.late = late;`), TypeModule())
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		v.Free()
	}
	if o, c := census.FileOpens.Load(), census.FileCloses.Load(); o == 0 || o != c {
		t.Fatalf("file opens %d closes %d", o, c)
	}
	img := mustImage(t, rt)
	r := restore(t, img, Option{ReadOnlyDir: dir})
	if got := mustEval(t, r, `String(x + 1)`); got != "42" {
		t.Fatalf("x = %q", got)
	}
	// The module loader still works after restore.
	v, err = r.Eval("t.js", Code(`late().then(m => globalThis.got = m.default)`))
	if err != nil {
		t.Fatal(err)
	}
	if v.IsPromise() {
		if v2, err := v.Await(); err == nil {
			v2.Free()
		}
	}
	v.Free()
	if got := mustEval(t, r, `String(globalThis.got)`); got != "late" {
		t.Fatalf("dynamic import after restore = %q", got)
	}
}

func TestShell(t *testing.T) {
	opt := Option{CloseOnContextDone: true, CacheDir: t.TempDir()}
	s, err := NewShell(opt)
	if err != nil {
		t.Fatal(err)
	}
	if s.Compatible(Option{CacheDir: opt.CacheDir}) || s.Compatible(Option{CloseOnContextDone: true}) || s.Compatible(Option{CloseOnContextDone: true, CacheDir: opt.CacheDir, MemoryLimitPages: 10}) {
		t.Fatal("incompatible options accepted")
	}
	if !s.Compatible(Option{CloseOnContextDone: true, CacheDir: opt.CacheDir, MemoryLimitPages: DefaultMemoryLimitPages, MemoryLimit: 1 << 20}) {
		t.Fatal("compatible options refused")
	}
	if _, err := New(Option{Shell: s}); err == nil {
		t.Fatal("New took an incompatible shell")
	}
	o := opt
	o.Shell = s
	rt, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rt.Context().SetFunc("f", textFn("shell"))
	if got := mustEval(t, rt, `f() + (1 + 1)`); got != "shell2" {
		t.Fatalf("eval = %q", got)
	}
	if _, err := New(o); err == nil {
		t.Fatal("shell used twice")
	}
	s.Close() // no effect after use
	if got := mustEval(t, rt, `f()`); got != "shell" {
		t.Fatalf("after Close = %q", got)
	}
	// A shell can also restore an image.
	img := mustImage(t, rt)
	s2, err := NewShell(opt)
	if err != nil {
		t.Fatal(err)
	}
	o.Shell, o.Image, o.ImageFunctions = s2, img, []Function{textFn("again")}
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := mustEval(t, r, `f()`); got != "again" {
		t.Fatalf("restored through a shell = %q", got)
	}
	// An unused shell can be closed.
	s3, err := NewShell(opt)
	if err != nil {
		t.Fatal(err)
	}
	s3.Close()
	o.Shell, o.Image, o.ImageFunctions = s3, nil, nil
	if _, err := New(o); err == nil {
		t.Fatal("closed shell used")
	}
}
