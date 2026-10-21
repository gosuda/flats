package engine

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"

	"github.com/gosuda/flats/internal/board/model"
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

// trimTwoStep chooses items to drop from items, given in today's trim
// order, with their sizes and whether each is a guest's (TECH public edit
// 4.4): first guest items while guest items exceed guestLimit (negative:
// no guest limit), then guest items and, only when none is left, other
// items while everything exceeds limit. A member item is therefore dropped
// only when member items alone exceed limit.
func trimTwoStep(n int, size func(i int) int64, guest func(i int) bool, limit, guestLimit int64) []int {
	var total, guestTotal int64
	for i := range n {
		total += size(i)
		if guest(i) {
			guestTotal += size(i)
		}
	}
	dropped := make([]bool, n)
	var out []int
	drop := func(i int) {
		dropped[i] = true
		out = append(out, i)
		total -= size(i)
		if guest(i) {
			guestTotal -= size(i)
		}
	}
	for i := 0; i < n && guestLimit >= 0 && guestTotal > guestLimit; i++ {
		if guest(i) {
			drop(i)
		}
	}
	for _, guests := range []bool{true, false} {
		for i := 0; i < n && total > limit; i++ {
			if !dropped[i] && guest(i) == guests {
				drop(i)
			}
		}
	}
	return out
}

// TrimReceipts drops receipts until they take at most limit bytes (4.8):
// guests' first, then receipts of expired sessions, then the oldest. A
// later resend of a dropped batch number gets FatalReceiptExpired.
func (s *State) TrimReceipts(limit, now int64) Changed { return s.TrimReceiptsCapped(limit, -1, now) }

// TrimReceiptsCapped is TrimReceipts that also keeps guests' receipts at
// most guestLimit bytes, in two steps (trimTwoStep).
func (s *State) TrimReceiptsCapped(limit, guestLimit, now int64) Changed {
	m := s.Savepoint()
	sp := s.sps[m]
	keys := make([]ReceiptKey, 0, len(s.receipts))
	for k := range s.receipts {
		keys = append(keys, k)
	}
	expired := func(k ReceiptKey) bool {
		se := s.sessions[k.Session]
		return se == nil || se.expired(now)
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
	drop := trimTwoStep(len(keys), func(i int) int64 { return receiptBytes(s.receipts[keys[i]]) },
		func(i int) bool { return s.GuestSession(keys[i].Session) }, limit, guestLimit)
	for _, i := range drop {
		s.deleteReceipt(keys[i])
	}
	c := s.changedSince(sp)
	s.Release(m)
	return c
}

// TrimDeleted removes the oldest deleted element rows until those left take
// at most limit bytes (4.8), rows guests deleted first.
func (s *State) TrimDeleted(limit int64) Changed { return s.TrimDeletedCapped(limit, -1) }

// TrimDeletedCapped is TrimDeleted that also keeps the rows guests deleted
// at most guestLimit bytes, in two steps (trimTwoStep).
func (s *State) TrimDeletedCapped(limit, guestLimit int64) Changed {
	type del struct {
		id    string
		seq   int64
		bytes int64
		guest bool
	}
	var dels []del
	for id, r := range s.rows {
		if r.Deleted {
			b, _ := json.Marshal(r.PreRel)
			dels = append(dels, del{id, r.DelSeq, int64(len(r.El.CanonicalJSON()) + len(b)), model.IsGuestKey(r.DelKey)})
		}
	}
	slices.SortFunc(dels, func(a, b del) int { return cmp.Or(cmp.Compare(a.seq, b.seq), cmp.Compare(a.id, b.id)) })
	var purge []string
	for _, i := range trimTwoStep(len(dels), func(i int) int64 { return dels[i].bytes }, func(i int) bool { return dels[i].guest }, limit, guestLimit) {
		purge = append(purge, dels[i].id)
	}
	return s.PurgeDeleted(purge)
}
