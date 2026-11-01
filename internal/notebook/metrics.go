package notebook

import (
	"errors"
	"sync/atomic"
)

// Stats contains process-wide notebook transport and write counters.
type Stats struct{ Applied, Rejected, Retried, Connections int64 }

var notebookApplied, notebookRejected, notebookRetried, notebookConnections atomic.Int64

// HostStats returns a race-safe snapshot, independent of board metrics.
func HostStats() Stats {
	return Stats{Applied: notebookApplied.Load(), Rejected: notebookRejected.Load(), Retried: notebookRetried.Load(), Connections: notebookConnections.Load()}
}

func recordBatch(err error) {
	if err == nil {
		notebookApplied.Add(1)
		return
	}
	var problem *WriteError
	if errors.As(err, &problem) && problem.Retry {
		notebookRetried.Add(1)
		return
	}
	notebookRejected.Add(1)
}
