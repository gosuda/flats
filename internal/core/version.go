package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/config"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

var ErrVersionQuota = errors.New("version quota reached")

// ErrSnapshotFailed means deployment stopped before touching live data.
var ErrSnapshotFailed = errors.New("data snapshot failed before deployment")

func (s *Service) versionQuota(f store.Flat) int {
	if f.VersionQuota != nil {
		return *f.VersionQuota
	}
	return clampInt(s.intSetting(SetVersionQuota))
}

func (s *Service) checkVersionQuota(ctx context.Context, f store.Flat) error {
	quota := s.versionQuota(f)
	if quota == 0 {
		return nil
	}
	count, err := s.st.VersionCount(ctx, f.Slug)
	if err != nil {
		return err
	}
	if count >= quota {
		return fmt.Errorf("%w: %w: this flat holds %d versions and its quota is %d; delete older versions or raise the version quota", ErrConflict, ErrVersionQuota, count, quota)
	}
	return nil
}

func (s *Service) checkVersionDiskQuota(flat string, extra int64) error {
	quota := s.diskQuota()
	if quota == 0 {
		return nil
	}
	used := dirSize(s.flatDir(flat))
	if used > quota || extra > quota-used {
		return fmt.Errorf("%w: %w: creating a version would exceed the flat's %d-byte disk quota; delete older versions or raise the disk quota", ErrConflict, ErrDiskQuota, quota)
	}
	return nil
}

// SetVersionQuotaOverride changes only the count limit, retaining all versions.
// A nil override follows the host default; zero is unlimited.
func (s *Service) SetVersionQuotaOverride(ctx context.Context, flat string, quota *int) error {
	if quota != nil && (*quota < 0 || int64(*quota) > config.MaxInt) {
		return invalidf("version quota must be an integer from 0 to %d", config.MaxInt)
	}
	unlock, err := s.lockStorageCtx(ctx, flat)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.st.GetFlat(ctx, flat)
	if err != nil {
		return err
	}
	if err := s.refuseUnversioned(ctx, f); err != nil {
		return err
	}
	return s.st.SetVersionQuota(ctx, flat, quota)
}

type deleteVersionParams struct {
	Version      int    `json:"version"`
	Hash         string `json:"hash"`
	ExpectedLive int    `json:"expected_live"`
	Visibility   string `json:"visibility"`
	Providers    string `json:"providers"`
}

// DeleteVersion is direct for the console and otherwise requests approval.
func (s *Service) DeleteVersion(ctx context.Context, flat string, number int, via Via) (ActionResult, error) {
	unlock, err := s.lockStorageCtx(ctx, flat)
	if err != nil {
		return ActionResult{}, err
	}
	defer unlock()
	f, err := s.st.GetFlat(ctx, flat)
	if err != nil {
		return ActionResult{}, err
	}
	if err := s.refuseUnversioned(ctx, f); err != nil {
		return ActionResult{}, err
	}
	if number <= 0 {
		return ActionResult{}, invalidf("version must be a positive integer")
	}
	v, err := s.st.GetVersion(ctx, flat, number)
	if err != nil {
		return ActionResult{}, err
	}
	if number == f.LiveVersion {
		return ActionResult{}, fmt.Errorf("%w: the live version cannot be deleted", ErrConflict)
	}
	if via == ViaConsole {
		return s.deleteVersionLocked(ctx, f, v, "")
	}
	providers, err := s.providerKey(ctx, flat)
	if err != nil {
		return ActionResult{}, err
	}
	params := deleteVersionParams{Version: number, Hash: v.Hash, ExpectedLive: f.LiveVersion,
		Visibility: string(f.Visibility.Canonical()), Providers: providers}
	return s.requestFrozen(ctx, flat, "delete_version", params, via, "")
}

func (s *Service) RequestDeleteVersion(ctx context.Context, flat string, number int, via Via) (ActionResult, error) {
	return s.DeleteVersion(ctx, flat, number, via)
}

func (s *Service) applyDeleteVersionApproval(ctx context.Context, a store.Approval) (ActionResult, error) {
	var p deleteVersionParams
	if err := json.Unmarshal(a.Params, &p); err != nil {
		return ActionResult{}, err
	}
	unlock, err := s.lockStorageCtx(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	defer unlock()
	f, err := s.st.GetFlat(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if err := s.refuseUnversioned(ctx, f); err != nil {
		return ActionResult{}, err
	}
	if f.LiveVersion != p.ExpectedLive {
		return ActionResult{}, mismatch(strconv.Itoa(p.ExpectedLive), strconv.Itoa(f.LiveVersion), "live version")
	}
	if string(f.Visibility.Canonical()) != p.Visibility {
		return ActionResult{}, mismatch(p.Visibility, string(f.Visibility.Canonical()), "visibility")
	}
	providers, err := s.providerKey(ctx, a.Flat)
	if err != nil {
		return ActionResult{}, err
	}
	if providers != p.Providers {
		return ActionResult{}, mismatch(p.Providers, providers, "provider permissions")
	}
	v, err := s.st.GetVersion(ctx, a.Flat, p.Version)
	if err != nil {
		return ActionResult{}, err
	}
	if v.Hash != p.Hash {
		return ActionResult{}, mismatch(p.Hash, v.Hash, "version hash")
	}
	if f.LiveVersion == v.Number {
		return ActionResult{}, fmt.Errorf("%w: the live version cannot be deleted", ErrConflict)
	}
	return s.deleteVersionLocked(ctx, f, v, a.ID)
}

const versionDeletePrefix = ".version-delete-"

func (s *Service) deleteVersionLocked(ctx context.Context, f store.Flat, v store.Version, approval string) (ActionResult, error) {
	if err := slug.Validate(f.Slug); err != nil {
		return ActionResult{}, invalid(err)
	}
	if strings.Contains(f.Slug, "..") || strings.ContainsAny(f.Slug, `/\`) || v.Number <= 0 {
		return ActionResult{}, invalidf("invalid version deletion target")
	}

	ps, err := s.st.ListPreviews(ctx, f.Slug)
	if err != nil {
		return ActionResult{}, err
	}
	for _, p := range ps {
		if p.Target != "draft" && p.Version == v.Number {
			if err := s.ClosePreview(ctx, p.Host); err != nil {
				return ActionResult{}, fmt.Errorf("close version preview: %w", err)
			}
		}
	}
	dir := s.versionDir(f.Slug, v.Number)
	staged := filepath.Join(s.flatDir(f.Slug), versionDeletePrefix+strconv.Itoa(v.Number))
	moved := false
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		return ActionResult{}, fmt.Errorf("unfinished version deletion at %s; restart Flats to recover it", staged)
	}
	if err := os.Rename(dir, staged); err != nil {
		if !os.IsNotExist(err) {
			return ActionResult{}, err
		}
	} else {
		moved = true
	}
	if moved {
		if err := errors.Join(syncDirectory(filepath.Dir(dir)), syncDirectory(s.flatDir(f.Slug))); err != nil {
			return ActionResult{}, errors.Join(err, os.Rename(staged, dir))
		}
	}
	raw, _ := json.Marshal(ApprovalExecution{Status: "applied", CleanupPending: true, Version: v.Number, DataImpact: "none", HealthData: "not_run", LiveData: "untouched"})
	if err := s.st.CommitVersionDeletion(ctx, f.Slug, v.Number, approval, raw, s.now()); err != nil {
		if moved {
			err = errors.Join(err, os.Rename(staged, dir))
		}
		return ActionResult{}, err
	}
	// NORMAL-mode commits may be lost until checkpointed. Keep the recoverable
	// staged copy until the deletion and applied approval receipt are durable.
	cleanupErr := s.st.Sync(context.WithoutCancel(ctx))
	if cleanupErr == nil && moved {
		cleanupErr = os.RemoveAll(staged)
	}
	if cleanupErr == nil && moved {
		cleanupErr = syncDirectory(s.flatDir(f.Slug))
	}
	status, message := "done", fmt.Sprintf("deleted version %d", v.Number)
	pending := cleanupErr != nil
	if pending {
		status = "cleanup_pending"
		message = fmt.Sprintf("version %d was deleted from metadata; private storage cleanup is pending. Restart Flats before saving, creating, deploying, or previewing this flat; do not retry the deletion", v.Number)
		s.logf("deleted version %s/%d: storage cleanup pending: %v", f.Slug, v.Number, cleanupErr)
	}
	s.Event(ctx, f.Slug, "info", "version", message, nil)
	current, err := s.st.GetFlat(ctx, f.Slug)
	if err != nil {
		current = f
	}
	view := s.view(ctx, current)
	return ActionResult{Status: status, CleanupPending: pending, Flat: &view, Version: v.Number, Message: message}, nil
}

// Reconcile an interruption between moving files and committing the deletion.
// A retained row restores its files; an absent row authorizes cleanup only.
func (s *Service) recoverVersionDeletions(ctx context.Context) error {
	flats, err := s.st.ListFlats(ctx)
	if err != nil {
		return err
	}
	synced := false
	for _, f := range flats {
		entries, err := os.ReadDir(s.flatDir(f.Slug))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), versionDeletePrefix) {
				continue
			}
			if err := slug.Validate(f.Slug); err != nil {
				return invalid(err)
			}
			if strings.Contains(f.Slug, "..") || strings.ContainsAny(f.Slug, `/\`) {
				return invalidf("invalid version recovery slug")
			}
			if !synced {
				if err := s.st.Sync(ctx); err != nil {
					return err
				}
				synced = true
			}
			n, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), versionDeletePrefix))
			if err != nil || n <= 0 || entry.Name() != versionDeletePrefix+strconv.Itoa(n) || !entry.IsDir() {
				return fmt.Errorf("invalid version deletion staging path %q", entry.Name())
			}
			staged := filepath.Join(s.flatDir(f.Slug), entry.Name())
			_, err = s.st.GetVersion(ctx, f.Slug, n)
			if err == nil {
				dir := s.versionDir(f.Slug, n)
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					return fmt.Errorf("cannot restore interrupted version deletion: target %s exists or cannot be inspected", dir)
				}
				if err := os.Rename(staged, dir); err != nil {
					return err
				}
				if err := errors.Join(syncDirectory(filepath.Dir(dir)), syncDirectory(s.flatDir(f.Slug))); err != nil {
					return err
				}
			} else if errors.Is(err, store.ErrNotFound) {
				if err := os.RemoveAll(staged); err != nil {
					return err
				}
				if err := syncDirectory(s.flatDir(f.Slug)); err != nil {
					return err
				}
			} else {
				return err
			}
		}
	}
	approvals, err := s.st.ListPendingVersionDeletions(ctx)
	if err != nil {
		return err
	}
	settled := false
	for _, a := range approvals {
		if a.Action != "delete_version" {
			continue
		}
		var receipt ApprovalExecution
		if err := json.Unmarshal(a.ResultData, &receipt); err != nil || !receipt.CleanupPending {
			continue
		}
		if !synced {
			if err := s.st.Sync(ctx); err != nil {
				return err
			}
			synced = true
		}
		if receipt.Version <= 0 {
			return fmt.Errorf("invalid cleanup-pending deletion receipt %s", a.ID)
		}
		// An approved receipt follows the atomic deletion commit in SQLite's
		// ordered WAL. Once synced, it proves that original deletion survived.
		// The flat may since have been renamed or deleted and its name reused;
		// a current row or matching content hash cannot identify the old version.
		// Staged directories were reconciled above. Never inspect or remove fresh
		// files through this historical receipt's reusable slug/version identity.
		receipt.CleanupPending = false
		raw, _ := json.Marshal(receipt)
		if err := s.st.SettleVersionDeletion(ctx, a.ID, a.ResultData, raw, fmt.Sprintf("deleted version %d", receipt.Version)); err != nil {
			return err
		}
		settled = true
	}
	if settled {
		return s.st.Sync(ctx)
	}
	return nil
}
