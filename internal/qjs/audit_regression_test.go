package qjs

import (
	"strings"
	"testing"
)

// A Go function type without return values must be bindable from JS: the
// Go-to-JS direction explicitly supports it (GoFuncResultToJs), so the
// JS-to-Go direction must call it instead of panicking.
func TestConvertedFuncWithoutReturnValues(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	v, err := c.Eval("fn.js", Code(`(x) => { globalThis.seen = x; }`))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	var converted func(int)
	converted, err = JsFuncToGo(v, converted)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("calling a converted func() panicked: %v", r)
			}
		}()
		converted(41)
	}()
	seen, err := safeEval(rt, `String(globalThis.seen)`)
	if err != nil || seen != "41" {
		t.Fatalf("side effect lost: %q, %v", seen, err)
	}
}

// JS Maps allow any key; a null key must convert to the zero Go key instead
// of panicking in reflection.
func TestMapWithNullKeyToGo(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	v, err := c.Eval("map.js", Code(`new Map([[null, "a"], ["b", 2]])`))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	m, err := ToGoValue[map[any]any](v, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := m[nil]; !ok || got != "a" {
		t.Fatalf("null key entry: %v, %v", got, ok)
	}
	if got, ok := m["b"]; !ok || got != int64(2) {
		t.Fatalf("string key entry: %v, %v", got, ok)
	}
}

// A string whose text merely equals an object string tag is not a Map, Set
// or ArrayBuffer.
func TestStringTagsAreNotMapSetOrArrayBuffer(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	for _, tc := range []struct {
		code string
		hit  func(*Value) bool
	}{
		{`"[object Map]"`, func(v *Value) bool { return v.ToMap() != nil }},
		{`"[object Set]"`, func(v *Value) bool { return v.ToSet() != nil }},
		{`"[object ArrayBuffer]"`, func(v *Value) bool { return v.IsByteArray() }},
	} {
		v, err := c.Eval("tag.js", Code(tc.code))
		if err != nil {
			t.Fatal(err)
		}
		if tc.hit(v) {
			t.Errorf("%s misclassified", tc.code)
		}
		v.Free()
	}
}

// An embedded field hidden with json:"-<options>" must be skipped like every
// other JSON-tag handler in this package does, not flattened into the object.
func TestEmbeddedFieldJSONHiddenWithOptions(t *testing.T) {
	type Inner struct {
		Secret string
	}
	type payload struct {
		*Inner `json:"-,omitempty"`
	}
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	v, err := ToJsValue(c, payload{Inner: &Inner{Secret: "s3cret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	text, err := v.JSONStringify()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "Secret") {
		t.Fatalf("hidden embedded field leaked: %s", text)
	}
}

// A channel send that would block must not poison later sends: after
// draining the channel, a send that fits must succeed.
func TestChannelSendAfterBlockedSend(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	ch := make(chan int, 1)
	obj, err := ToJsValue(c, ch)
	if err != nil {
		t.Fatal(err)
	}
	c.Global().SetPropertyStr("ch", obj)
	if _, err := safeEval(rt, `ch.send(1)`); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if _, err := safeEval(rt, `ch.send(2)`); err == nil {
		t.Fatal("send on a full channel must fail")
	}
	if _, err := safeEval(rt, `ch.receive()`); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, err := safeEval(rt, `ch.send(3)`); err != nil {
		t.Fatalf("send after drain: %v", err)
	}
	if got := <-ch; got != 3 {
		t.Fatalf("received %d, want 3", got)
	}
}

// String to Go int conversions must reject values outside the target range
// like the fixed-width siblings do, not return platform garbage.
func TestStringToIntOverflowErrors(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	v, err := c.Eval("num.js", Code(`"1e300"`))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	i, err := ToGoValue[int](v, 0)
	if err == nil {
		t.Fatalf("1e300 converted silently to int %d", i)
	}
}

// An instanceof check that throws (here a Proxy whose getPrototypeOf trap
// throws) is not an instance, and must not leave its exception pending.
func TestThrowingInstanceOfIsNotAnInstance(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	v, err := c.Eval("proxy.js", Code(`new Proxy({}, { getPrototypeOf() { throw new Error("boom"); } })`))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Free()
	if v.IsMap() || v.IsSet() || v.IsByteArray() {
		t.Fatal("object whose prototype lookup throws was classified as Map, Set or ArrayBuffer")
	}
	if c.HasException() {
		t.Fatalf("classification left a pending exception: %v", c.Exception())
	}
	if got, err := safeEval(rt, `"alive"`); err != nil || got != "alive" {
		t.Fatalf("context poisoned after classification: %q, %v", got, err)
	}
}
