package runtime

// startPrewarm prepares flat-independent engine state in a pre-started
// worker before it receives its spec. It does not block.
func startPrewarm(p prewarmSpec) { _ = p }

// waitPrewarm returns when prewarm started by startPrewarm has finished.
func waitPrewarm() {}
