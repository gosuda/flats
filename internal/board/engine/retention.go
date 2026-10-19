package engine

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
)

// HoldAsset records an uploaded image and protects it until heldUntil
// (5.5), so an add can reference it before any element does.
func (s *State) HoldAsset(meta Asset, heldUntil int64) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	a := s.editAsset(meta.Hash)
	a.Mime, a.W, a.H, a.Bytes = meta.Mime, meta.W, meta.H, meta.Bytes
	a.HeldUntil = max(a.HeldUntil, heldUntil)
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

// PurgeDeleted removes deleted element rows (4.8 retention). Their assets'
// deleted_refs go down; undoing those deletions is then skipped as gone.
func (s *State) PurgeDeleted(ids []string) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	for _, id := range ids {
		if r := s.rows[id]; r != nil && r.Deleted {
			s.putRow(id, nil)
		}
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

// SweepAssets removes asset rows that nothing references and whose
// protection ended. The blobs stay for the store's sweep to judge.
func (s *State) SweepAssets(now int64) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	for _, h := range slices.Sorted(maps.Keys(s.assets)) {
		if a := s.assets[h]; a.Refs == 0 && a.DeletedRefs == 0 && a.HeldUntil <= now {
			remember(sp.assets, s.assets, h)
			delete(s.assets, h)
		}
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

// receiptBytes approximates the stored size of a receipt row.
func receiptBytes(r *Receipt) int64 {
	n := int64(len(r.Session) + len(r.Results) + 48)
	if r.Reject != nil {
		n += int64(len(r.Reject.Code) + len(r.Reject.Detail))
	}
	return n
}

// TrimReceipts drops receipts until they take at most limit bytes (4.8):
// receipts of expired sessions first, then the oldest. A later resend of a
// dropped batch number gets FatalReceiptExpired.
func (s *State) TrimReceipts(limit, now int64) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	var total int64
	keys := make([]ReceiptKey, 0, len(s.receipts))
	for k, r := range s.receipts {
		total += receiptBytes(r)
		keys = append(keys, k)
	}
	expired := func(k ReceiptKey) bool {
		se := s.sessions[k.Session]
		return se == nil || now >= se.LastAt+SessionTTL
	}
	slices.SortFunc(keys, func(a, b ReceiptKey) int {
		ea, eb := expired(a), expired(b)
		if ea != eb {
			if ea {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(s.receipts[a].Seq, s.receipts[b].Seq), cmp.Compare(a.Session, b.Session), cmp.Compare(a.N, b.N))
	})
	for _, k := range keys {
		if total <= limit {
			break
		}
		total -= receiptBytes(s.receipts[k])
		s.deleteReceipt(k)
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

// TrimDeleted removes the oldest deleted element rows until those left take
// at most limit bytes (4.8).
func (s *State) TrimDeleted(limit int64) Changed {
	type del struct {
		id    string
		seq   int64
		bytes int64
	}
	var dels []del
	var total int64
	for id, r := range s.rows {
		if r.Deleted {
			b, _ := json.Marshal(r.PreRel)
			d := del{id, r.DelSeq, int64(len(r.El.CanonicalJSON()) + len(b))}
			dels = append(dels, d)
			total += d.bytes
		}
	}
	slices.SortFunc(dels, func(a, b del) int { return cmp.Or(cmp.Compare(a.seq, b.seq), cmp.Compare(a.id, b.id)) })
	var purge []string
	for _, d := range dels {
		if total <= limit {
			break
		}
		total -= d.bytes
		purge = append(purge, d.id)
	}
	return s.PurgeDeleted(purge)
}
