package backup

import "github.com/gosuda/flats/internal/notebook"

// The driver's table of collations is not safe against a registration that overlaps a connection open in
// another goroutine. Run registers the collation of a notebook database before it starts a goroutine, but the
// tests run in parallel, so one test's first Run could still overlap the fixture of another test. Registering
// here, before any test starts, keeps the race detector for the goroutines of the code under test.
func init() {
	if err := notebook.RegisterCollation(); err != nil {
		panic(err)
	}
}
