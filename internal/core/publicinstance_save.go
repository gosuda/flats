package core

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/gosuda/flats/internal/store"
)

// SavedDraftError reports a saved Draft whose automatic publication failed.
// Cause retains the deployment or quota error for transport classification.
type SavedDraftError struct {
	Revision int
	Cause    error
}

func (e *SavedDraftError) Error() string {
	return fmt.Sprintf("Draft revision %d was saved but not published; the Draft was kept: %v", e.Revision, e.Cause)
}
func (e *SavedDraftError) Unwrap() error { return e.Cause }

// The caller holds the flat lock from saving through activation. The version
// row is committed only by activation, so failed checks leave no version behind.
func (s *Service) publishPublicInstanceSaveLocked(ctx context.Context, flat string, draft store.Version) (store.Version, error) {
	f, err := s.st.GetFlat(ctx, flat)
	if err != nil {
		return draft, &SavedDraftError{Revision: draft.Revision, Cause: err}
	}
	if !s.PublicInstance().Enabled || !f.CreatedPublicInstance || !f.Visibility.Public() {
		return draft, nil
	}
	published, err := s.publishSavedDraftLocked(ctx, f, draft)
	if err != nil {
		return draft, &SavedDraftError{Revision: draft.Revision, Cause: err}
	}
	return published, nil
}

func (s *Service) publishSavedDraftLocked(ctx context.Context, f store.Flat, draft store.Version) (store.Version, error) {
	if err := s.checkVersionQuota(ctx, f); err != nil {
		return draft, err
	}
	if err := s.checkVersionDiskQuota(f.Slug, draft.Size); err != nil {
		return draft, err
	}
	n, err := s.st.NextVersionNumber(ctx, f.Slug)
	if err != nil {
		return draft, err
	}
	dir := s.versionDir(f.Slug, n)
	if err := os.RemoveAll(dir); err != nil {
		return draft, err
	}
	if err := copyTree(s.draftRevDir(f.Slug, draft.Revision), dir); err != nil {
		os.RemoveAll(dir)
		return draft, err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(dir)
		}
	}()
	if err := s.checkVersionQuota(ctx, f); err != nil {
		return draft, err
	}
	if err := s.checkVersionDiskQuota(f.Slug, 0); err != nil {
		return draft, err
	}
	v := draft
	v.Number, v.Published, v.Role, v.CreatedAt = n, true, "published", s.now()
	_, err = s.deployVersionLocked(ctx, f, v, "public-instance", ViaPublicInstance, "")
	var de *DeployError
	committed = err == nil || (errors.As(err, &de) && de.Committed)
	if err != nil {
		return draft, err
	}
	return v, nil
}
