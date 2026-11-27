package sheet

import "sync/atomic"

// workSteps counts steps of work that tests bound by their number rather
// than by the time they take, which a busy machine stretches. Each field
// is one kind of step.
type workSteps struct {
	formulas atomic.Int64 // formulas a walk of the workbook visited (eachFormulaPaced)
	shared   atomic.Int64 // entries of the table of shared formulas that intern looked at
	weighed  atomic.Int64 // bytes of typed text that weigh counted
	sorted   atomic.Int64 // comparisons and swaps of the sort of the interval index
	ordered  atomic.Int64 // comparisons that put the cells of a captured row in order
	notes    atomic.Int64 // notes a sweep of undo looked at
}

// counting holds the counts while a test counts. It is nil outside tests.
var counting atomic.Pointer[workSteps]
