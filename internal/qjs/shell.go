package qjs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	wsp1 "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// FORK (flats): runtime shells.
//
// A Shell is everything New does before it instantiates qjs.wasm: the wazero
// runtime with its compilation cache, WASI, the "env" host module and the
// compiled module. None of it depends on a flat, so a pre-started worker
// builds one while it waits for its spec; New(Option{Shell: s}) then only
// instantiates the module. A shell is consumed by one New.

// Shell is a prepared, not yet instantiated qjs runtime. See NewShell.
type Shell struct {
	mu       sync.Mutex
	used     bool
	opt      Option // the options the shell was built with
	pages    uint32
	wrt      wazero.Runtime
	compiled wazero.CompiledModule
	wasm     []byte // the patched module bytes
	registry *ProxyRegistry
}

// NewShell prepares a runtime with opt's runtime-wide settings (CacheDir,
// DisableBuildCache, CloseOnContextDone, MemoryLimitPages, QuickJSWasmBytes).
// Pass it to exactly one New with compatible options, or Close it.
func NewShell(opt Option) (s *Shell, err error) {
	defer func() {
		if rerr := AnyToError(recover()); rerr != nil {
			s = nil
			err = fmt.Errorf("failed to create QJS shell: %w", rerr)
		}
	}()
	if opt.Shell != nil || opt.Image != nil || opt.ProxyFunction != nil {
		return nil, errors.New("qjs: a shell is built from runtime options only")
	}
	registry := NewProxyRegistry()
	opt, err = getRuntimeOption(registry, opt)
	if err != nil {
		return nil, fmt.Errorf("failed to get runtime options: %w", err)
	}
	s, err = newShell(opt, registry)
	if err != nil {
		return nil, err
	}
	// Hash the module now: a later image key needs it.
	wasmSum(s.wasm)
	return s, nil
}

func newShell(option Option, registry *ProxyRegistry) (*Shell, error) {
	cache, err := compilationCache(option.CacheDir)
	if err != nil {
		return nil, err
	}
	if option.DisableBuildCache {
		cache = wazero.NewCompilationCache()
	}
	pages := option.MemoryLimitPages
	if pages == 0 {
		pages = DefaultMemoryLimitPages
	}
	rcfg := wazero.NewRuntimeConfig().
		WithCompilationCache(cache).
		WithCloseOnContextDone(option.CloseOnContextDone).
		WithMemoryLimitPages(pages)

	s := &Shell{opt: option, pages: pages, registry: registry}
	s.wrt = wazero.NewRuntimeWithConfig(option.Context, rcfg)
	ok := false
	defer func() {
		if !ok {
			_ = s.wrt.Close(context.Background())
		}
	}()

	if _, err := wsp1.Instantiate(option.Context, s.wrt); err != nil {
		return nil, fmt.Errorf("failed to instantiate WASI: %w", err)
	}

	if _, err := s.wrt.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithFunc(option.ProxyFunction).
		Export("jsFunctionProxy").
		Instantiate(option.Context); err != nil {
		return nil, fmt.Errorf("failed to setup host module: %w", err)
	}

	qjsBytes := wasmBytes
	if len(option.QuickJSWasmBytes) > 0 {
		qjsBytes = option.QuickJSWasmBytes
	}
	if qjsBytes, err = exportStackPointer(qjsBytes); err != nil {
		return nil, fmt.Errorf("failed to prepare qjs module: %w", err)
	}
	if s.compiled, err = s.wrt.CompileModule(option.Context, qjsBytes); err != nil {
		return nil, fmt.Errorf("failed to compile qjs module: %w", err)
	}
	s.wasm = qjsBytes
	ok = true
	return s, nil
}

// Compatible reports whether opt can use this shell: its runtime-wide
// settings equal the shell's and it brings no proxy function of its own.
func (s *Shell) Compatible(opt Option) bool {
	if s == nil || opt.ProxyFunction != nil {
		return false
	}
	pages := opt.MemoryLimitPages
	if pages == 0 {
		pages = DefaultMemoryLimitPages
	}
	return opt.CacheDir == s.opt.CacheDir &&
		opt.DisableBuildCache == s.opt.DisableBuildCache &&
		opt.CloseOnContextDone == s.opt.CloseOnContextDone &&
		pages == s.pages &&
		sameBytes(opt.QuickJSWasmBytes, s.opt.QuickJSWasmBytes)
}

// take hands the shell to one New with options opt.
func (s *Shell) take(opt Option) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used {
		return errors.New("qjs: shell already used")
	}
	if !s.Compatible(opt) {
		return errors.New("qjs: options do not match the shell")
	}
	s.used = true
	return nil
}

// Close releases an unused shell. It does nothing after the shell was used.
func (s *Shell) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.used {
		s.used = true
		_ = s.wrt.Close(context.Background())
	}
}

func sameBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || &a[0] == &b[0]
}

var (
	sumMu   sync.Mutex
	sumMemo = map[*byte][sha256.Size]byte{}
)

// wasmSum is the SHA-256 of patched module bytes (computed once per slice).
func wasmSum(wasm []byte) [sha256.Size]byte {
	sumMu.Lock()
	defer sumMu.Unlock()
	if s, ok := sumMemo[&wasm[0]]; ok {
		return s
	}
	s := sha256.Sum256(wasm)
	sumMemo[&wasm[0]] = s
	return s
}
