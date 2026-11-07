package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/imagecheck"
	"github.com/gosuda/flats/internal/store"
)

// docsImageNameDigits is how much of an image's SHA-256 names its file.
const docsImageNameDigits = 16

// docsImageExtensions maps a format imagecheck detected to its file extension.
var docsImageExtensions = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// DocumentImage is an image of a document's bundle, added or already there.
type DocumentImage struct {
	// Reference is its path relative to the document, for Markdown.
	Reference string `json:"reference"`
	// Path is its path in the bundle.
	Path string `json:"path"`
	// Added is false when the bundle already held these bytes there.
	Added bool `json:"added"`
}

// AddDocumentImage stores image bytes beside a running document, as a save of
// its bundle: sibling files and live text are kept, the new version starts at
// once and label is credited with its revision. Bytes the bundle already holds
// at that path change nothing.
func (s *Service) AddDocumentImage(ctx context.Context, slugName, doc string, data []byte, label string) (DocumentImage, error) {
	if !fs.ValidPath(doc) || !bundle.IsMarkdown(doc) {
		return DocumentImage{}, invalidf("doc must be a relative .md or .markdown path")
	}
	info, err := imagecheck.CheckImage(data, s.UploadLimit())
	if err != nil {
		return DocumentImage{}, invalid(err)
	}
	ext, ok := docsImageExtensions[info.Mime]
	if !ok {
		return DocumentImage{}, invalidf("the image cannot be accepted: %s is not a supported format", info.Mime)
	}
	reference := "images/" + info.Hash[:docsImageNameDigits] + ext
	image := path.Join(path.Dir(doc), reference)
	out := DocumentImage{Reference: reference, Path: image}
	unlock, err := s.lockStorage(slugName)
	if err != nil {
		return DocumentImage{}, err
	}
	defer unlock()
	f, err := s.st.GetFlat(ctx, slugName)
	if err != nil {
		return DocumentImage{}, err
	}
	live := s.state(slugName).cur.Load()
	if live == nil {
		return DocumentImage{}, fmt.Errorf("%w: the document is not running", ErrNotDeployed)
	}
	if contenttype.FromManifest(live.version.Manifest) != contenttype.Docs {
		return DocumentImage{}, ErrNotDocs
	}
	files, _, err := s.docsSourceLocked(ctx, slugName, f)
	if err != nil {
		return DocumentImage{}, err
	}
	found, held := false, false
	for _, file := range files {
		switch {
		case file.Path == doc:
			found = true
		case file.Path == image:
			if !bytes.Equal(file.Data, data) {
				return DocumentImage{}, fmt.Errorf("%w: a different file already holds %s", ErrConflict, image)
			}
			held = true
		case strings.HasPrefix(image, file.Path+"/") || strings.HasPrefix(file.Path, image+"/"):
			return DocumentImage{}, fmt.Errorf("%w: the file %s is in the way of %s", ErrConflict, file.Path, image)
		}
	}
	if !found {
		return DocumentImage{}, ErrDocumentNotFound
	}
	if held {
		return out, nil
	}
	files = append(files, bundle.File{Path: image, Data: data})
	_, err = s.saveVersionLocked(ctx, slugName, files, SaveMeta{Message: "added image " + image, label: label}, ViaConsole)
	// A committed failure still added the image: the host serves it, and a
	// later request retries preparing the document.
	var deploy *DeployError
	if err != nil && (!errors.As(err, &deploy) || !deploy.Committed) {
		return DocumentImage{}, err
	}
	out.Added = true
	return out, nil
}

// docsImageRoute adds a pasted image for the document's own Private page
// (docs/spec/docs-image-paste.md). Everything else goes to next. A Public
// route gets 404.
func (s *Service) docsImageRoute(slugName string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_docs/api/images" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Header.Get("X-Flats-Access") != "private" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			docsRevisionError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
			return
		}
		if !docsPageRequest(w, r) {
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.UploadLimit()))
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			docsRevisionError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("The image is larger than this host's upload limit of %d bytes. The operator can raise upload_max_bytes.", tooLarge.Limit))
			return
		}
		if err != nil {
			docsRevisionError(w, http.StatusBadRequest, "invalid", "the image could not be read")
			return
		}
		q := r.URL.Query()
		image, err := s.AddDocumentImage(r.Context(), slugName, q.Get("doc"), data, docsPersonLabel(r, q.Get("name")))
		if err != nil {
			docsImageError(w, err)
			return
		}
		docsRevisionReply(w, image, nil)
	})
}

// docsImageError answers a refused image in words the page can show.
func docsImageError(w http.ResponseWriter, err error) {
	var deploy *DeployError
	verr, isValidation := bundle.IsValidation(err)
	switch {
	case isValidation:
		for _, p := range verr.Problems {
			switch {
			case strings.Contains(p.Message, "upload is larger than"):
				docsRevisionError(w, http.StatusRequestEntityTooLarge, "bundle_full", "This image would put the document's files over this host's upload limit. The operator can raise upload_max_bytes.")
				return
			case strings.Contains(p.Message, "disk quota"):
				docsRevisionError(w, http.StatusInsufficientStorage, "disk_quota", "This image would put the document over its disk quota. The operator can raise the quota in system settings.")
				return
			}
		}
		docsRevisionError(w, http.StatusBadRequest, "invalid", verr.Error())
	case errors.As(err, &deploy):
		docsRevisionError(w, http.StatusServiceUnavailable, "unavailable", "The document could not start with this image, so it keeps running without it.")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, ErrNotDeployed):
		docsRevisionError(w, http.StatusNotFound, "not_found", "This document is not available any more.")
	default:
		docsRevisionReply(w, nil, err)
	}
}
