package board

import (
	"context"
	"errors"
	"sync"

	"github.com/gosuda/flats/internal/imagecheck"
)

// AssetInfo describes a checked image.
type AssetInfo = imagecheck.AssetInfo

// CheckImage checks image bytes and remembers their dimensions for content reads.
func CheckImage(data []byte, uploadMax int64) (AssetInfo, error) {
	info, err := imagecheck.CheckImage(data, uploadMax)
	if err != nil {
		return AssetInfo{}, errorf(CategoryInvalid, "%s", err)
	}
	rememberDims(info.Hash, info.W, info.H)
	return info, nil
}

// checkImages checks the images an agent call adds, without storing them.
func checkImages(images [][]byte, uploadMax int64) ([]AssetInfo, error) {
	out := make([]AssetInfo, 0, len(images))
	for _, data := range images {
		a, err := CheckImage(data, uploadMax)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// newBlob is a checked image that may still need storing.
type newBlob struct {
	info AssetInfo
	data []byte
}

// errReplay ends an admission's dry run: the request is a repeated op id,
// which stores nothing and is answered from ops.
var errReplay = errors.New("replayed op")

// admitBlobs admits storing blobs before any is written (5.4), so that a
// refused request leaves no new blob bytes behind and concurrent writers
// cannot spend the same headroom:
//
//   - The blobs the board certainly holds already (an asset row or a
//     retained revision) are skipped; the others count as new.
//   - dry, when set, runs the request in a transaction that is always
//     rolled back, with the new bytes added to the quota formula, and must
//     return the request's refusal. Without dry, the formula plus the new
//     bytes is checked against the quota.
//   - An admitted request reserves the new bytes in the measurement, so
//     every other quota check counts them until release.
//
// store reports whether the caller should store the blobs (false for a
// replay). The caller stores them with putBlobs, which counts each newly
// stored blob, and then calls release (more than once is fine) before the
// request's own quota check.
func (in *Instance) admitBlobs(ctx context.Context, blobs []newBlob, dry func(t *txn) error) (store bool, release func(), err error) {
	release = func() {}
	var need int64
	seen := map[string]bool{}
	for _, b := range blobs {
		h := b.info.Hash
		if seen[h] {
			continue
		}
		seen[h] = true
		if _, ok := in.assets.get(h); ok {
			continue
		}
		if _, ok, err := in.b.RevisionAsset(ctx, h); err == nil && ok {
			continue
		}
		need += b.info.Bytes
	}
	quota := in.b.Limits().DiskQuota
	if need == 0 || (dry == nil && quota <= 0) {
		return true, release, nil
	}
	if quota > 0 {
		if _, err := in.meas.wait(ctx); err != nil {
			return false, release, err
		}
	}
	err = in.do(ctx, func() error {
		t, err := in.begin()
		if err != nil {
			return err
		}
		defer t.rollback()
		t.pending = need
		if dry != nil {
			if err := dry(t); err != nil {
				return err
			}
		} else if over, ok, err := t.overQuota(quota); err != nil {
			return err
		} else if !ok {
			return errorf(CategoryUnavailable, "the board's disk usage cannot be measured right now; try again")
		} else if over {
			return errorf(CategoryCapacity, "the board is out of disk space for this image")
		}
		in.meas.reserve(need)
		return nil
	})
	if errors.Is(err, errReplay) {
		return false, release, nil
	}
	if err != nil {
		return false, release, err
	}
	var once sync.Once
	return true, func() { once.Do(func() { in.meas.reserve(-need) }) }, nil
}

// putBlobs stores admitted blobs and counts the new bytes; locked tells
// that the caller holds the flat lock (5.4, measurer.install).
func (in *Instance) putBlobs(ctx context.Context, blobs []newBlob, locked bool) error {
	for _, b := range blobs {
		stored, err := in.b.PutBlob(ctx, b.info.Hash, b.data)
		if err != nil {
			return err
		}
		if stored {
			in.meas.add(b.info.Bytes, locked)
		}
	}
	return nil
}

// dimCache remembers the pixel size of image blobs by hash. A blob's bytes
// never change, so an entry never goes stale; ContentFromFiles then needs
// no blob read for an asset whose size it knows.
var dimCache = struct {
	sync.Mutex
	m map[string][2]int
}{m: map[string][2]int{}}

const dimCacheMax = 1 << 16

func rememberDims(hash string, w, h int) {
	if w < 1 || h < 1 {
		return
	}
	dimCache.Lock()
	defer dimCache.Unlock()
	if _, ok := dimCache.m[hash]; !ok && len(dimCache.m) >= dimCacheMax {
		for k := range dimCache.m {
			delete(dimCache.m, k)
			break
		}
	}
	dimCache.m[hash] = [2]int{w, h}
}

func knownDims(hash string) ([2]int, bool) {
	dimCache.Lock()
	defer dimCache.Unlock()
	d, ok := dimCache.m[hash]
	return d, ok
}
