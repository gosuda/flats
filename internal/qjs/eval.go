package qjs

import "fmt"

func evalPreamble(c *Context, op, file string, flags ...EvalOptionFunc) (*EvalOption, uint64, error) {
	if err := c.checkOpen(); err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", op, file, err)
	}

	option := createEvalOption(c, file, flags...)
	if err := checkEvalOption(option); err != nil {
		return nil, 0, err
	}

	return option, option.Handle(), nil
}

func load(c *Context, file string, flags ...EvalOptionFunc) (*Value, error) {
	if file == "" {
		return nil, ErrInvalidFileName
	}

	flags = append(flags, TypeModule())
	option, handle, err := evalPreamble(c, "load", file, flags...)
	if err != nil {
		return nil, err
	}
	defer option.Free()

	result := c.Call("QJS_Load", c.Raw(), handle)

	return normalizeJsValue(c, result)
}

func eval(c *Context, file string, flags ...EvalOptionFunc) (*Value, error) {
	if file == "" {
		return nil, ErrInvalidFileName
	}

	option, handle, err := evalPreamble(c, "eval", file, flags...)
	if err != nil {
		return nil, err
	}
	defer option.Free()

	result := c.Call("QJS_Eval", c.Raw(), handle)

	return normalizeJsValue(c, result)
}

func compile(c *Context, file string, flags ...EvalOptionFunc) (_ []byte, err error) {
	option, handle, err := evalPreamble(c, "compile", file, flags...)
	if err != nil {
		return nil, err
	}
	defer option.Free()

	result := c.Call("QJS_Compile2", c.Raw(), handle)
	if result, err = normalizeJsValue(c, result); err != nil {
		return nil, err
	}

	defer result.handle.Free()

	bytecodeBytes := result.Bytes()

	bytes := make([]byte, len(bytecodeBytes))
	copy(bytes, bytecodeBytes)

	return bytes, nil
}

func checkEvalOption(option *EvalOption) error {
	if err := cStringError("file", option.file); err != nil {
		return err
	}

	return cStringError("code", option.code)
}

func normalizeJsValue(c *Context, value *Value) (*Value, error) {
	hasException := c.HasException()
	if hasException {
		value.Free()

		return nil, c.Exception()
	}

	if value.IsError() {
		defer value.Free()

		return nil, value.Exception()
	}

	return value, nil
}
