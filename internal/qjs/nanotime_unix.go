//go:build !windows

package qjs

import _ "unsafe" // for go:linkname

// hostNanotime is the host monotonic clock exactly as wazero's
// WithSysNanotime reads it on this platform (runtime.nanotime).
//
//go:linkname hostNanotime runtime.nanotime
func hostNanotime() int64
