package runtimeref

// STUB (Autopilot unit A): the two types of autopilot-TECH.md 5.9, so core
// compiles Autopilot.GuideState before unit C adds docs/autopilot.go.
// Delete this file when docs/autopilot.go defines them.

// AutopilotOperation is one operation agents may decide.
type AutopilotOperation struct {
	ID       string // "deploy"
	Label    string // "Deploy"
	Requests string // "deploy of a saved version (activation or redeploy)"
}

// AutopilotState is what the on texts depend on. A nil *AutopilotState
// means Autopilot is off.
type AutopilotState struct {
	Allowed []AutopilotOperation // in the order of 2.2; empty when none
	Guide   string
}
