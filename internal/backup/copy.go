package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/flats/internal/durable"
)

// maxTries bounds every retry of the spec: an unstable file, a busy snapshot, a
// file that is gone and an attempt at coherence.
const maxTries = 3

// vanishPause is how long the command waits before it looks again for a file
// that is gone (rule 8).
const vanishPause = 100 * time.Millisecond

// busyTimeout is how long a snapshot waits for a lock before it counts as busy (rule 4).
const busyTimeout = 5 * time.Second

var bufPool = sync.Pool{New: func() any { b := make([]byte, 256<<10); return &b }}

// copier holds the state of one run.
type copier struct {
	ctx        context.Context
	data       string // the resolved data directory
	root       string // the partial directory; the copy of the data directory is root/data
	excludeKey bool

	// mu guards the fields below while snapshots run together (rule 15).
	mu       sync.Mutex
	files    map[string]File   // by manifest path
	dbs      map[string]string // manifest path of each database -> kind
	excluded map[string]bool
	skipped  map[string]string
	unstable map[string]bool
	retries  int

	// Test hooks. They are nil in production. order makes the snapshots of one
	// attempt run one by one, in the order that it returns, instead of together.
	afterSnap func(rel string)
	midCopy   func(src string)
	order     func(all []dbEntry) []dbEntry
	maxGroup  int
	busy      time.Duration
}

func newCopier(ctx context.Context, data, root string, excludeKey bool) *copier {
	return &copier{
		ctx: ctx, data: data, root: root, excludeKey: excludeKey,
		files: map[string]File{}, dbs: map[string]string{}, excluded: map[string]bool{},
		skipped: map[string]string{}, unstable: map[string]bool{}, maxGroup: maxTogether, busy: busyTimeout,
	}
}

// manifestPath is the path of rel in the copy.
func manifestPath(rel string) string { return "data/" + rel }

func (c *copier) dst(rel string) string {
	return filepath.Join(c.root, "data", filepath.FromSlash(rel))
}

func (c *copier) src(rel string) string { return filepath.Join(c.data, filepath.FromSlash(rel)) }

func (c *copier) mkdir(rel string) error { return os.MkdirAll(c.dst(rel), 0o700) }

func (c *copier) skip(rel, reason string) { c.skipped[manifestPath(rel)] = reason }

func (c *copier) skipMP(mp, reason string) { c.skipped[mp] = reason }

func (c *copier) exclude(rel string) { c.excluded[manifestPath(rel)] = true }

// copyOnce copies src to dst and reports whether src was stable: the same size
// and modification time before and after the copy, and as many bytes read as
// the size said (rule 6).
func (c *copier) copyOnce(src, dst string) (File, bool, error) {
	before, err := os.Stat(src)
	if err != nil {
		return File{}, false, err
	}
	in, err := durable.Open(src)
	if err != nil {
		return File{}, false, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return File{}, false, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return File{}, false, err
	}
	h := sha256.New()
	bp := bufPool.Get().(*[]byte)
	n, err := io.CopyBuffer(io.MultiWriter(out, h), in, *bp)
	bufPool.Put(bp)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return File{}, false, err
	}
	if c.midCopy != nil {
		c.midCopy(src)
	}
	after, err := os.Stat(src)
	if err != nil {
		return File{}, false, err
	}
	stable := n == before.Size() && after.Size() == before.Size() && after.ModTime().Equal(before.ModTime())
	return File{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, stable, nil
}

// skipsVanished reports whether a file of class cl that is gone is skipped
// because the host removes such files on purpose (rule 8).
func skipsVanished(rel string, cl class) bool {
	return cl == classWriteOnce || cl == classGuest ||
		strings.HasPrefix(rel, nameConfigHist+"/") || strings.HasPrefix(rel, "network-retirements/")
}

// copyFile copies rel, with the retries of rules 6 to 8 for its class.
func (c *copier) copyFile(rel string, cl class) error {
	return c.copyTo(c.src(rel), c.dst(rel), manifestPath(rel), rel, cl)
}

// copyTo copies src to dst, which the manifest knows as mp. rel decides whether a
// file that is gone is skipped (rule 8).
func (c *copier) copyTo(src, dst, mp, rel string, cl class) error {
	gone := 0
	for tries := 0; tries < maxTries; {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		f, stable, err := c.copyOnce(src, dst)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			os.Remove(dst)
			if skipsVanished(rel, cl) {
				c.skipMP(mp, "gone while it was copied")
				return nil
			}
			if gone++; gone >= maxTries {
				return &RerunError{fmt.Errorf("%s is gone", rel)}
			}
			time.Sleep(vanishPause)
			continue
		case err != nil:
			return fmt.Errorf("copy %s: %w", rel, err)
		case stable:
			f.Path = mp
			c.files[mp] = f
			return nil
		}
		if tries++; tries < maxTries {
			c.retries++
		}
		if tries == maxTries {
			switch cl {
			case classGuest:
				f.Path = mp
				c.files[mp] = f
				c.unstable[mp] = true
				return nil
			case classWriteOnce:
				os.Remove(dst)
				c.skipMP(mp, "kept changing while it was copied")
				return nil
			}
			os.Remove(dst)
			return &RerunError{fmt.Errorf("%s changed while it was copied", rel)}
		}
	}
	return nil
}

// hashFile returns the size and SHA-256 of the file at path.
func hashFile(path string) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	h := sha256.New()
	bp := bufPool.Get().(*[]byte)
	n, err := io.CopyBuffer(h, f, *bp)
	bufPool.Put(bp)
	if err != nil {
		return File{}, err
	}
	return File{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// manifest builds the manifest of what the copier took.
func (c *copier) manifest() *Manifest {
	m := &Manifest{Format: FormatVersion}
	for _, f := range c.files {
		m.Files = append(m.Files, f)
	}
	for p, k := range c.dbs {
		m.Databases = append(m.Databases, Database{Path: p, Kind: k})
	}
	for p := range c.excluded {
		m.Excluded = append(m.Excluded, p)
	}
	for p, r := range c.skipped {
		m.Skipped = append(m.Skipped, Skip{Path: p, Reason: r})
	}
	for p := range c.unstable {
		m.Unstable = append(m.Unstable, p)
	}
	m.sort()
	return m
}

// syncTree makes the directories of the copy durable.
func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if err := durable.SyncDir(d); err != nil {
			return err
		}
	}
	return nil
}
