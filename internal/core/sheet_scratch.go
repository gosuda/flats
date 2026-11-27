package core

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gosuda/flats/internal/durable"
	"github.com/gosuda/flats/internal/filelock"
)

// sheetScratch is the host's scratch storage for the large undo payloads
// of sheets (technical design, section 14). It follows the notebook's
// spill storage: every running host has one directory under
// cache/sheet-undo and holds a file lock in it for as long as it runs. A
// start removes the directories whose lock is free, which a host that was
// killed left behind, and keeps those of hosts that still run on the same
// data directory. The directory is outside every flat's directory, so its
// bytes are in no disk quota, revision or backup.
type sheetScratch struct {
	dir  string
	lock *os.File
}

const sheetScratchOwner = "owner-"

// sheetScratchName reports whether name is an owner directory. The fixed
// form keeps the cleanup away from anything else in the directory.
func sheetScratchName(name string) bool {
	id, ok := strings.CutPrefix(name, sheetScratchOwner)
	if !ok || len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// lockSheetScratch makes root and takes the lock that a host holds while it
// makes its directory or removes those of others, so that hosts that start
// at the same time take turns. With wait it waits for the lock. Without,
// a lock that another host holds is filelock.ErrLocked.
func lockSheetScratch(root string, wait bool) (unlock func(), err error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	namespace, err := os.OpenFile(filepath.Join(root, ".owners"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if wait {
		err = filelock.Lock(namespace)
	} else {
		err = filelock.TryLock(namespace)
	}
	if err != nil {
		return nil, errors.Join(err, namespace.Close())
	}
	return func() {
		filelock.Unlock(namespace)
		namespace.Close()
	}, nil
}

// removeSheetScratchLeftovers removes from root the directories of hosts
// that no longer run. The caller holds the lock of lockSheetScratch. A
// leftover that cannot be examined or removed now is reported through logf
// and stays for the next start: it must not keep this host from its own
// directory.
func removeSheetScratchLeftovers(root string, logf func(string, ...any)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !sheetScratchName(entry.Name()) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		// A host that runs holds the lock, and a host that closes at this
		// moment removes its directory itself.
		lock, err := os.OpenFile(filepath.Join(path, ".owner"), os.O_CREATE|os.O_RDWR, 0o600)
		if err == nil {
			err = filelock.TryLock(lock)
			lock.Close() // an open file cannot be removed on Windows
			if err == nil {
				err = durable.RemoveAll(path)
			}
		}
		if err != nil && !errors.Is(err, filelock.ErrLocked) && !errors.Is(err, os.ErrNotExist) {
			logf("sheet scratch storage: leftover %s stays: %v", entry.Name(), err)
		}
	}
	return nil
}

// openSheetScratch makes the directory of this host under root. With
// sweep it waits for hosts that start at the same time and first removes
// the directories of hosts that no longer run, as a start does. Without,
// it removes nothing and waits for no one, which bounds what a caller that
// holds a workbook lock spends here.
func openSheetScratch(root string, logf func(string, ...any), sweep bool) (*sheetScratch, error) {
	unlock, err := lockSheetScratch(root, sweep)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if sweep {
		if err := removeSheetScratchLeftovers(root, logf); err != nil {
			return nil, err
		}
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	o := &sheetScratch{dir: filepath.Join(root, sheetScratchOwner+hex.EncodeToString(id[:]))}
	if err := os.Mkdir(o.dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(o.dir, ".owner"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.Join(err, durable.RemoveAll(o.dir))
	}
	if err := filelock.Lock(lock); err != nil {
		return nil, errors.Join(err, lock.Close(), durable.RemoveAll(o.dir))
	}
	o.lock = lock
	if _, err := fmt.Fprintf(lock, "%d\n", os.Getpid()); err != nil {
		return nil, errors.Join(err, o.close())
	}
	return o, nil
}

// close gives the directory up and removes it.
func (o *sheetScratch) close() error {
	err := o.lock.Close() // an open file cannot be removed on Windows
	return errors.Join(err, durable.RemoveAll(o.dir))
}

// sweepSheetScratch removes from root the directories of hosts that no
// longer run, waiting for hosts that start at the same time.
func sweepSheetScratch(root string, logf func(string, ...any)) error {
	unlock, err := lockSheetScratch(root, true)
	if err != nil {
		return err
	}
	defer unlock()
	return removeSheetScratchLeftovers(root, logf)
}

// sheetScratchState is the scratch storage of one service. mu is held for
// short steps only, never while waiting for another host or removing
// leftovers: a sheet that asks for the directory holds its workbook lock.
type sheetScratchState struct {
	mu    sync.Mutex
	owner *sheetScratch
	// gone are directories of this host that were not found any more. The
	// host keeps their locks until it closes: should one of them still be
	// there, no start of another host takes it for a leftover.
	gone []*sheetScratch
	// swept is set once this host removed what earlier hosts left.
	swept  bool
	closed bool
}

var (
	errSheetScratchClosed = errors.New("the host is closing its sheet scratch storage")
	errSheetScratchBusy   = errors.New("the sheet scratch storage is being set up")
	errSheetScratchGone   = errors.New("the sheet scratch directory was removed while it was set up")
)

func (s *Service) sheetScratchRoot() string {
	return filepath.Join(s.cfg.DataDir, "cache", "sheet-undo")
}

// current returns the host's directory, or "" when it has none: none could
// be made yet, or it was removed with its files while the host ran. The
// caller holds st.mu.
func (st *sheetScratchState) current() string {
	if st.owner == nil {
		return ""
	}
	if _, err := os.Stat(st.owner.dir); errors.Is(err, os.ErrNotExist) {
		st.gone, st.owner = append(st.gone, st.owner), nil
		return ""
	}
	return st.owner.dir
}

// sheetScratchDir is the directory the instances of this host keep their
// undo payloads in, as a start prepares it: what earlier hosts left is
// removed, after waiting for hosts that start at the same time. Core calls
// it at startup and before each instance opens, where no workbook is
// locked. When the directory cannot be made it logs why, and every later
// call tries again, as sheetScratchNow does.
func (s *Service) sheetScratchDir() (string, error) {
	st := &s.sheetScratch
	root := s.sheetScratchRoot()
	st.mu.Lock()
	closed, dir, swept := st.closed, st.current(), st.swept
	st.mu.Unlock()
	var made *sheetScratch
	var err error
	switch {
	case closed:
		return "", errSheetScratchClosed
	case dir == "":
		made, err = openSheetScratch(root, s.logf, true)
	case !swept:
		// A sheet made the directory without the removal a start does.
		err = sweepSheetScratch(root, s.logf)
	default:
		return dir, nil
	}
	if err != nil {
		s.logf("sheet scratch storage: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.swept = st.swept || err == nil
	if made != nil {
		if st.closed || st.current() != "" {
			// The host closed, or a sheet made the directory meanwhile.
			if err := made.close(); err != nil {
				s.logf("sheet scratch cleanup: %v", err)
			}
		} else {
			st.owner = made
		}
	}
	if st.closed {
		return "", errSheetScratchClosed
	}
	if dir = st.current(); dir != "" {
		return dir, nil
	}
	if err == nil {
		err = errSheetScratchGone
	}
	return "", err
}

// sheetScratchNow is the directory for an instance that needs a scratch
// file while it holds its workbook lock (sheet.Options.ScratchRoot). When
// the host has none it makes one, and waits for nothing and removes
// nothing for it: while this host or another sets a directory up at that
// moment it reports an error. The instance then refuses its action as
// unavailable and logs the error with its slug, and a later attempt asks
// again. An action that needs a scratch file is never served from the
// system's temporary directory.
func (s *Service) sheetScratchNow() (string, error) {
	st := &s.sheetScratch
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return "", errSheetScratchClosed
	}
	if dir := st.current(); dir != "" {
		return dir, nil
	}
	owner, err := openSheetScratch(s.sheetScratchRoot(), s.logf, false)
	if errors.Is(err, filelock.ErrLocked) {
		return "", errSheetScratchBusy
	}
	if err != nil {
		return "", err
	}
	st.owner = owner
	return owner.dir, nil
}

// closeSheetScratch removes this host's scratch directory. Every instance
// has stopped and removed its own files by then.
func (s *Service) closeSheetScratch() {
	st := &s.sheetScratch
	st.mu.Lock()
	owners := st.gone
	if st.owner != nil {
		owners = append(owners, st.owner)
	}
	st.owner, st.gone, st.closed = nil, nil, true
	st.mu.Unlock()
	for _, owner := range owners {
		if err := owner.close(); err != nil {
			s.logf("sheet scratch cleanup: %v", err)
		}
	}
}
