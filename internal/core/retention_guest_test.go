package core

import (
	"context"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/gosuda/flats/internal/store"
)

// retainKinds stores revisions after the board's first one, guest-only
// where kinds says 'g' and pinned where it says 'P' (a pinned guest-only
// one) or 'p', runs retention with quota q and returns the kept numbers,
// oldest first. Revision 1 (created) is deleted first so only kinds count.
func retainKinds(t *testing.T, q int, kinds string) []int {
	t.Helper()
	ctx := context.Background()
	s, _ := newTestService(t)
	newBoard(t, s, "plan")
	unlock := s.lock("plan")
	defer unlock()
	var kept []int
	err := s.st.ContentTx(ctx, "plan", func(tx *store.ContentTx) error {
		if err := tx.DeleteRevision(ctx, 1); err != nil {
			return err
		}
		for i, k := range kinds {
			r := store.Revision{Number: i + 2, Kind: "person", Authors: []string{"Ann"}}
			if k == 'g' || k == 'P' {
				r.Authors, r.GuestAuthors, r.GuestOnly = nil, []string{"Kim"}, true
			}
			r.Pinned = k == 'P' || k == 'p'
			if err := tx.InsertRevision(ctx, r, nil); err != nil {
				return err
			}
		}
		if err := tx.SetQuotas(ctx, &q, nil); err != nil {
			return err
		}
		if _, err := s.retainTx(ctx, tx); err != nil {
			return err
		}
		cands, err := tx.RetainCandidates(ctx)
		for _, c := range cands {
			kept = append(kept, c.Number)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pinned revisions are not candidates; add them back for the result.
	for i, k := range kinds {
		if k == 'p' || k == 'P' {
			kept = append(kept, i+2)
		}
	}
	slices.Sort(kept)
	return kept
}

// Retention (board-public-edit-TECH 5.3): guest-only revisions keep at most
// half of the quota, the newest revision and pins always stay.
func TestRetentionGuestOnlyShare(t *testing.T) {
	for _, tc := range []struct {
		q     int
		kinds string // revisions 2, 3, ... oldest first: m member, g guest-only, p/P pinned
		want  []int
	}{
		// Two older guest-only revisions and a guest-only newest.
		{1, "mggmg", []int{6}},
		{2, "mggmg", []int{5, 6}},
		{4, "mggmg", []int{2, 4, 5, 6}},
		{100, "mggmg", []int{2, 3, 4, 5, 6}},
		// A member newest: guests keep floor(Q/2).
		{1, "gggm", []int{5}},
		{2, "gggm", []int{4, 5}},
		{4, "mgggmm", []int{4, 5, 6, 7}},
		// Pinned revisions are neither counted nor deleted.
		{2, "PgPgm", []int{2, 4, 5, 6}},
	} {
		if got := retainKinds(t, tc.q, tc.kinds); !slices.Equal(got, tc.want) {
			t.Errorf("Q=%d %s: kept %v, want %v", tc.q, tc.kinds, got, tc.want)
		}
	}
}

// Over random histories, retention keeps the guarantees of spec rule 25.
func TestRetentionGuestOnlyProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, q := range []int{1, 2, 4, 100} {
		for range 6 {
			n := 1 + rng.IntN(q+12)
			b := make([]byte, n)
			for i := range b {
				b[i] = "mmggp"[rng.IntN(5)]
			}
			kinds := string(b)
			kept := retainKinds(t, q, kinds)
			newest := n + 1
			if !slices.Contains(kept, newest) {
				t.Fatalf("Q=%d %s: newest %d deleted (%v)", q, kinds, newest, kept)
			}
			unpinned, guests, members := 0, 0, 0
			for _, k := range kept {
				switch kinds[k-2] {
				case 'p':
					continue
				case 'g':
					guests++
				default:
					members++
				}
				unpinned++
			}
			for i, k := range kinds {
				if k == 'p' && !slices.Contains(kept, i+2) {
					t.Fatalf("Q=%d %s: pinned %d deleted", q, kinds, i+2)
				}
			}
			limit := q / 2
			if kinds[n-1] == 'g' {
				limit = max(limit, 1)
			}
			if guests > limit || unpinned > q {
				t.Fatalf("Q=%d %s: kept %v (%d guest-only, %d unpinned)", q, kinds, kept, guests, unpinned)
			}
			// Members lose places only to the quota, never to guests.
			total := 0
			for _, k := range kinds {
				if k == 'm' {
					total++
				}
			}
			if want := min(total, q-limit); members < want {
				t.Fatalf("Q=%d %s: %d member revisions kept, want at least %d", q, kinds, members, want)
			}
		}
	}
}
