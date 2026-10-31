package qjs

import "fmt"

func load(c *Context, file string, flags ...EvalOptionFunc) (*Value, error) {
	if file == "" {
		return nil, ErrInvalidFileName
	}
	if err := c.checkOpen(); err != nil {
		return nil, fmt.Errorf("load %s: %w", file, err)
	}

	// Module: Force TypeModule() since load only works with modules
	flags = append(flags, TypeModule())
	option := createEvalOption(c, file, flags...)
	if err := checkEvalOption(option); err != nil {
		return nil, err
	}

	evalOptions := option.Handle()

	defer option.Free()

	result := c.Call("QJS_Load", c.Raw(), evalOptions)

	return normalizeJsValue(c, result)
}

func eval(c *Context, file string, flags ...EvalOptionFunc) (*Value, error) {
	if file == "" {
		return nil, ErrInvalidFileName
	}
	if err := c.checkOpen(); err != nil {
		return nil, fmt.Errorf("eval %s: %w", file, err)
	}

	option := createEvalOption(c, file, flags...)
	if err := checkEvalOption(option); err != nil {
		return nil, err
	}

	evalOptions := option.Handle()
	defer option.Free()

	result := c.Call("QJS_Eval", c.Raw(), evalOptions)

	return normalizeJsValue(c, result)
}

func compile(c *Context, file string, flags ...EvalOptionFunc) (_ []byte, err error) {
	if err := c.checkOpen(); err != nil {
		return nil, fmt.Errorf("compile %s: %w", file, err)
	}

	option := createEvalOption(c, file, flags...)
	if err := checkEvalOption(option); err != nil {
		return nil, err
	}

	evalOptions := option.Handle()
	defer option.Free()

	result := c.Call("QJS_Compile2", c.Raw(), evalOptions)
	if result, err = normalizeJsValue(c, result); err != nil {
		return nil, err
	}

	defer result.handle.Free()

	bytecodeBytes := result.Bytes()

	// Bytecode: Create independent copy to avoid memory corruption
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
