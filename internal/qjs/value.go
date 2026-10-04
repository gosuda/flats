package qjs

import (
	"errors"
	"fmt"
	"math/big"
	"time"
	"unsafe"
)

type (
	Function      func(ctx *This) (*Value, error)
	AsyncFunction func(ctx *This)
	JSAtom        uint32
)

type Value struct {
	handle  *Handle
	context *Context
}

type This struct {
	*Value

	context *Context
	args    []*Value
	promise *Value
	isAsync bool
}

type JSPropertyEnum struct {
	isEnumerable bool
	atom         JSAtom
}

const jsPropertyEnumSize = uint32(unsafe.Sizeof(JSPropertyEnum{}))

// Atom represents a JavaScript atom:
// Object property names and some strings are stored as Atoms (unique strings) to save memory and allow fast comparison.
type Atom struct {
	*Value

	context *Context
}

type OwnProperty struct {
	isEnumerable bool
	atom         Atom
}

func (p OwnProperty) String() string {
	return p.atom.String()
}

func (t *This) Context() *Context {
	return t.context
}

func (t *This) Args() []*Value {
	return t.args
}

func (t *This) Promise() *Value {
	return t.promise
}

func (t *This) IsAsync() bool {
	return t.isAsync
}

func (a Atom) Free() {
	a.context.Call("JS_FreeAtom", a.context.Raw(), a.Raw())
}

func (a Atom) String() string {
	result := a.context.Call("QJS_AtomToCString", a.context.Raw(), a.Raw())
	return a.context.readCString(result.handle)
}

func (a Atom) ToValue() *Value {
	return a.context.Call("JS_AtomToValue", a.context.Raw(), a.Raw())
}

func (v *Value) Handle() *Handle {
	return v.handle
}

func (v *Value) Raw() uint64 {
	if v == nil || v.handle == nil {
		return 0
	}

	return v.handle.raw
}

func (v *Value) Free() {
	if v != nil && v.Raw() != 0 {
		v.context.FreeJsValue(v.handle.raw)
		v.handle.raw = 0
	}
}

func (v *Value) Context() *Context {
	return v.context
}

func (v *Value) Ctx() uint64 {
	return v.context.Raw()
}

func (v *Value) Call(name string, args ...uint64) *Value {
	return v.context.Call(name, args...)
}

func (v *Value) Clone() *Value {
	return v.Call("QJS_CloneValue", v.Ctx(), v.Raw())
}

func (v *Value) Type() string {
	// Check Symbol first, as it is a special case
	if v.IsSymbol() {
		return "Symbol"
	}

	if v.IsQJSProxyValue() {
		return "QJSProxyValue"
	}

	if v.IsNaN() {
		return "NaN"
	}

	if v.IsInfinity() {
		return "Infinity"
	}

	if v.IsBigInt() {
		return "BigInt"
	}

	if v.IsNumber() {
		return "Number"
	}

	if v.IsDate() {
		return "Date"
	}

	if v.IsBool() {
		return "Boolean"
	}

	if v.IsNull() {
		return "Null"
	}

	if v.IsUndefined() {
		return "Undefined"
	}

	if v.IsUninitialized() {
		return "Uninitialized"
	}

	if v.IsString() {
		return "String"
	}

	if v.IsError() {
		return "Error"
	}

	if v.IsArray() {
		return "Array"
	}

	if v.IsMap() {
		return "Map"
	}

	if v.IsSet() {
		return "Set"
	}

	// Check Promise before Function and Constructor.
	if v.IsPromise() {
		return "Promise"
	}

	// Check Constructor before Function.
	if v.IsConstructor() {
		name := v.GetPropertyStr("name")
		defer name.Free()

		constructorName := ""
		if name.IsString() && name.String() != "" {
			constructorName = " " + name.String()
		}

		return "Constructor" + constructorName
	}

	if v.IsFunction() {
		return "Function"
	}

	if v.IsByteArray() {
		return "ArrayBuffer"
	}

	return "unknown"
}

func (v *Value) NewUndefined() *Value {
	return v.context.NewUndefined()
}

// GetOwnPropertyNames returns the names of the properties of the value.
func (v *Value) GetOwnPropertyNames() (_ []string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = AnyToError(recovered)
		}
	}()
	pList := v.GetOwnProperties()
	defer func() {
		for _, property := range pList {
			property.atom.Free()
		}
	}()
	names := make([]string, len(pList))
	for i := range names {
		names[i] = pList[i].String()
	}
	return names, nil
}

func (v *Value) GetOwnProperties() (properties []OwnProperty) {
	if !v.IsObject() {
		return nil
	}
	// Own JS results avoid the upstream js_malloc array, whose allocator has
	// no exported matching free. The captured intrinsics survive guest overrides.
	keys, err := v.context.invokeIntrinsic("ownKeys", v)
	if err != nil {
		panic(err)
	}
	defer keys.Free()
	complete := false
	defer func() {
		if !complete {
			for _, property := range properties {
				property.atom.Free()
			}
		}
	}()
	for i := int64(0); i < keys.Len(); i++ {
		func() {
			key := keys.GetPropertyIndex(i)
			defer key.Free()
			descriptor, err := v.context.invokeIntrinsic("getOwnPropertyDescriptor", v, key)
			if err != nil {
				panic(err)
			}
			defer descriptor.Free()
			// Proxy traps can remove a property between ownKeys and descriptor lookup.
			if descriptor.IsUndefined() {
				return
			}
			enumerable := descriptor.GetPropertyStr("enumerable")
			defer enumerable.Free()
			atom := v.Call("JS_ValueToAtom", v.Ctx(), key.Raw())
			if atom.Raw() == 0 && v.context.HasException() {
				panic(v.context.Exception())
			}
			properties = append(properties, OwnProperty{isEnumerable: enumerable.Bool(), atom: Atom{context: v.context, Value: atom}})
		}()
	}
	complete = true
	return properties
}

func (v *Value) GetProperty(name *Value) *Value {
	atom := v.Call("JS_ValueToAtom", v.Ctx(), name.Raw())
	defer v.Call("JS_FreeAtom", v.Ctx(), atom.Raw())

	return v.Call("JS_GetProperty", v.Ctx(), v.Raw(), atom.Raw())
}

func (v *Value) SetProperty(name, val *Value) {
	atom := v.Call("JS_ValueToAtom", v.Ctx(), name.Raw())
	defer v.Call("JS_FreeAtom", v.Ctx(), atom.Raw())
	v.Call("JS_SetProperty", v.Ctx(), v.Raw(), atom.Raw(), val.Raw())
}

// GetPropertyStr returns the value of the property with the given name.
func (v *Value) GetPropertyStr(name string) *Value {
	nameVal := v.context.NewStringHandle(name)
	defer v.context.FreeHandle(nameVal.Raw())

	return v.Call("JS_GetPropertyStr", v.Ctx(), v.Raw(), nameVal.Raw())
}

// SetPropertyStr sets the value of the property with the given name.
func (v *Value) SetPropertyStr(name string, val *Value) {
	if val != nil {
		nameVal := v.context.NewStringHandle(name)
		defer nameVal.handle.Free()
		v.Call("JS_SetPropertyStr", v.Ctx(), v.Raw(), nameVal.Raw(), val.Raw())
	}
}

// HasPropertyIndex returns true if the value has the property with the given index.
func (v *Value) HasPropertyIndex(index int64) bool {
	prop := v.context.NewAtomIndex(index)
	defer prop.Free()

	return v.Call("JS_HasProperty", v.Ctx(), v.Raw(), prop.Raw()).Bool()
}

// HasProperty returns true if the value has the property with the given name.
func (v *Value) HasProperty(name string) bool {
	prop := v.context.NewAtom(name)
	defer prop.Free()

	return v.Call("JS_HasProperty", v.Ctx(), v.Raw(), prop.Raw()).Bool()
}

// DeleteProperty deletes the property with the given name.
func (v *Value) DeleteProperty(name string) bool {
	prop := v.context.NewAtom(name)
	defer prop.Free()

	return v.Call("JS_DeleteProperty", v.Ctx(), v.Raw(), prop.Raw(), uint64(1)).Bool()
}

// Invoke call the object's method with the given name and arguments.
func (v *Value) Invoke(fname string, args ...any) (_ *Value, err error) {
	jsArgs := make([]*Value, len(args))
	for i, arg := range args {
		if jsArgs[i], err = ToJsValue(v.context, arg); err != nil {
			return nil, err
		}
		defer jsArgs[i].Free()
	}

	return v.InvokeJS(fname, jsArgs...)
}

// InvokeJS call the object's method with the given name and JS arguments.
func (v *Value) InvokeJS(fname string, args ...*Value) (*Value, error) {
	if !v.IsObject() {
		return v.NewUndefined(), ErrCallFuncOnNonObject
	}

	fn := v.GetPropertyStr(fname)
	defer fn.Free()

	if !fn.IsFunction() {
		return v.NewUndefined(), fmt.Errorf("JS property '%s' is not a function", fname)
	}

	argc, argvPtr := createJsCallArgs(v.context, args...)
	defer v.context.FreeHandle(argvPtr)

	jsCallArgs := []uint64{v.Ctx(), fn.Raw(), v.Raw(), argc, argvPtr}
	result := v.Call("QJS_Call", jsCallArgs...)

	return normalizeJsValue(v.context, result)
}

// SetPropertyIndex sets the value of the property with the given index.
func (v *Value) SetPropertyIndex(index int64, val *Value) {
	v.Call("JS_SetPropertyUint32", v.Ctx(), v.Raw(), uint64(index), val.Raw())
}

// GetPropertyIndex returns the value of the property with the given index.
func (v *Value) GetPropertyIndex(index int64) *Value {
	return v.Call("QJS_GetPropertyUint32", v.Ctx(), v.Raw(), uint64(index))
}

// Len returns the length of the array.
func (v *Value) Len() int64 {
	l := v.GetPropertyStr("length")
	defer l.Free()

	return l.Int64()
}

// ByteLen returns the length of the ArrayBuffer.
func (v *Value) ByteLen() int64 {
	length := v.GetPropertyStr("byteLength")
	defer length.Free()
	return length.Int64()
}

// ToByteArray returns the byte array of the ArrayBuffer.
func (v *Value) ToByteArray() []byte {
	// JS_GetArrayBuffer borrows the backing store: only the packed wrapper
	// is malloc-owned. Keep v alive and copy before freeing that wrapper.
	result := v.Call("QJS_GetArrayBuffer", v.Ctx(), v.Raw())
	defer result.handle.Free()
	if result.Raw() == 0 {
		if v.context.HasException() {
			panic(v.context.Exception())
		}
		return nil
	}
	addr, size := v.context.runtime.mem.UnpackPtr(result.Raw())
	if size == 0 {
		return []byte{}
	}
	return append([]byte(nil), v.context.MemRead(addr, uint64(size))...)
}

func (v *Value) Exception() error {
	cause := v.String()

	stack := v.GetPropertyStr("stack")
	defer stack.Free()

	if stack.IsUndefined() {
		return errors.New(cause)
	}

	stackStr := stack.String()

	return errors.New(cause + "\n" + stackStr)
}

func (v *Value) IsNumber() bool {
	return v.Call("QJS_IsNumber", v.Raw()).handle.Bool()
}

func (v *Value) IsNaN() bool {
	return v.String() == "NaN"
}

func (v *Value) IsInfinity() bool {
	return v.String() == "Infinity"
}

func (v *Value) IsBigInt() bool {
	return v.Call("QJS_IsBigInt", v.Raw()).handle.Bool()
}

func (v *Value) IsDate() bool {
	return v.Call("JS_IsDate", v.Raw()).handle.Bool()
}

func (v *Value) IsBool() bool {
	return v.Call("QJS_IsBool", v.Raw()).handle.Bool()
}

func (v *Value) IsNull() bool {
	return v.Call("QJS_IsNull", v.Raw()).handle.Bool()
}

func (v *Value) IsUndefined() bool {
	return v.Call("QJS_IsUndefined", v.Raw()).handle.Bool()
}

// func (v *Value) IsException() bool {
// 	return v.Call("QJS_IsException", v.Raw()).handle.Bool()
// }

func (v *Value) IsUninitialized() bool {
	return v.Call("QJS_IsUninitialized", v.Raw()).handle.Bool()
}

func (v *Value) IsString() bool {
	return v.Call("QJS_IsString", v.Raw()).handle.Bool()
}

func (v *Value) IsSymbol() bool {
	return v.Call("QJS_IsSymbol", v.Raw()).handle.Bool()
}

func (v *Value) IsQJSProxyValue() bool {
	return v.IsObject() && v.IsGlobalInstanceOf("QJS_PROXY_VALUE")
}

func (v *Value) IsObject() bool {
	return v.Call("QJS_IsObject", v.Raw()).handle.Bool()
}

func (v *Value) IsArray() bool {
	return v.Call("QJS_IsArray", v.Raw()).handle.Bool()
}

func (v *Value) IsError() bool {
	return v.Call("QJS_IsError", v.Ctx(), v.Raw()).handle.Bool()
}

func (v *Value) IsFunction() bool {
	return v.Call("QJS_IsFunction", v.Ctx(), v.Raw()).handle.Bool()
}

func (v *Value) IsConstructor() bool {
	return v.Call("QJS_IsConstructor", v.Ctx(), v.Raw()).handle.Bool()
}

func (v *Value) IsPromise() bool {
	return v.Call("QJS_IsPromise", v.Ctx(), v.Raw()).handle.Bool()
}

// Resolve resolves a promise with the given arguments.
// This method is intended for use with Go function bindings (this.Promise() in async Go functions).
// It will NOT work with native JavaScript promises created via "new Promise()".
// For native JS promises, use direct function calls or Promise.withResolvers instead.
func (v *Value) Resolve(args ...*Value) error {
	if v.IsPromise() {
		result, err := v.InvokeJS("resolve", args...)
		if err != nil {
			return err
		}

		result.Free()
	}

	return nil
}

// Reject rejects a promise with the given arguments.
// This method is intended for use with Go function bindings (this.Promise() in async Go functions).
// It will NOT work with native JavaScript promises created via "new Promise()".
// For native JS promises, use direct function calls or Promise.withResolvers instead.
func (v *Value) Reject(args ...*Value) error {
	if v.IsPromise() {
		result, err := v.InvokeJS("reject", args...)
		if err != nil {
			return err
		}

		result.Free()
	}

	return nil
}

func (v *Value) Await() (*Value, error) {
	if !v.IsPromise() {
		return nil, newInvalidJsInputErr("Promise", v)
	}

	// js_std_await consumes its promise argument. Preserve caller ownership.
	copy := v.Clone()
	result := v.Call("js_std_await", v.Ctx(), copy.Raw())

	return normalizeJsValue(v.context, result)
}

func (v *Value) IsMap() bool {
	return v.IsObject() && v.IsGlobalInstanceOf("Map") ||
		v.String() == "[object Map]"
}

func (v *Value) IsSet() bool {
	return v.IsObject() && v.IsGlobalInstanceOf("Set") ||
		v.String() == "[object Set]"
}

// IsGlobalInstanceOf checks if the value is an instance of the given global constructor.
func (v *Value) IsGlobalInstanceOf(name string) bool {
	ctor := v.context.Global().GetPropertyStr(name)
	defer ctor.Free()

	if ctor.IsUndefined() {
		return false
	}

	instanceOf := v.Call("QJS_IsInstanceOf", v.Ctx(), v.Raw(), ctor.Raw())

	return instanceOf.handle.Bool()
}

// IsByteArray return true if the value is array buffer.
func (v *Value) IsByteArray() bool {
	return v.IsObject() && v.IsGlobalInstanceOf("ArrayBuffer") ||
		v.String() == "[object ArrayBuffer]"
}

// Object returns the object value of the value.
func (v *Value) Object() *Value {
	return v.Call("JS_ToObject", v.Ctx(), v.Raw())
}

// ForEach iterates over the properties of the object and calls the given function for each property.
func (v *Value) ForEach(fn func(key *Value, value *Value)) {
	if !v.IsObject() {
		return
	}
	props := v.GetOwnProperties()
	// Enumeration owns all atoms, including unvisited ones if the callback panics.
	defer func() {
		for _, prop := range props {
			if prop.atom.Value != nil {
				prop.atom.Free()
			}
		}
	}()
	for i, prop := range props {
		func() {
			defer func() { prop.atom.Free(); props[i].atom.Value = nil }()
			key := prop.atom.ToValue()
			defer key.Free()
			if key.String() == "length" {
				return
			}
			value := v.GetProperty(key)
			defer value.Free()
			if v.context.HasException() {
				panic(v.context.Exception())
			}
			fn(key, value)
		}()
	}
}

func (v *Value) Bytes() []byte {
	return v.handle.Bytes()
}

func (v *Value) String() string {
	result := v.Call("QJS_ToCString", v.Ctx(), v.Raw())
	return v.context.readCString(result.handle)
}

// JSONStringify returns the JSON string representation of the value.
func (v *Value) JSONStringify() (_ string, err error) {
	defer func() {
		r := AnyToError(recover())
		if r != nil {
			err = fmt.Errorf("failed to stringify JS value: %w", r)
		}
	}()

	// The upstream packed helper frees its string before returning it.
	result, err := v.context.invokeIntrinsic("stringify", v)
	if err != nil {
		return "", err
	}
	defer result.Free()
	return result.String(), nil
}

// DateTime returns the date value of the value.
func (v *Value) DateTime(tzs ...string) *time.Time {
	var loc *time.Location
	if len(tzs) > 0 && tzs[0] != "" {
		loc = ParseTimezone(tzs[0])
	} else {
		loc = time.Local
	}

	if !v.IsDate() {
		return nil
	}

	epochValue := v.Call("QJS_ToEpochTime", v.Ctx(), v.Raw())
	defer epochValue.Free()

	epoch := epochValue.handle.Float64()

	// Check for NaN which indicates an invalid Date
	// Note: NaN != NaN is true, so this checks for NaN
	if epoch != epoch {
		return nil
	}

	epochNs := int64(epoch * 1e6)
	t := time.Unix(0, epochNs).In(loc)

	return &t
}

// Bool returns the boolean value of the value.
func (v *Value) Bool() bool {
	return v.Call("JS_ToBool", v.Ctx(), v.Raw()).handle.Bool()
}

// Int32 returns the int32 value of the value.
// in c int is 32 bit, but in go it is depends on the architecture.
func (v *Value) Int32() int32 {
	return v.Call("QJS_ToInt32", v.Ctx(), v.Raw()).handle.Int32()
}

func (v *Value) Int64() int64 {
	return v.Call("QJS_ToInt64", v.Ctx(), v.Raw()).handle.Int64()
}

// Uint32 returns the uint32 value of the value.
func (v *Value) Uint32() uint32 {
	return v.Call("QJS_ToUint32", v.Ctx(), v.Raw()).handle.Uint32()
}

// Float64 returns the float64 value of the value.
func (v *Value) Float64() float64 {
	return v.Call("QJS_ToFloat64", v.Ctx(), v.Raw()).handle.Float64()
}

// BigInt returns the big.Int value of the value.
func (v *Value) BigInt() *big.Int {
	if !v.IsBigInt() {
		return nil
	}

	val, _ := new(big.Int).SetString(v.String(), 10)

	return val
}

// ToArray returns the array value of the value. DO NOT FREE.
func (v *Value) ToArray() (*Array, error) {
	if !v.IsArray() {
		return nil, fmt.Errorf("expected JS array, got %s=%s", v.Type(), v.String())
	}

	return NewArray(v), nil
}

// ToMap returns the map value of the value. DO NOT FREE.
func (v *Value) ToMap() *Map {
	if !v.IsMap() {
		return nil
	}

	return NewMap(v)
}

// ToSet returns the set value of the value. DO NOT FREE.
func (v *Value) ToSet() *Set {
	if !v.IsSet() {
		return nil
	}

	return NewSet(v)
}

// New creates a new instance of the value as a constructor with the given arguments.
func (v *Value) New(args ...*Value) *Value {
	return v.CallConstructor(args...)
}

// CallConstructor calls the constructor with the given arguments.
func (v *Value) CallConstructor(args ...*Value) *Value {
	if !v.IsConstructor() {
		return v.context.NewError(ErrObjectNotAConstructor)
	}

	argc, argvPtr := createJsCallArgs(v.context, args...)
	defer v.context.FreeHandle(argvPtr)

	jsCallArgs := []uint64{v.Ctx(), v.Raw(), argc, argvPtr}

	return v.Call("JS_CallConstructor", jsCallArgs...)
}
