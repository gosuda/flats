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
		// start until the completed .json journal has been synced.
		if strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, fmt.Errorf("unexpected storage recovery entry %q", entry.Name())
		}
		raw, err := os.ReadFile(filepath.Join(s.storageJournalDir(), entry.Name()))
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
	if fromOwned == toOwned {
		return fmt.Errorf("cannot recover rename %s -> %s: ambiguous database owner", op.From, op.To)
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
		return fmt.Errorf("cannot recover rename %s -> %s: both directories exist", op.From, op.To)
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
		return fmt.Errorf("cannot recover delete %s: both active and deleted storage exist", op.From)
	}
	if owned && trashExists {
		// A database commit lost during a crash still owns its original
		// files. Restore them before starting the corresponding runtime.
		if err := os.Rename(trash, source); err != nil {
			return err
		}
		if err := syncDirectory(s.deletedStorageRoot()); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Join(s.cfg.DataDir, "flats")); err != nil {
			return err
		}
	}
	if !owned {
		if sourceExists {
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
	}
	return s.finishStorageOperation(op)
}

// discardDeletedStorage runs only after the metadata deletion commits and
// all serving runtimes have stopped. On failure its journal stays in place,
// preventing slug reuse until the next successful recovery.
func (s *Service) discardDeletedStorage(op storageOperation) error {
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
	if sourceExists {
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

func (s *Service) quarantineUnownedStorage(ctx context.Context) error {
	flats, err := s.st.ListFlats(ctx)
	if err != nil {
		return err
	}
	owned := make(map[string]bool, len(flats))
	for _, f := range flats {
		owned[f.Slug] = true
	}
	root := filepath.Join(s.cfg.DataDir, "flats")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	orphans := filepath.Join(s.storageJournalDir(), "orphans")
	for _, entry := range entries {
		name := entry.Name()
		if slug.Validate(name) != nil || owned[name] {
			continue
		}
		if err := os.MkdirAll(orphans, 0700); err != nil {
			return err
		}
		target := filepath.Join(orphans, name)
		exists, err := pathExists(target)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("cannot quarantine unowned storage %q: destination already exists", name)
		}
		if err := os.Rename(filepath.Join(root, name), target); err != nil {
			return err
		}
		if err := syncDirectory(root); err != nil {
			return err
		}
		if err := syncDirectory(orphans); err != nil {
			return err
		}
		s.logf("quarantined unowned flat storage %q at %q", name, target)
	}
	return nil
}

// recoverStorageOperations runs before draft migrations, runtime restore,
// previews, and redirects. Unknown legacy directories are quarantined; they
// are never implicitly assigned to a newly created flat.
func (s *Service) recoverStorageOperations(ctx context.Context) error {
	if err := os.MkdirAll(s.deletedStorageRoot(), 0700); err != nil {
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
