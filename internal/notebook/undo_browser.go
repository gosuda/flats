package notebook

import (
	"context"
	"database/sql"
)

// UndoSummary is the bounded browser projection of a personal inverse result.
type UndoSummary struct {
	Applied          int `json:"applied"`
	SkippedLater     int `json:"skipped_later"`
	SkippedGone      int `json:"skipped_gone"`
	SkippedStructure int `json:"skipped_structure"`
}

func isUndoControl(op Operation) bool {
	return op.OperationName() == "undo" || op.OperationName() == "redo"
}

func applyBrowserUndo(ctx context.Context, tx *sql.Tx, opts editOptions, redo bool) (editResult, *UndoSummary, error) {
	summary, err := undoEdit(ctx, tx, opts, redo)
	if err != nil {
		return editResult{}, nil, err
	}
	result := operationResult{Index: 0, Status: "applied"}
	if summary.Applied == 0 {
		result.Status = "ignored"
	} else if summary.Skipped > 0 {
		result.Status = "adjusted"
	}
	if summary.Empty {
		result.Reason = "nothing_to_undo"
		if redo {
			result.Reason = "nothing_to_redo"
		}
		if summary.Evicted {
			result.Reason = "undo_evicted"
		}
	} else if summary.Capacity > 0 {
		result.Reason = "item_limit"
	}
	return editResult{Results: []operationResult{result}, Records: summary.Records}, &UndoSummary{
		Applied: summary.Applied, SkippedLater: summary.LaterEdits, SkippedGone: summary.Missing,
		SkippedStructure: summary.Structure + summary.Capacity,
	}, nil
}
