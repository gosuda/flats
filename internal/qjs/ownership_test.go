package qjs

import (
	"fmt"
	"strings"
	"testing"
)

func TestBindingOwnership(t *testing.T) {
	rt, err := New(Option{MemoryLimit: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	text := strings.Repeat("x", 1<<20)
	c.SetFunc("read", func(this *This) (*Value, error) {
		if this.Args()[0].String() != text {
			t.Fatal("callback string")
		}
		return c.NewInt32(1), nil
	})
	for i := 0; i < 200; i++ {
		v := c.NewString(text)
		if v.String() != text {
			t.Fatal("string")
		}
		v.Free()
		b := c.NewArrayBuffer([]byte(text))
		for j := 0; j < 2; j++ {
			if string(b.ToByteArray()) != text {
				t.Fatal("borrowed buffer")
			}
		}
		b.Free()
		obj := c.ParseJSON(`{"x":"` + text + `"}`)
		encoded, err := obj.JSONStringify()
		if err != nil || encoded != `{"x":"`+text+`"}` {
			t.Fatal("JSON", err)
		}
		obj.Free()
		if _, err := safeEval(rt, `read("x".repeat(1048576))`); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}

func TestEvalAndAtomOwnership(t *testing.T) {
	rt, err := New(Option{MemoryLimit: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	source := `({"` + strings.Repeat("k", 1024*1024) + `":1})`
	for i := 0; i < 100; i++ {
		v, err := c.Eval("large.js", Code(source))
		if err != nil {
			t.Fatalf("eval %d: %v", i, err)
		}
		names, err := v.GetOwnPropertyNames()
		if err != nil || len(names) != 1 || len(names[0]) != 1024*1024 {
			t.Fatal("atom", err)
		}
		v.Free()
	}
}

func TestConvertedFunctionOwnership(t *testing.T) {
	rt, err := New(Option{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	object, err := c.Eval("functions.js", Code(`({fn: x => x + 1})`))
	if err != nil {
		t.Fatal(err)
	}
	var converted func(int) int
	for i := 0; i < 100; i++ {
		object.ForEach(func(key, value *Value) {
			converted, err = JsFuncToGo(value, (func(int) int)(nil))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	object.Free()
	if got := converted(41); got != 42 {
		t.Fatalf("converted object function: %d", got)
	}
	if len(c.goFunctions) != 1 {
		t.Fatalf("repeated conversion retains %d functions", len(c.goFunctions))
	}

	var callback func(int) int
	host, err := FuncToJS(c, func(fn func(int) int) { callback = fn })
	if err != nil {
		t.Fatal(err)
	}
	c.Global().SetPropertyStr("saveFunction", host)
	if _, err := safeEval(rt, `saveFunction(x => x * 2)`); err != nil {
		t.Fatal(err)
	}
	if got := callback(21); got != 42 {
		t.Fatalf("converted callback argument: %d", got)
	}

	array, err := c.Eval("array.js", Code(`[x => x + 2]`))
	if err != nil {
		t.Fatal(err)
	}
	functions, err := ToGoValue(array, []func(int) int{})
	if err != nil {
		t.Fatal(err)
	}
	array.Free()
	if got := functions[0](40); got != 42 {
		t.Fatalf("converted array function: %d", got)
	}
}

func TestCapturedIntrinsicsAndPropertyErrors(t *testing.T) {
	rt, err := New(Option{MemoryLimit: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	c := rt.Context()
	if _, err := safeEval(rt, `JSON.stringify = () => "wrong"; Reflect.ownKeys = () => []; Reflect.getOwnPropertyDescriptor = () => undefined;`); err != nil {
		t.Fatal(err)
	}
	object := c.ParseJSON(`{"a":1}`)
	text, err := object.JSONStringify()
	if err != nil || text != `{"a":1}` {
		t.Fatalf("captured stringify: %q, %v", text, err)
	}
	names, err := object.GetOwnPropertyNames()
	if err != nil || len(names) != 1 || names[0] != "a" {
		t.Fatalf("captured enumeration: %v, %v", names, err)
	}
	object.Free()
	primitive := c.NewInt32(1)
	if props := primitive.GetOwnProperties(); len(props) != 0 {
		t.Fatalf("primitive properties: %v", props)
	}
	primitive.Free()
	disappearing, err := c.Eval("proxy.js", Code(`new Proxy({}, { ownKeys() { return ["gone"] }, getOwnPropertyDescriptor() { return undefined } })`))
	if err != nil {
		t.Fatal(err)
	}
	names, err = disappearing.GetOwnPropertyNames()
	disappearing.Free()
	if err != nil || len(names) != 0 {
		t.Fatalf("disappearing property: %v, %v", names, err)
	}
	for i := 0; i < 100; i++ {
		broken, err := c.Eval("broken.js", Code(`new Proxy({}, {ownKeys() {return ["k".repeat(1048576), "bad"]}, getOwnPropertyDescriptor(o, k) {if (k === "bad") throw Error("descriptor failed"); return {value: 1, configurable: true, enumerable: true}}})`))
		if err != nil {
			t.Fatal(err)
		}
		_, err = broken.GetOwnPropertyNames()
		broken.Free()
		if err == nil || !strings.Contains(err.Error(), "descriptor failed") {
			t.Fatalf("iteration %d error: %v", i, err)
		}
	}
	if text, err := safeEval(rt, `"still usable"`); err != nil || text != "still usable" {
		t.Fatalf("pending exception: %q %v", text, err)
	}
}

func TestForEachPanicOwnership(t *testing.T) {
	rt, err := New(Option{MemoryLimit: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	for i := 0; i < 100; i++ {
		object, err := rt.Context().Eval("iteration.js", Code(`({["k".repeat(1048576)]: "v".repeat(1048576), remaining: 1})`))
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		func() {
			defer func() {
				if recover() != "callback panic" {
					t.Fatal("missing callback panic")
				}
			}()
			object.ForEach(func(key, value *Value) { panic("callback panic") })
		}()
		object.Free()
	}
}

func TestForEachGetterExceptionOwnership(t *testing.T) {
	rt, err := New(Option{MemoryLimit: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	for i := 0; i < 100; i++ {
		object, err := rt.Context().Eval("getter.js", Code(`({get ["k".repeat(1048576)]() {throw Error("getter failed")}, remaining: 1})`))
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		func() {
			defer func() {
				if recovered := recover(); recovered == nil || !strings.Contains(fmt.Sprint(recovered), "getter failed") {
					t.Fatalf("getter exception: %v", recovered)
				}
			}()
			object.ForEach(func(key, value *Value) { t.Fatal("throwing getter reached callback") })
		}()
		object.Free()
		if rt.Context().HasException() {
			t.Fatal("getter left a pending exception")
		}
	}
	if got, err := safeEval(rt, `"still usable"`); err != nil || got != "still usable" {
		t.Fatalf("subsequent call: %q, %v", got, err)
	}
}
