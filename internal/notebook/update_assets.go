package notebook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Update already owns the host storage lock and its measured quota input.
type assetMeasurementKey struct{}

func (in *Instance) prepareUpdateAssets(ctx context.Context, u Update, other int64) (Update, error) {
	ctx = context.WithValue(ctx, assetMeasurementKey{}, other)
	uploads, err := in.PrepareUploads(ctx, u.Assets)
	if err != nil {
		return u, err
	}
	for _, op := range u.Ops {
		markdown, ok := op.(*MarkdownToBlocksOp)
		if !ok {
			continue
		}
		for i := range markdown.AssetRefs {
			target := &markdown.AssetRefs[i].Asset
			if target.Tmp != "" {
				hash, ok := uploads[target.Tmp]
				if !ok {
					return u, &WriteError{Code: "invalid", Reason: "asset"}
				}
				*target = AssetTarget{Hash: hash}
			}
		}
		if _, err := in.PrepareAssets(ctx, markdown.AssetRefs); err != nil {
			return u, err
		}
	}
	u.Assets = nil
	return u, nil
}

func (in *Instance) replayUpdate(ctx context.Context, op, hash string) (WriteResult, bool, error) {
	var result WriteResult
	var found bool
	err := in.ReadTx(ctx, func(tx *sql.Tx, _ Sequence) error {
		var storedHash, raw string
		err := tx.QueryRowContext(ctx, `SELECT request_hash,outcome FROM commands WHERE op_id=?`, op).Scan(&storedHash, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if hash != storedHash {
			return &WriteError{Code: "conflict", Reason: "op_id_conflict"}
		}
		var out commandOutcome
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return err
		}
		if out.Error != nil {
			return out.Error
		}
		if out.Result == nil {
			return unavailable("receipt_corrupt")
		}
		result = *out.Result
		result.Replay = true
		return nil
	})
	return result, found, err
}
