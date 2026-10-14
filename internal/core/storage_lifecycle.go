package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

// A journal is durable before either the filesystem or SQLite is changed.
// Startup settles interrupted operations before it starts a flat's runtime.
type storageOperation struct {
	Kind string
	From string
	To   string
}

func (op storageOperation) validate() error {
	if err := slug.Validate(op.From); err != nil {
		return err
	}
	switch op.Kind {
	case "rename":
		if err := slug.Validate(op.To); err != nil {
			return err
		}
		if op.From == op.To {
			return fmt.Errorf("rename journal has identical slugs %q", op.From)
		}
	case "delete":
		if op.To != "" {
			return fmt.Errorf("delete journal has a destination")
		}
	default:
		return fmt.Errorf("unknown storage operation %q", op.Kind)
	}
	return nil
}

func (op storageOperation) fileName() string {
	if op.Kind == "rename" {
		return "rename." + op.From + "." + op.To + ".json"
	}
	return "delete." + op.From + ".json"
}

func (s *Service) storageJournalDir() string {
	return filepath.Join(s.cfg.DataDir, "flats", ".lifecycle")
}

func (s *Service) deletedStorageRoot() string {
	return filepath.Join(s.storageJournalDir(), "deleted")
}

func (s *Service) orphanStorageRoot() string {
	return filepath.Join(s.storageJournalDir(), "orphans")
}

func (s *Service) deletedStorageDir(slugName string) string {
	return filepath.Join(s.deletedStorageRoot(), slugName)
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path) // A dangling symlink also owns its path.
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Service) storageOperations() ([]storageOperation, error) {
	entries, err := os.ReadDir(s.storageJournalDir())
	if err != nil {
		return nil, err
	}
	var operations []storageOperation
	for _, entry := range entries {
		if entry.Name() == "deleted" || entry.Name() == "orphans" {
			if !entry.IsDir() {
				return nil, fmt.Errorf("storage recovery path %q is not a directory", entry.Name())
			}
			continue
		}
		// atomicJournal writes .tmp first. No filesystem operation can
		// start until the completed .json journal has been synced, so a .tmp
		// file is an abandoned write. Other names (for example a desktop
		// index file) are not journals either and must not stop the host.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.storageJournalDir(), entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			// An operation on other slugs finished during this scan. A
			// journal naming the caller's slug cannot disappear here: it
			// needs that slug's lock, which the caller holds.
			continue
		}
		if err != nil {
			return nil, err
		}
		var op storageOperation
		if err := json.Unmarshal(raw, &op); err != nil {
			return nil, fmt.Errorf("read storage journal %q: %w", entry.Name(), err)
		}
		if err := op.validate(); err != nil {
			return nil, fmt.Errorf("invalid storage journal %q: %w", entry.Name(), err)
		}
		if entry.Name() != op.fileName() {
			return nil, fmt.Errorf("storage journal %q does not match its contents", entry.Name())
		}
		operations = append(operations, op)
	}
	return operations, nil
}

func (s *Service) checkStorageJournal(slugName string) error {
	operations, err := s.storageOperations()
	if err != nil {
		return err
	}
	for _, op := range operations {
		if op.From == slugName || op.To == slugName {
			return withKind(ErrConflict, fmt.Errorf("flat %q has unfinished %s storage cleanup", slugName, op.Kind))
		}
	}
	return nil
}

// A database-free slug may still have a previous owner's files on disk.
// Never adopt that directory (or a symlink) as a newly created flat.
func (s *Service) requireUnusedStorage(slugName string) error {
	if err := s.checkStorageJournal(slugName); err != nil {
		return err
	}
	exists, err := pathExists(s.flatDir(slugName))
	if err != nil {
		return err
	}
	if exists {
		return withKind(ErrConflict, fmt.Errorf("flat %q has unowned storage on disk", slugName))
	}
	return nil
}

func (s *Service) beginStorageOperation(op storageOperation) error {
	if err := op.validate(); err != nil {
		return err
	}
	if err := s.checkStorageJournal(op.From); err != nil {
		return err
	}
	if op.To != "" {
		if err := s.checkStorageJournal(op.To); err != nil {
			return err
		}
	} else {
		exists, err := pathExists(s.deletedStorageDir(op.From))
		if err != nil {
			return err
		}
		if exists {
			return withKind(ErrConflict, fmt.Errorf("flat %q has unfinished deleted storage", op.From))
		}
	}
	b, err := json.Marshal(op)
	if err != nil {
		return err
	}
	if err := atomicJournal(filepath.Join(s.storageJournalDir(), op.fileName()), b); err != nil {
		return err
	}
	return syncDirectory(s.storageJournalDir())
}

func (s *Service) finishStorageOperation(op storageOperation) error {
	if err := os.Remove(filepath.Join(s.storageJournalDir(), op.fileName())); err != nil {
		return err
	}
	return syncDirectory(s.storageJournalDir())
}

func (s *Service) storageOwner(ctx context.Context, slugName string) (bool, error) {
	_, err := s.st.GetFlat(ctx, slugName)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) recoverStorageRename(ctx context.Context, op storageOperation) error {
	fromOwned, err := s.storageOwner(ctx, op.From)
	if err != nil {
		return err
	}
	toOwned, err := s.storageOwner(ctx, op.To)
	if err != nil {
		return err
	}
	if !fromOwned && !toOwned {
		// The flat is gone (for example its metadata was removed by hand).
		// Nothing may run on either path; the quarantine below moves any
		// remaining directory aside instead of assigning it to a new flat.
		return s.finishStorageOperation(op)
	}
	if fromOwned && toOwned {
		return fmt.Errorf("cannot recover rename %s -> %s: both slugs have a flat in flats.db; resolve it by hand, then restart", op.From, op.To)
	}
	fromExists, err := pathExists(s.flatDir(op.From))
	if err != nil {
		return err
	}
	toExists, err := pathExists(s.flatDir(op.To))
	if err != nil {
		return err
	}
	if fromExists && toExists {
		// Never guess which directory holds the flat's data.
		return fmt.Errorf("cannot recover rename %s -> %s: both %q and %q exist; move the one that does not belong to the flat out of %q, then restart",
			op.From, op.To, s.flatDir(op.From), s.flatDir(op.To), filepath.Dir(s.flatDir(op.From)))
	}
	switch {
	case fromOwned && toExists:
		err = os.Rename(s.flatDir(op.To), s.flatDir(op.From))
	case toOwned && fromExists:
		err = os.Rename(s.flatDir(op.From), s.flatDir(op.To))
	}
	if err != nil {
		return err
	}
	if (fromOwned && toExists) || (toOwned && fromExists) {
		if err := syncDirectory(filepath.Join(s.cfg.DataDir, "flats")); err != nil {
			return err
		}
		owner := op.From
		if toOwned {
			owner = op.To
		}
		s.logf("recovered interrupted rename %s -> %s: storage now belongs to %s", op.From, op.To, owner)
	}
	return s.finishStorageOperation(op)
}

func (s *Service) recoverStorageDelete(ctx context.Context, op storageOperation) error {
	owned, err := s.storageOwner(ctx, op.From)
	if err != nil {
		return err
	}
	source := s.flatDir(op.From)
	trash := s.deletedStorageDir(op.From)
	sourceExists, err := pathExists(source)
	if err != nil {
		return err
	}
	trashExists, err := pathExists(trash)
	if err != nil {
		return err
	}
	if sourceExists && trashExists {
		if !owned {
			// Leave both for the operator; the journal keeps the slug
			// reserved and startup quarantines the unowned active path.
			s.logf("delete %s: both %q and %q exist; keeping both for inspection", op.From, source, trash)
			return nil
		}
		return fmt.Errorf("cannot recover delete %s: both active and deleted storage exist", op.From)
	}
	if owned && trashExists {
		// A database commit lost during a crash still owns its original
		// files. Restore them before starting the corresponding runtime.
		if err := os.Rename(trash, source); err != nil {
			return err
		}
		// Persist the restored entry before the removal from the trash, so
		// a crash between the syncs cannot leave the files at neither path.
		if err := syncDirectory(filepath.Join(s.cfg.DataDir, "flats")); err != nil {
			return err
		}
		if err := syncDirectory(s.deletedStorageRoot()); err != nil {
			return err
		}
		s.logf("recovered interrupted delete of %s: the flat still exists, so its storage was restored", op.From)
	}
	if !owned {
		// The deletion committed: no runtime starts for this slug, and the
		// kept journal blocks its reuse. A cleanup failure must therefore not
		// stop the whole host; the next startup retries it.
		if err := s.removeDeletedStorage(op, sourceExists && !trashExists); err != nil {
			s.logf("delete %s: storage cleanup is still incomplete; the slug stays reserved until a later startup finishes it: %v", op.From, err)
		}
		return nil
	}
	return s.finishStorageOperation(op)
}

// removeDeletedStorage moves a deleted flat's directory out of its reusable
// path (when move is set), removes it, and then drops the journal.
func (s *Service) removeDeletedStorage(op storageOperation, move bool) error {
	source, trash := s.flatDir(op.From), s.deletedStorageDir(op.From)
	if move {
		if err := os.Rename(source, trash); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Join(s.cfg.DataDir, "flats")); err != nil {
			return err
		}
		if err := syncDirectory(s.deletedStorageRoot()); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(trash); err != nil {
		return err
	}
	if err := syncDirectory(s.deletedStorageRoot()); err != nil {
		return err
	}
	return s.finishStorageOperation(op)
}

// discardDeletedStorage runs only after the metadata deletion commits and
// all serving runtimes have stopped. On failure its journal stays in place,
// preventing slug reuse until the next successful recovery.
func (s *Service) discardDeletedStorage(ctx context.Context, op storageOperation) error {
	// Remove nothing until the deletion is durable: after a power loss the
	// database could otherwise bring back a flat whose files are gone.
	if err := s.st.Sync(ctx); err != nil {
		return err
	}
	source, trash := s.flatDir(op.From), s.deletedStorageDir(op.From)
	trashExists, err := pathExists(trash)
	if err != nil {
		return err
	}
	if trashExists {
		return fmt.Errorf("deleted storage destination %q already exists", trash)
	}
	sourceExists, err := pathExists(source)
	if err != nil {
		return err
	}
	return s.removeDeletedStorage(op, sourceExists)
}

func (s *Service) quarantineUnownedStorage(ctx context.Context) error {
	flats, err := s.st.ListFlats(ctx)
	if err != nil {
		return err
	}
	owned := make(map[string]bool, len(flats))
	for _, f := range flats {
		owned[f.Slug] = true
	}
	// A delete whose files could not be moved to the trash yet still owns
	// its active path: quarantining it would let the next startup release
	// the journal while the deleted files stay in orphans. Its journal keeps
	// the slug reserved until a later startup removes the files. With a
	// trash copy as well, the active path is ambiguous and is quarantined.
	operations, err := s.storageOperations()
	if err != nil {
		return err
	}
	pendingDelete := make(map[string]bool)
	for _, op := range operations {
		if op.Kind != "delete" {
			continue
		}
		trashExists, err := pathExists(s.deletedStorageDir(op.From))
		if err != nil {
			return err
		}
		if !trashExists {
			pendingDelete[op.From] = true
		}
	}
	root := filepath.Join(s.cfg.DataDir, "flats")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	orphans := s.orphanStorageRoot()
	for _, entry := range entries {
		name := entry.Name()
		if slug.Validate(name) != nil || owned[name] || pendingDelete[name] {
			continue
		}
		// A failure leaves the path in place: creation and rename still
		// refuse an occupied path, so it is never adopted. Do not stop the
		// host over it.
		target, err := s.quarantineOrphan(root, orphans, name)
		if err != nil {
			s.logf("could not quarantine unowned flat storage %q; the slug stays unavailable until it is moved away: %v", name, err)
			continue
		}
		s.logf("quarantined unowned flat storage %q at %q", name, target)
	}
	return nil
}

// quarantineOrphan moves root/name below orphans without replacing an
// earlier quarantined directory of the same name. recoverStorageOperations
// has already created orphans and synced its parent.
func (s *Service) quarantineOrphan(root, orphans, name string) (string, error) {
	for i := 0; i < 1000; i++ {
		target := filepath.Join(orphans, name)
		if i > 0 {
			target = fmt.Sprintf("%s.%d", target, i)
		}
		exists, err := pathExists(target)
		if err != nil {
			return "", err
		}
		if exists {
			continue
		}
		if err := os.Rename(filepath.Join(root, name), target); err != nil {
			return "", err
		}
		// Persist the quarantined entry before the removal from root, so a
		// crash between the syncs cannot leave the files at neither path.
		if err := syncDirectory(orphans); err != nil {
			return target, err
		}
		return target, syncDirectory(root)
	}
	return "", fmt.Errorf("too many quarantined copies of %q", name)
}

// recoverStorageOperations runs before draft migrations, runtime restore,
// previews, and redirects. Unknown legacy directories are quarantined; they
// are never implicitly assigned to a newly created flat.
func (s *Service) recoverStorageOperations(ctx context.Context) error {
	// Create both recovery directories before the syncs below make their
	// entries durable; storage is only moved into a durable directory.
	if err := os.MkdirAll(s.deletedStorageRoot(), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.orphanStorageRoot(), 0700); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Join(s.cfg.DataDir, "flats")); err != nil {
		return err
	}
	if err := syncDirectory(s.storageJournalDir()); err != nil {
		return err
	}
	operations, err := s.storageOperations()
	if err != nil {
		return err
	}
	if len(operations) > 0 {
		// Decide from metadata that is durable, not from commits still
		// only in the write-ahead log.
		if err := s.st.Sync(ctx); err != nil {
			return err
		}
	}
	for _, op := range operations {
		switch op.Kind {
		case "rename":
			err = s.recoverStorageRename(ctx, op)
		case "delete":
			err = s.recoverStorageDelete(ctx, op)
		}
		if err != nil {
			return fmt.Errorf("recover %s storage for %q: %w", op.Kind, op.From, err)
		}
	}
	return s.quarantineUnownedStorage(ctx)
}
