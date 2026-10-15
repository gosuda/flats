package core

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/store"
)

// docsRevisionRoutes serves a document's history to its own Private page
// (docs/spec/document-revisions-TECH.md, "Serving revisions"). Everything
// else goes to next. A Public route gets 404 for every revision path.
func (s *Service) docsRevisionRoutes(slugName string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p != "/_docs/api/revisions" && !strings.HasPrefix(p, "/_docs/api/revisions/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Header.Get("X-Flats-Access") != "private" {
			http.NotFound(w, r)
			return
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(p, "/_docs/api/revisions"), "/")
		if rest == "" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				docsRevisionError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
				return
			}
			before, _ := strconv.Atoi(r.URL.Query().Get("before"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit <= 0 {
				limit = 50
			}
			page, err := s.ListRevisions(r.Context(), slugName, before, limit)
			docsRevisionReply(w, page, err)
			return
		}
		numText, sub, _ := strings.Cut(rest, "/")
		n, err := strconv.Atoi(numText)
		if err != nil || n < 1 || strconv.Itoa(n) != numText {
			http.NotFound(w, r)
			return
		}
		switch {
		case sub == "" && r.Method == http.MethodGet:
			out, err := s.docsRevisionView(r, slugName, n)
			docsRevisionReply(w, out, err)
		case sub == "document" && r.Method == http.MethodGet:
			doc := r.URL.Query().Get("doc")
			if !fs.ValidPath(doc) || !bundle.IsMarkdown(doc) {
				docsRevisionError(w, http.StatusBadRequest, "invalid", "doc must be a Markdown path")
				return
			}
			b, f, err := s.RevisionFileContent(r.Context(), slugName, n, doc)
			docsRevisionReply(w, map[string]any{"doc": doc, "markdown": string(b), "hash": f.Hash}, err)
		case strings.HasPrefix(sub, "files/") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			s.docsRevisionFile(w, r, slugName, n, strings.TrimPrefix(sub, "files/"))
		case sub == "pin" && r.Method == http.MethodPost:
			var in struct {
				Pinned bool `json:"pinned"`
			}
			if !docsWriteAllowed(w, r, &in) {
				return
			}
			err := s.PinRevision(r.Context(), slugName, n, in.Pinned)
			docsRevisionReply(w, map[string]any{"status": "done", "revision": n, "pinned": in.Pinned}, err)
		case sub == "rollback" && r.Method == http.MethodPost:
			var in struct {
				Name string `json:"name"`
			}
			if !docsWriteAllowed(w, r, &in) {
				return
			}
			res, err := s.RollbackRevision(r.Context(), slugName, n, docsPersonLabel(r, in.Name), ViaConsole)
			docsRevisionReply(w, map[string]any{"status": "done", "revision": res, "rollback_of": n}, err)
		default:
			docsRevisionError(w, http.StatusNotFound, "not_found", "no such revision route")
		}
	})
}

// docsRevisionView describes one revision with the titles of its
// documents, derived as the docs app derives them.
func (s *Service) docsRevisionView(r *http.Request, slugName string, n int) (map[string]any, error) {
	rev, files, err := s.GetRevision(r.Context(), slugName, n)
	if err != nil {
		return nil, err
	}
	var m bundle.Manifest
	if b, _, err := s.RevisionFileContent(r.Context(), slugName, n, bundle.ManifestName); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	entry := m.Entry
	type docInfo struct {
		Path  string `json:"path"`
		Title string `json:"title"`
	}
	docs := []docInfo{}
	var paths []string
	for _, f := range files {
		if bundle.IsMarkdown(f.Path) {
			paths = append(paths, f.Path)
		}
	}
	if entry == "" {
		entry = defaultDocsEntry(paths)
	}
	for _, p := range paths {
		title := p
		if p == entry && m.Name != "" {
			title = m.Name
		} else if b, _, err := s.RevisionFileContent(r.Context(), slugName, n, p); err == nil {
			if h := firstHeading.FindSubmatch(b); h != nil {
				title = strings.TrimSpace(string(h[1]))
			}
		}
		docs = append(docs, docInfo{Path: p, Title: title})
	}
	// The revision before it that still exists, so the page can show what
	// this one changed; 0 when it is the oldest kept.
	previous := 0
	if older, err := s.st.ListRevisions(r.Context(), slugName, n, 1); err == nil && len(older) == 1 {
		previous = older[0].Number
	}
	// Other files with their hashes, so the page can tell which changed
	// between two revisions it compares.
	other := []store.RevisionFile{}
	for _, f := range files {
		if !bundle.IsMarkdown(f.Path) {
			other = append(other, f)
		}
	}
	return map[string]any{"revision": rev, "entry": entry, "documents": docs, "previous": previous, "files": other}, nil
}

var firstHeading = regexp.MustCompile(`(?m)^#\s+(.+)$`)

// defaultDocsEntry follows the bundle's entry default: index.md, then
// README.md, then the only Markdown file.
func defaultDocsEntry(paths []string) string {
	for _, want := range []string{"index.md", "README.md"} {
		for _, p := range paths {
			if p == want {
				return p
			}
		}
	}
	if len(paths) == 1 {
		return paths[0]
	}
	return ""
}

// docsRevisionFile serves a non-document file of a revision under the same
// inert rules as the document's current assets.
func (s *Service) docsRevisionFile(w http.ResponseWriter, r *http.Request, slugName string, n int, p string) {
	if !fs.ValidPath(p) || bundle.IsMarkdown(p) {
		http.NotFound(w, r)
		return
	}
	b, _, err := s.RevisionFileContent(r.Context(), slugName, n, p)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		docsRevisionError(w, http.StatusServiceUnavailable, "unavailable", "could not read the file")
		return
	}
	typ := docsInlineMIME(p)
	if strings.HasPrefix(typ, "audio/") || strings.HasPrefix(typ, "video/") {
		w.Header().Set("Content-Security-Policy", docsMediaCSP)
	} else {
		w.Header().Set("Content-Security-Policy", docsAssetCSP)
	}
	if typ != "" {
		w.Header().Set("Content-Type", typ)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}

// docsWriteAllowed applies the write protection of the revision routes and
// decodes the JSON body into v.
func docsWriteAllowed(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Header.Get("X-Flats-Docs") != "1" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		docsRevisionError(w, http.StatusForbidden, "forbidden", "revision changes need the document page")
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		if site != "same-origin" {
			docsRevisionError(w, http.StatusForbidden, "forbidden", "cross-site revision change")
			return false
		}
	} else if origin := r.Header.Get("Origin"); origin != "" && origin != requestOrigin(r) {
		docsRevisionError(w, http.StatusForbidden, "forbidden", "cross-origin revision change")
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil || (len(body) > 0 && json.Unmarshal(body, v) != nil) {
		docsRevisionError(w, http.StatusBadRequest, "invalid", "body must be JSON")
		return false
	}
	return true
}

func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// docsPersonLabel credits a person's rollback: the verified Tailscale name
// or login, else the page's collaborator name, else "Person".
func docsPersonLabel(r *http.Request, name string) string {
	if v := r.Header.Get("Tailscale-User-Name"); v != "" {
		return truncate(v, 80)
	}
	if v := r.Header.Get("Tailscale-User-Login"); v != "" {
		return truncate(v, 80)
	}
	name = strings.Join(strings.FieldsFunc(name, func(c rune) bool { return c < ' ' || c == 0x7f || c == ' ' }), " ")
	if name = truncate(strings.TrimSpace(name), 80); name != "" {
		return name
	}
	return "Person"
}

func docsRevisionReply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		var verr *bundle.ValidationError
		switch {
		case errors.Is(err, store.ErrNotFound):
			docsRevisionError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, ErrConflict):
			docsRevisionError(w, http.StatusConflict, "conflict", err.Error())
		case errors.As(err, &verr), errors.Is(err, ErrInvalid):
			docsRevisionError(w, http.StatusBadRequest, "invalid", err.Error())
		default:
			docsRevisionError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func docsRevisionError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": truncate(message, 500)})
}
