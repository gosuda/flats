package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/store"
)

// ErrTypeChange refuses content whose type differs from the flat's: a slug
// keeps the type it was first saved with.
var ErrTypeChange = fmt.Errorf("%w: a flat cannot change its content type (website, page or document)", ErrConflict)

// flatType is the content type of a flat: that of its live version, or,
// while nothing runs, that of its Draft. It is "" for a flat that has never
// been saved.
func (s *Service) flatType(ctx context.Context, f store.Flat) (string, error) {
	if f.LiveVersion > 0 {
		v, err := s.st.GetVersion(ctx, f.Slug, f.LiveVersion)
		if err != nil {
			return "", err
		}
		return contenttype.FromManifest(v.Manifest), nil
	}
	d, err := s.st.GetDraft(ctx, f.Slug)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return contenttype.FromManifest(d.Manifest), nil
}

// checkTypeChange refuses a save of type typ into a flat that already has
// another type.
func (s *Service) checkTypeChange(ctx context.Context, f store.Flat, typ string) error {
	cur, err := s.flatType(ctx, f)
	if err != nil || cur == "" || cur == typ {
		return err
	}
	return typeChangeError(f.Slug, cur, typ, "this save")
}

// checkCandidateType refuses publishing or activating content whose type
// differs from the running version's. Earlier releases could leave such
// content behind (a docs Draft over a running website); a slug keeps its
// type, so it never goes live.
func (s *Service) checkCandidateType(ctx context.Context, f store.Flat, manifest json.RawMessage, what string) error {
	if f.LiveVersion == 0 {
		return nil
	}
	v, err := s.st.GetVersion(ctx, f.Slug, f.LiveVersion)
	if err != nil {
		return err
	}
	cur, typ := contenttype.FromManifest(v.Manifest), contenttype.FromManifest(manifest)
	if cur == typ {
		return nil
	}
	return typeChangeError(f.Slug, cur, typ, what)
}

func typeChangeError(slugName, cur, typ, what string) error {
	fix := "use another slug"
	if cur == contenttype.Page {
		fix = `add flats.json {"type":"page"} (or use save_page) to update it, or use another slug`
	}
	return fmt.Errorf("%w: flat %s is a %s and %s is a %s; %s", ErrTypeChange, slugName, typeNoun(cur), what, typeNoun(typ), fix)
}

// typeNoun names a content type in messages.
func typeNoun(t string) string {
	switch t {
	case contenttype.Docs:
		return "document"
	case contenttype.Page:
		return "page"
	}
	return "website"
}
