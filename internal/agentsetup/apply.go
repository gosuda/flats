package agentsetup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ErrChanged reports that a file changed between the plan and the write.
var ErrChanged = errors.New("the file changed while the command ran")

// ApplyOptions tells Apply where to keep backups.
type ApplyOptions struct {
	BackupDir string
	Now       func() time.Time // nil: time.Now
}

// Apply carries out one planned item. It copies an existing file to the backup
// directory first, and it changes nothing when the file no longer matches the
// plan. Items that need no change, and print-only items, do nothing.
func Apply(it *Item, o ApplyOptions) error {
	if it.Err != nil || it.Kind == KindPrint || it.Action == Unchanged {
		return nil
	}
	if it.Exists {
		if err := backup(it, o); err != nil {
			return fmt.Errorf("back up %s: %w", it.Real, err)
		}
	}
	if it.Action == Delete {
		if err := unchanged(it); err != nil {
			return err
		}
		return os.Remove(it.Real)
	}
	dir := filepath.Dir(it.Real)
	if err := os.MkdirAll(dir, it.dirMode); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(it.Real)+".flats-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(it.After); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, it.mode); err != nil {
		return err
	}
	if err := unchanged(it); err != nil {
		return err
	}
	return os.Rename(tmp, it.Real)
}

// unchanged checks that the target still holds what the plan read.
func unchanged(it *Item) error {
	cur, err := os.ReadFile(it.Real)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !it.Exists:
		return nil
	case err != nil:
		return fmt.Errorf("%w: %s: %v", ErrChanged, it.Real, err)
	case !it.Exists || !bytes.Equal(cur, it.Before):
		return fmt.Errorf("%w: %s", ErrChanged, it.Real)
	}
	return nil
}

// backup copies the file the plan read into the backup directory. The name is
// <UTC time>-<8 hex digits of the SHA-256 of the path>-<file name>, with a
// counter when a backup of the same second exists.
func backup(it *Item, o ApplyOptions) error {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	if err := os.MkdirAll(o.BackupDir, 0o700); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(it.Real))
	base := fmt.Sprintf("%s-%s-%s", now().UTC().Format("20060102T150405Z"), hex.EncodeToString(sum[:4]), filepath.Base(it.Real))
	for n := 0; ; n++ {
		name := base
		if n > 0 {
			name = fmt.Sprintf("%s.%d", base, n)
		}
		f, err := os.OpenFile(filepath.Join(o.BackupDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) && n < 100 {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := f.Write(it.Before); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
}
