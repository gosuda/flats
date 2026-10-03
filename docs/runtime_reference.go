// Package runtimeref contains the runtime contract shipped with the host.
package runtimeref

import _ "embed"

const Version = "1"
const URI = "flats://docs/runtime-api/v1"

// Markdown is the public, source-independent runtime API contract.
//
//go:embed runtime-api-v1.md
var Markdown string
