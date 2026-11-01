package notebook

// View and order writes sanitize dependencies against the apply-time schema.
func databaseResolvesDependencies(op Operation) bool {
	switch op.(type) {
	case *CreateViewOp, *SetViewOp, *SetPropertyOrderOp, *SetOptionOrderOp, *SetViewOrderOp:
		return true
	default:
		return false
	}
}
