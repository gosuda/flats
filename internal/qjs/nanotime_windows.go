//go:build windows

package qjs

import "time"

// nanoBase anchors hostNanotime. wazero's WithSysNanotime on Windows uses a
// base private to wazero, so absolute values differ from it by a constant;
// only differences are meaningful to the guest.
var nanoBase = time.Now()

// hostNanotime is the host monotonic clock.
func hostNanotime() int64 { return time.Since(nanoBase).Nanoseconds() }
