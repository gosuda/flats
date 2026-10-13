//go:build race

package bundle

// raceEnabled reports a -race build, whose instrumentation allocates on
// every memory access, so allocation budgets do not apply.
const raceEnabled = true
