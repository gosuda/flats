package sheet

import (
	"maps"
	"math/rand"
	"slices"
	"testing"
)

// randomAxis returns an axis of n ids, drawn from 1 to 3n, in a random
// order, whose born stamps change every few ids.
func randomAxis(rng *rand.Rand, n int) axis {
	pick := rng.Perm(3 * n)[:n]
	ids := make([]uint64, n)
	for i, p := range pick {
		ids[i] = uint64(p) + 1
	}
	sorted := slices.Sorted(slices.Values(ids))
	a := axis{ids: ids, pos: map[uint64]int32{}}
	seq := uint64(1)
	for _, id := range sorted {
		if rng.Intn(4) == 0 {
			seq++
		}
		a.born.set(id, 1, seq)
	}
	a.reindex(0)
	return a
}

// stampMap returns the born stamp of every id of an axis.
func stampMap(a *axis) map[uint64]uint64 {
	m := make(map[uint64]uint64, len(a.ids))
	for _, id := range a.ids {
		m[id] = a.born.get(id)
	}
	return m
}

// wantAxis checks that an axis holds want's ids in want's order, each at
// its position and with want's born stamp, and that its runs are sorted,
// apart, joined where they can be, and stamp nothing else.
func wantAxis(t *testing.T, what string, got, want *axis) {
	t.Helper()
	if !slices.Equal(got.ids, want.ids) {
		t.Fatalf("%s: the ids are %v, want %v", what, got.ids, want.ids)
	}
	if len(got.pos) != len(got.ids) {
		t.Fatalf("%s: %d positions for %d ids", what, len(got.pos), len(got.ids))
	}
	for i, id := range got.ids {
		if got.pos[id] != int32(i) {
			t.Fatalf("%s: id %d is at %d, its position says %d", what, id, i, got.pos[id])
		}
	}
	if !maps.Equal(stampMap(got), stampMap(want)) {
		t.Fatalf("%s: the born stamps differ", what)
	}
	var n uint64
	for i, r := range got.born {
		if r.count == 0 || (i > 0 && got.born[i-1].first+got.born[i-1].count > r.first) {
			t.Fatalf("%s: run %d %+v after %+v", what, i, r, got.born[max(i-1, 0)])
		}
		if i > 0 && got.born[i-1].first+got.born[i-1].count == r.first && got.born[i-1].seq == r.seq {
			t.Fatalf("%s: runs %d and %d are one", what, i-1, i)
		}
		n += r.count
	}
	if n != uint64(len(got.ids)) {
		t.Fatalf("%s: the runs stamp %d ids, the axis holds %d", what, n, len(got.ids))
	}
}

func cloneAxis(a *axis) axis {
	return axis{ids: slices.Clone(a.ids), pos: maps.Clone(a.pos), born: slices.Clone(a.born)}
}

// stopAfter returns a pacer that lets k calls go on and ends the loop at
// the next one.
func stopAfter(k int) pacer {
	return func(int) bool {
		k--
		return k >= 0
	}
}

// TestAxisEditsKeepOrderAndStamps takes random runs of ids out of random
// axes and puts them back, whole and stopped at every point their pacer is
// asked. An edit that is stopped and then undone, as the journal undoes
// it, leaves the axis as it was, and one that runs to its end leaves what
// the edit means.
func TestAxisEditsKeepOrderAndStamps(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 40; round++ {
		n := 1 + rng.Intn(3*paceEvery)
		a := randomAxis(rng, n)
		at := rng.Intn(n)
		k := 1 + rng.Intn(n-at)
		gone := slices.Clone(a.ids[at : at+k])
		want := cloneAxis(&a)
		want.ids = slices.Delete(want.ids, at, at+k)
		want.pos = map[uint64]int32{}
		want.reindex(0)
		want.born = nil
		for _, id := range slices.Sorted(slices.Values(want.ids)) {
			want.born.set(id, 1, a.born.get(id))
		}

		for stop := 0; ; stop++ {
			b := cloneAxis(&a)
			sorted, ok := sortedIDs(gone, stopAfter(stop))
			var runs, rest runStamps
			if ok {
				runs, ok = b.born.stampsOf(sorted, stopAfter(stop))
			}
			if ok {
				rest, ok = b.born.without(runs, stopAfter(stop))
			}
			if !ok {
				// Nothing has changed yet.
				wantAxis(t, "a deletion stopped while its stamps were worked out", &b, &a)
				continue
			}
			if b.take(at, gone, rest, stopAfter(stop)) {
				wantAxis(t, "a deletion", &b, &want)
				// Its undo puts the ids back with their stamps.
				b.insert(at, gone, runs)
				wantAxis(t, "the undo of a deletion", &b, &a)
				break
			}
			b.insert(at, gone, runs)
			wantAxis(t, "the undo of a stopped deletion", &b, &a)
		}

		// The ids return with a stamp of their own, as an undo brings them.
		after := cloneAxis(&want)
		sorted, _ := sortedIDs(gone, nil)
		add, _ := stamped(sorted, 99, nil)
		back := cloneAxis(&a)
		back.born = nil
		for _, id := range slices.Sorted(slices.Values(a.ids)) {
			seq := a.born.get(id)
			if slices.Contains(gone, id) {
				seq = 99
			}
			back.born.set(id, 1, seq)
		}
		for stop := 0; ; stop++ {
			b := cloneAxis(&after)
			born, ok := b.born.with(add, stopAfter(stop))
			if !ok {
				wantAxis(t, "an insert stopped while its stamps were worked out", &b, &after)
				continue
			}
			done := b.put(at, gone, born, stopAfter(stop))
			if done {
				wantAxis(t, "an insert", &b, &back)
			}
			rest, _ := b.born.without(add, nil)
			b.take(at, gone, rest, nil)
			wantAxis(t, "the undo of an insert", &b, &after)
			if done {
				break
			}
		}
	}
}
