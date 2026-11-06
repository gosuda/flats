package qjs

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

type conversionNode struct {
	Name string
	Next *conversionNode
}

type conversionInner struct {
	Extra string
}

type ConversionPublic struct {
	Visible string
}

type conversionPayload struct {
	Name    string `json:"name"`
	Skipped string `json:"-,omitempty"`
	hidden  string
	conversionInner
	*ConversionPublic
}

type conversionHidden struct {
	Hidden string
}

type conversionHiddenPayload struct {
	*conversionHidden
}

func (p conversionPayload) Hello() string {
	return "hi " + p.Name
}

func conversionRuntime(t *testing.T) *Runtime {
	t.Helper()

	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(rt.Close)

	return rt
}

func conversionEval(t *testing.T, rt *Runtime, code string) *Value {
	t.Helper()

	v, err := rt.Eval("conversion.js", Code(code))
	if err != nil {
		t.Fatalf("eval %s: %v", code, err)
	}

	t.Cleanup(v.Free)

	return v
}

func conversionPanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()

	return nil
}

func conversionAlive(t *testing.T, rt *Runtime) {
	t.Helper()

	got, err := safeEval(rt, `"alive"`)
	if err != nil || got != "alive" {
		t.Fatalf("context unusable after conversion: %q, %v", got, err)
	}
}

func TestNumberToGoRejectsOutOfRangeValues(t *testing.T) {
	rt := conversionRuntime(t)

	rejected := []struct {
		name string
		code string
		run  func(*Value) error
	}{
		{"int/2^63", `2**63`, func(v *Value) error { _, err := ToGoValue[int](v, 0); return err }},
		{"int/1e300", `1e300`, func(v *Value) error { _, err := ToGoValue[int](v, 0); return err }},
		{"int64/2^63", `2**63`, func(v *Value) error { _, err := ToGoValue[int64](v, 0); return err }},
		{"int64/1e300", `1e300`, func(v *Value) error { _, err := ToGoValue[int64](v, 0); return err }},
		{"uint/-1", `-1`, func(v *Value) error { _, err := ToGoValue[uint](v, 0); return err }},
		{"uint/1e300", `1e300`, func(v *Value) error { _, err := ToGoValue[uint](v, 0); return err }},
		{"uint64/2^64", `2**64`, func(v *Value) error { _, err := ToGoValue[uint64](v, 0); return err }},
		{"uint64/-1", `-1`, func(v *Value) error { _, err := ToGoValue[uint64](v, 0); return err }},
		{"uintptr/-1", `-1`, func(v *Value) error { _, err := ToGoValue[uintptr](v, 0); return err }},
		{"uintptr/1e300", `1e300`, func(v *Value) error { _, err := ToGoValue[uintptr](v, 0); return err }},
		{"int8/128", `128`, func(v *Value) error { _, err := ToGoValue[int8](v, 0); return err }},
		{"int8/-129", `-129`, func(v *Value) error { _, err := ToGoValue[int8](v, 0); return err }},
		{"uint8/256", `256`, func(v *Value) error { _, err := ToGoValue[uint8](v, 0); return err }},
		{"uint8/-1", `-1`, func(v *Value) error { _, err := ToGoValue[uint8](v, 0); return err }},
		{"float32/1e300", `1e300`, func(v *Value) error { _, err := ToGoValue[float32](v, 0); return err }},
	}

	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			v := conversionEval(t, rt, tc.code)
			if err := tc.run(v); err == nil {
				t.Fatalf("%s converted without error", tc.code)
			}
		})
	}

	conversionAlive(t, rt)
}

func TestNumberToGoAcceptsInRangeValues(t *testing.T) {
	rt := conversionRuntime(t)

	t.Run("int truncates fractions", func(t *testing.T) {
		v := conversionEval(t, rt, `1.5`)
		got, err := ToGoValue[int](v, 0)
		if err != nil || got != 1 {
			t.Fatalf("1.5 -> %d, %v; want 1", got, err)
		}
	})

	t.Run("int64 min", func(t *testing.T) {
		v := conversionEval(t, rt, `-(2**63)`)
		got, err := ToGoValue[int64](v, 0)
		if err != nil || got != math.MinInt64 {
			t.Fatalf("-(2**63) -> %d, %v; want %d", got, err, int64(math.MinInt64))
		}
	})

	t.Run("uint64 high bit", func(t *testing.T) {
		v := conversionEval(t, rt, `2**63`)
		got, err := ToGoValue[uint64](v, 0)
		if err != nil || got != 1<<63 {
			t.Fatalf("2**63 -> %d, %v; want %d", got, err, uint64(1)<<63)
		}
	})

	t.Run("int8 bounds", func(t *testing.T) {
		for _, tc := range []struct {
			code string
			want int8
		}{{`-128`, -128}, {`127`, 127}, {`-0`, 0}} {
			v := conversionEval(t, rt, tc.code)
			got, err := ToGoValue[int8](v, 0)
			if err != nil || got != tc.want {
				t.Fatalf("%s -> %d, %v; want %d", tc.code, got, err, tc.want)
			}
		}
	})

	t.Run("uint8 bounds", func(t *testing.T) {
		v := conversionEval(t, rt, `255`)
		got, err := ToGoValue[uint8](v, 0)
		if err != nil || got != 255 {
			t.Fatalf("255 -> %d, %v", got, err)
		}
	})

	t.Run("float32", func(t *testing.T) {
		v := conversionEval(t, rt, `1e30`)
		got, err := ToGoValue[float32](v, 0)
		if err != nil || got != 1e30 {
			t.Fatalf("1e30 -> %v, %v", got, err)
		}
	})

	t.Run("string bounds", func(t *testing.T) {
		v := conversionEval(t, rt, `"127"`)
		got, err := ToGoValue[int8](v, 0)
		if err != nil || got != 127 {
			t.Fatalf(`"127" -> %d, %v`, got, err)
		}

		v = conversionEval(t, rt, `"128"`)
		if _, err := ToGoValue[int8](v, 0); err == nil {
			t.Fatal(`"128" converted to int8 without error`)
		}

		v = conversionEval(t, rt, `"255"`)
		if got, err := ToGoValue[uint8](v, 0); err != nil || got != 255 {
			t.Fatalf(`"255" -> %d, %v`, got, err)
		}

		v = conversionEval(t, rt, `"256"`)
		if _, err := ToGoValue[uint8](v, 0); err == nil {
			t.Fatal(`"256" converted to uint8 without error`)
		}
	})
}

func TestInvalidDateToGoErrors(t *testing.T) {
	rt := conversionRuntime(t)

	for _, code := range []string{`new Date(NaN)`, `new Date(8.64e15 + 1)`} {
		v := conversionEval(t, rt, code)
		if _, err := ToGoValue[time.Time](v); err == nil {
			t.Fatalf("%s converted to a time without error", code)
		}
	}

	v := conversionEval(t, rt, `new Date("2020-01-02T03:04:05.678Z")`)
	got, err := ToGoValue[time.Time](v)
	if err != nil {
		t.Fatal(err)
	}

	want := time.Date(2020, 1, 2, 3, 4, 5, 678000000, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("date -> %v; want %v", got.UTC(), want)
	}

	v = conversionEval(t, rt, `new Date(-1)`)
	got, err = ToGoValue[time.Time](v)
	if err != nil {
		t.Fatal(err)
	}

	if want := time.UnixMilli(-1).UTC(); !got.Equal(want) {
		t.Fatalf("date -1ms -> %v; want %v", got.UTC(), want)
	}
}

func TestCyclicGoValueToJsErrors(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	cyclicMap := map[string]any{}
	cyclicMap["self"] = cyclicMap

	cyclicSlice := make([]any, 1)
	cyclicSlice[0] = cyclicSlice

	loop := &conversionNode{Name: "loop"}
	loop.Next = loop

	type holder struct {
		List []any
	}

	held := holder{List: make([]any, 1)}
	held.List[0] = held

	sharedMap := map[string]int{"a": 1}

	for _, tc := range []struct {
		name  string
		value any
	}{
		{"map", cyclicMap},
		{"slice", cyclicSlice},
		{"pointer", loop},
		{"slice through struct", held},
		{"map through slice", []any{sharedMap, sharedMap}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				got *Value
				err error
			)

			if recovered := conversionPanic(func() { got, err = ToJsValue(c, tc.value) }); recovered != nil {
				t.Fatalf("cyclic %s panicked: %v", tc.name, recovered)
			}

			if got != nil {
				got.Free()
			}

			if tc.name == "map through slice" {
				if err != nil {
					t.Fatalf("shared map converted with error: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("cyclic %s converted without error", tc.name)
			}
		})
	}

	conversionAlive(t, rt)
}

func TestSharedGoValuesToJs(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	shared := []int{1, 2}
	empty := []any{}

	v, err := ToJsValue(c, []any{shared, shared, empty, empty})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()

	text, err := v.JSONStringify()
	if err != nil {
		t.Fatal(err)
	}

	if text != "[[1,2],[1,2],[],[]]" {
		t.Fatalf("shared values -> %s", text)
	}
}

func TestCyclicJsValueToGoErrors(t *testing.T) {
	rt := conversionRuntime(t)

	cases := []struct {
		name string
		code string
		run  func(*Value) error
	}{
		{
			"array to slice",
			`(() => { const a = []; a.push(a); return a; })()`,
			func(v *Value) error { _, err := ToGoValue[[]any](v); return err },
		},
		{
			"array to array",
			`(() => { const a = []; a.push(a); return a; })()`,
			func(v *Value) error { _, err := ToGoValue[[1]any](v); return err },
		},
		{
			"nested array object",
			`(() => { const a = []; const o = { a }; a.push(o); return a; })()`,
			func(v *Value) error { _, err := ToGoValue[[]any](v); return err },
		},
		{
			"object to map",
			`(() => { const o = {}; o.self = o; return o; })()`,
			func(v *Value) error { _, err := ToGoValue[map[string]any](v); return err },
		},
		{
			"object to struct",
			`(() => { const o = { Name: "x" }; o.Next = o; return o; })()`,
			func(v *Value) error { _, err := ToGoValue[conversionNode](v); return err },
		},
		{
			"set to slice",
			`(() => { const s = new Set(); s.add(s); return s; })()`,
			func(v *Value) error { _, err := ToGoValue[[]any](v); return err },
		},
		{
			"map to map",
			`(() => { const m = new Map(); m.set("self", m); return m; })()`,
			func(v *Value) error { _, err := ToGoValue[map[string]any](v); return err },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := conversionEval(t, rt, tc.code)

			var err error
			if recovered := conversionPanic(func() { err = tc.run(v) }); recovered != nil {
				t.Fatalf("cyclic %s panicked: %v", tc.name, recovered)
			}

			if err == nil {
				t.Fatalf("cyclic %s converted without error", tc.name)
			}

			if strings.Contains(err.Error(), "stack overflow") {
				t.Fatalf("cyclic %s recursed until stack exhaustion: %v", tc.name, err)
			}
		})
	}

	conversionAlive(t, rt)
}

func TestSharedJsValuesToGo(t *testing.T) {
	rt := conversionRuntime(t)

	v := conversionEval(t, rt, `(() => { const a = [1]; return [a, a]; })()`)
	got, err := ToGoValue[[]any](v)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("shared arrays -> %v", got)
	}

	first, isSlice := got[0].([]any)
	if !isSlice || len(first) != 1 || first[0] != int64(1) {
		t.Fatalf("shared arrays -> %v", got)
	}
}

func TestGoMapToJsRejectsKeyCollisions(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	if v, err := ToJsValue(c, map[any]any{1: "a", "1": "b"}); err == nil {
		v.Free()
		t.Fatal("colliding keys converted without error")
	}

	v, err := ToJsValue(c, map[any]any{1: "a", "b": "c"})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()

	one := v.GetPropertyStr("1")
	defer one.Free()
	if one.String() != "a" {
		t.Fatalf("key 1 -> %q", one.String())
	}

	b := v.GetPropertyStr("b")
	defer b.Free()
	if b.String() != "c" {
		t.Fatalf("key b -> %q", b.String())
	}
}
func TestGoMapToJsRejectsNULKey(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	for _, m := range []any{
		map[string]int{"hello\x00world": 1},
		map[any]int{"a\x00b": 2},
	} {
		v, err := ToJsValue(c, m)
		if v != nil {
			v.Free()
		}
		if err == nil || !errors.Is(err, ErrStringContainsNUL) {
			t.Fatalf("ToJsValue with NUL key: got (%v, %v), want error wrapping ErrStringContainsNUL", v, err)
		}
	}
}

func TestConvertedJsFuncReportsErrors(t *testing.T) {
	rt := conversionRuntime(t)

	throw := conversionEval(t, rt, `() => { throw new Error("boom"); }`)
	withError, err := JsFuncToGo(throw, func() (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}

	if _, callErr := withError(); callErr == nil || !strings.Contains(callErr.Error(), "boom") {
		t.Fatalf("throwing JS function -> %v; want boom", callErr)
	}

	okValue := conversionEval(t, rt, `() => 42`)
	withError, err = JsFuncToGo(okValue, func() (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}

	if got, callErr := withError(); callErr != nil || got != 42 {
		t.Fatalf("JS function -> %d, %v; want 42", got, callErr)
	}

	wrongType := conversionEval(t, rt, `() => ({})`)
	withError, err = JsFuncToGo(wrongType, func() (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}

	if _, callErr := withError(); callErr == nil {
		t.Fatal("object result converted to int without error")
	}

	noErrorResult := conversionEval(t, rt, `() => { throw new Error("boom"); }`)
	plain, err := JsFuncToGo(noErrorResult, func() int { return 0 })
	if err != nil {
		t.Fatal(err)
	}

	recovered := conversionPanic(func() { _ = plain() })

	panicErr, isError := recovered.(error)
	if !isError || !strings.Contains(panicErr.Error(), "no error result") {
		t.Fatalf("signature without error result reported %v", recovered)
	}

	conversionAlive(t, rt)
}

func TestConvertedJsFuncCallBoundaries(t *testing.T) {
	rt := conversionRuntime(t)

	double := conversionEval(t, rt, `(x) => x * 2`)
	fn, err := JsFuncToGo(double, func(int) int { return 0 })
	if err != nil {
		t.Fatal(err)
	}

	if got := fn(21); got != 42 {
		t.Fatalf("21 -> %d; want 42", got)
	}

	fewArgs := conversionEval(t, rt, `(a) => String(a)`)
	few, err := JsFuncToGo(fewArgs, func(a, b int) string { return "" })
	if err != nil {
		t.Fatal(err)
	}

	if got := few(5, 6); got != "5" {
		t.Fatalf("missing JS args -> %q; want 5", got)
	}

	nullPointer := conversionEval(t, rt, `(p) => String(p)`)
	ptr, err := JsFuncToGo(nullPointer, func(*int) string { return "" })
	if err != nil {
		t.Fatal(err)
	}

	if got := ptr(nil); got != "null" {
		t.Fatalf("nil pointer -> %q; want null", got)
	}

	value := 3
	if got := ptr(&value); got != "3" {
		t.Fatalf("pointer -> %q; want 3", got)
	}

	variadicJs := conversionEval(t, rt, `(...xs) => xs.join(",")`)
	variadic, err := JsFuncToGo(variadicJs, func(...int) string { return "" })
	if err != nil {
		t.Fatal(err)
	}

	if got := variadic(1, 2, 3); got != "1,2,3" {
		t.Fatalf("variadic -> %q", got)
	}

	identity := conversionEval(t, rt, `(v) => 1`)
	withError, err := JsFuncToGo(identity, func(any) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}

	if _, callErr := withError(unsafe.Pointer(nil)); callErr == nil {
		t.Fatal("unconvertible argument reported no error")
	}

	conversionAlive(t, rt)
}

func TestFuncToJsRejectsUnsupportedSignatures(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	if v, err := FuncToJS(c, nil); err != nil || !v.IsNull() {
		t.Fatalf("nil -> %v, %v; want null", v, err)
	}

	var nilFn func(int)
	if v, err := FuncToJS(c, nilFn); err != nil || !v.IsNull() {
		t.Fatalf("nil func -> %v, %v; want null", v, err)
	}

	if _, err := FuncToJS(c, 42); err == nil {
		t.Fatal("non-function converted without error")
	}

	if _, err := FuncToJS(c, func(unsafe.Pointer) {}); err == nil {
		t.Fatal("unsafe.Pointer parameter converted without error")
	}

	inc := func(x int) int { return x + 1 }
	v, err := FuncToJS(c, &inc)
	if err != nil {
		t.Fatal(err)
	}

	c.Global().SetPropertyStr("inc", v)

	if got, err := safeEval(rt, `inc(1)`); err != nil || got != "2" {
		t.Fatalf("pointer to function -> %q, %v", got, err)
	}
}

func TestChannelConversionSignalsErrors(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	ch := make(chan int, 1)
	obj, err := ToJsValue(c, ch)
	if err != nil {
		t.Fatal(err)
	}

	c.Global().SetPropertyStr("ch", obj)

	steps := []struct {
		code string
		want string
	}{
		{`(() => { try { ch.receive(); return "no error"; } catch (e) { return String(e); } })()`, "would block"},
		{`(() => { try { ch.send(7); return "sent"; } catch (e) { return String(e); } })()`, "sent"},
		{`String(ch.length())`, "1"},
		{`(() => { try { ch.send(8); return "no error"; } catch (e) { return String(e); } })()`, "would block"},
		{`(() => String(ch.receive()))()`, "7"},
		{`String(ch.close())`, "undefined"},
		{`(() => { try { ch.send(1); return "no error"; } catch (e) { return String(e); } })()`, "send on closed channel"},
		{`(() => { try { ch.receive(); return "no error"; } catch (e) { return String(e); } })()`, "channel is closed"},
		{`(() => { try { ch.close(); return "no error"; } catch (e) { return String(e); } })()`, "close of closed channel"},
		{`String(ch.capacity())`, "1"},
	}

	for _, step := range steps {
		got, err := safeEval(rt, step.code)
		if err != nil {
			t.Fatalf("%s: %v", step.code, err)
		}

		if !strings.Contains(got, step.want) {
			t.Fatalf("%s -> %q; want %q", step.code, got, step.want)
		}
	}

	recvOnly := make(chan int, 1)
	recvValue, err := ToJsValue(c, (<-chan int)(recvOnly))
	if err != nil {
		t.Fatal(err)
	}

	c.Global().SetPropertyStr("ro", recvValue)

	for _, step := range []struct {
		code string
		want string
	}{
		{`typeof ro.send`, "undefined"},
		{`(() => { try { ro.close(); return "no error"; } catch (e) { return String(e); } })()`, "cannot close receive-only channel"},
	} {
		got, err := safeEval(rt, step.code)
		if err != nil {
			t.Fatalf("%s: %v", step.code, err)
		}

		if !strings.Contains(got, step.want) {
			t.Fatalf("%s -> %q; want %q", step.code, got, step.want)
		}
	}

	sendOnlyValue, err := ToJsValue(c, (chan<- int)(make(chan int, 1)))
	if err != nil {
		t.Fatal(err)
	}

	c.Global().SetPropertyStr("so", sendOnlyValue)

	if got, err := safeEval(rt, `typeof so.receive`); err != nil || got != "undefined" {
		t.Fatalf("send-only receive -> %q, %v", got, err)
	}

	conversionAlive(t, rt)
}

func TestGoCallbackPanicIsCatchable(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	jsFn, err := FuncToJS(c, func(int) int { panic("go boom") })
	if err != nil {
		t.Fatal(err)
	}

	c.Global().SetPropertyStr("boom", jsFn)

	got, err := safeEval(rt, `(() => { try { boom(1); return "no error"; } catch (e) { return String(e); } })()`)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, "go boom") {
		t.Fatalf("Go callback panic -> %q", got)
	}

	conversionAlive(t, rt)
}

func TestProxyRegistryLifecycle(t *testing.T) {
	registry := NewProxyRegistry()

	if id := registry.Register(nil); id != 0 {
		t.Fatalf("Register(nil) -> %d", id)
	}

	first := registry.Register("one")
	second := registry.Register("two")

	if first == 0 || second <= first {
		t.Fatalf("ids %d, %d are not monotonic", first, second)
	}

	if _, ok := registry.Get(0); ok {
		t.Fatal("Get(0) found a value")
	}

	if _, ok := registry.Get(second + 100); ok {
		t.Fatal("unknown id found a value")
	}

	if registry.Unregister(0) {
		t.Fatal("Unregister(0) reported removal")
	}

	if !registry.Unregister(first) {
		t.Fatal("Unregister(first) reported nothing")
	}

	if registry.Unregister(first) {
		t.Fatal("second Unregister(first) reported removal")
	}

	if registry.Len() != 1 {
		t.Fatalf("Len -> %d; want 1", registry.Len())
	}

	registry.Clear()

	if registry.Len() != 0 {
		t.Fatalf("Len after Clear -> %d", registry.Len())
	}

	if id := registry.Register("three"); id <= second {
		t.Fatalf("id %d reused after Clear", id)
	}
}

func TestProxyRegistryConcurrentUse(t *testing.T) {
	registry := NewProxyRegistry()

	const workers = 16

	ids := make([]uint64, workers)

	var wg sync.WaitGroup

	for i := range workers {
		wg.Go(func() {
			ids[i] = registry.Register(i)

			got, ok := registry.Get(ids[i])
			if !ok || got != i {
				t.Errorf("Get(%d) -> %v, %v", ids[i], got, ok)
			}

			registry.Len()
		})
	}

	wg.Wait()

	seen := make(map[uint64]bool, workers)
	for _, id := range ids {
		if id == 0 || seen[id] {
			t.Fatalf("duplicate or zero id %d", id)
		}

		seen[id] = true
	}

	for _, id := range ids {
		if !registry.Unregister(id) {
			t.Fatalf("Unregister(%d) reported nothing", id)
		}
	}

	if registry.Len() != 0 {
		t.Fatalf("Len -> %d after removals", registry.Len())
	}
}

func TestGoStringToJsRoundTrip(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	for _, s := range []string{"", "plain", "a\x00b", "\x00", "日本語", "😀", strings.Repeat("é", 5000)} {
		v, err := ToJsValue(c, s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}

		back, convErr := ToGoValue[string](v)
		v.Free()

		if convErr != nil || back != s {
			t.Fatalf("%q round trip -> %q, %v", s, back, convErr)
		}
	}
}

func TestGoBytesToJsCopies(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	buf := []byte{1, 2, 3}

	v, err := ToJsValue(c, buf)
	if err != nil {
		t.Fatal(err)
	}

	buf[0] = 9

	c.Global().SetPropertyStr("ab", v)

	got, err := safeEval(rt, `new Uint8Array(ab)[0]`)
	if err != nil {
		t.Fatal(err)
	}

	if got != "1" {
		t.Fatalf("JS buffer aliases the Go slice: first byte %q", got)
	}
}

func TestTypedArrayOffsetsToGo(t *testing.T) {
	rt := conversionRuntime(t)

	sub := conversionEval(t, rt, `(() => { const b = new Uint8Array([9, 8, 7, 6]); return b.subarray(1, 3); })()`)
	got, err := ToGoValue[[]byte](sub)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != string([]byte{8, 7}) {
		t.Fatalf("subarray -> %v; want [8 7]", got)
	}

	view := conversionEval(t, rt, `new DataView(new Uint8Array([1, 2, 3, 4]).buffer, 1, 2)`)
	got, err = ToGoValue[[]byte](view)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != string([]byte{2, 3}) {
		t.Fatalf("DataView -> %v; want [2 3]", got)
	}
}

func TestTypedArrayProxyBoundsToGo(t *testing.T) {
	rt := conversionRuntime(t)

	code := `new Proxy(new Uint8Array([1, 2, 3, 4]), {
		get(target, property) {
			if (property === "byteOffset") { return -1; }
			if (property === "byteLength") { return 2; }
			return target[property];
		},
	})`

	v := conversionEval(t, rt, code)

	var err error

	recovered := conversionPanic(func() { _, err = JsTypedArrayToGo(v) })

	if recovered != nil {
		t.Fatalf("lying byteOffset panicked: %v", recovered)
	}

	if err == nil {
		t.Fatal("lying byteOffset converted without error")
	}

	conversionAlive(t, rt)
}

func TestStructConversionBothWays(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	v, err := ToJsValue(c, conversionPayload{
		Name:             "n",
		Skipped:          "s",
		hidden:           "h",
		conversionInner:  conversionInner{Extra: "e"},
		ConversionPublic: &ConversionPublic{Visible: "v"},
	})
	if err != nil {
		t.Fatal(err)
	}

	c.Global().SetPropertyStr("payload", v)

	for _, step := range []struct {
		code string
		want string
	}{
		{`payload.name`, "n"},
		{`payload.Extra`, "e"},
		{`payload.Visible`, "v"},
		{`typeof payload.Hello`, "function"},
		{`payload.Hello()`, "hi n"},
		{`"hidden" in payload`, "false"},
		{`"Skipped" in payload`, "false"},
	} {
		got, err := safeEval(rt, step.code)
		if err != nil {
			t.Fatalf("%s: %v", step.code, err)
		}

		if got != step.want {
			t.Fatalf("%s -> %q; want %q", step.code, got, step.want)
		}
	}

	obj := conversionEval(t, rt, `({ name: "n", Extra: "e", Visible: "v" })`)
	back, err := ToGoValue[conversionPayload](obj)
	if err != nil {
		t.Fatal(err)
	}

	if back.Name != "n" || back.Extra != "e" {
		t.Fatalf("struct from object -> %+v", back)
	}

	if back.ConversionPublic == nil || back.ConversionPublic.Visible != "v" {
		t.Fatalf("embedded pointer from object -> %+v", back.ConversionPublic)
	}

	nilInner, err := ToJsValue(c, conversionPayload{Name: "solo"})
	if err != nil {
		t.Fatal(err)
	}

	c.Global().SetPropertyStr("solo", nilInner)

	if got, err := safeEval(rt, `"Visible" in solo`); err != nil || got != "false" {
		t.Fatalf("nil embedded pointer -> %q, %v", got, err)
	}
}

func TestStructConversionUnsettableEmbeddedPointerErrors(t *testing.T) {
	rt := conversionRuntime(t)

	v := conversionEval(t, rt, `({ Hidden: "h" })`)

	var err error

	recovered := conversionPanic(func() {
		_, err = ToGoValue[conversionHiddenPayload](v)
	})

	if recovered != nil {
		t.Fatalf("unsettable embedded pointer panicked: %v", recovered)
	}

	if err == nil || !strings.Contains(err.Error(), "cannot allocate embedded pointer") {
		t.Fatalf("unsettable embedded pointer -> %v", err)
	}

	conversionAlive(t, rt)
}

func TestRegistryRoundTripIdentity(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	original := &conversionPayload{Name: "id"}

	v, err := ToJsValue(c, original)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()

	back, err := ToGoValue[*conversionPayload](v)
	if err != nil {
		t.Fatal(err)
	}

	if back != original {
		t.Fatal("registry identity lost")
	}

	if _, err := ToGoValue[string](v); err == nil {
		t.Fatal("mismatched registry type converted")
	}
}

func TestNestedPointersToJs(t *testing.T) {
	rt := conversionRuntime(t)
	c := rt.Context()

	type leaf struct {
		Value int
	}

	type middle struct {
		Leaf *leaf
	}

	type root struct {
		Middle *middle
	}

	value := root{Middle: &middle{Leaf: &leaf{Value: 7}}}

	v, err := GoStructToJs(c, reflect.TypeOf(value), reflect.ValueOf(value))
	if err != nil {
		t.Fatalf("nested pointers: %v", err)
	}
	defer v.Free()

	text, err := v.JSONStringify()
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(text, `"Value":7`) {
		t.Fatalf("nested pointers -> %s", text)
	}
}
