// Package runtimeref contains the runtime contract shipped with the host.
package runtimeref

import _ "embed"

const Version = "1"
const URI = "flats://docs/runtime-api/v1"

// Markdown is the public, source-independent runtime API contract.
//
//go:embed runtime-api-v1.md
var Markdown string

const ContentTypesVersion = "1"
const ContentTypesURI = "flats://docs/content-types/v1"

// ContentTypesMarkdown describes the host content adapters.
//
//go:embed content-types.md
var ContentTypesMarkdown string
