// Package formula is the formula engine of the sheet content type: the
// lexer and parser, the stored reference form and its structural transforms,
// the evaluator with the function library, number formats, typed entry, the
// sort order and CSV fields.
//
// The package is pure. It imports nothing from Flats, does no I/O, reads no
// clock, starts no goroutine and keeps no mutable package state. A parsed
// Formula is immutable and can be shared by cells and read by several
// goroutines at once.
//
// The contract of the function library is the reference table in
// testdata/reference; TestReferenceTable checks every row of it.
package formula
