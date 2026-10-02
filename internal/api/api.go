// Package api is the HTTP API shared by agents, the CLI and the web console.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/oesni/flats/internal/bundle"
	"github.com/oesni/flats/internal/core"
	"github.com/oesni/flats/internal/store"
)

// System describes the host for /api/status and the console.
type System interface {
	Status(ctx context.Context) any
}

// Server serves /api and /console/api.
type Server struct {
	Svc    *core.Service
	System System
}

// Handler returns the API mux (mount at /).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, prefix := range []string{"/api", "/console/api"} {
		console := prefix == "/console/api"
		h := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, via core.Via)) {
			method, path, _ := strings.Cut(pattern, " ")
			mux.HandleFunc(method+" "+prefix+path, func(w http.ResponseWriter, r *http.Request) {
				via := core.ViaAPI
				if console {
					if !consoleRequest(r) {
						writeErr(w, http.StatusForbidden, errors.New("console endpoints only accept requests from the Flats web console"))
						return
					}
					via = core.ViaConsole
				} else if r.Header.Get("X-Flats-Client") == "cli" {
					via = core.ViaCLI
				}
				fn(w, r, via)
			})
		}
		h("GET /status", s.status)
		h("GET /flats", s.listFlats)
		h("GET /flats/{slug}", s.getFlat)
		h("GET /flats/{slug}/versions", s.listVersions)
		h("GET /flats/{slug}/versions/{n}", s.getVersion)
		h("GET /flats/{slug}/versions/{n}/files/{path...}", s.versionFile)
		h("POST /flats/{slug}/deploy", s.deploy)
		h("POST /flats/{slug}/rollback", s.rollback)
		h("GET /flats/{slug}/deployments", s.deployments)
		h("POST /flats/{slug}/previews", s.openPreview)
		h("GET /flats/{slug}/previews", s.listPreviews)
		h("DELETE /previews/{host}", s.closePreview)
		h("POST /flats/{slug}/visibility", s.visibility)
		h("POST /flats/{slug}/rename", s.rename)
		h("DELETE /flats/{slug}", s.deleteFlat)
		h("GET /flats/{slug}/logs", s.logs)
		h("GET /flats/{slug}/secrets", s.secrets)
		h("PUT /flats/{slug}/secrets/{name}", s.putSecret)
		h("DELETE /flats/{slug}/secrets/{name}", s.deleteSecret)
		h("GET /flats/{slug}/stats", s.stats)
		h("GET /approvals/{id}", s.getApproval)
		h("GET /approvals", s.listApprovals)
		if console {
			h("POST /approvals/{id}/approve", s.decide(true))
			h("POST /approvals/{id}/reject", s.decide(false))
			h("POST /flats/{slug}/name", s.setName)
			h("GET /settings", s.getSettings)
			h("PUT /settings", s.putSettings)
		} else {
			h("POST /flats", s.createFlat)
			h("POST /flats/{slug}/versions", s.saveVersion)
		}
	}
	return mux
}

// consoleRequest accepts only same-origin browser requests from the console.
func consoleRequest(r *http.Request) bool {
	if r.Header.Get("X-Flats-Console") != "1" {
		return false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// --- helpers ---

// ErrorBody is the JSON error shape.
type ErrorBody struct {
	Error    string             `json:"error"`
	Problems []bundle.Problem   `json:"problems,omitempty"`
	Health   *core.HealthResult `json:"health,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	body := ErrorBody{Error: err.Error()}
	if v, ok := bundle.IsValidation(err); ok {
		body.Problems = v.Problems
	}
	var de *core.DeployError
	if errors.As(err, &de) && de.Cause == nil {
		h := de.Health
		body.Health = &h
	}
	writeJSON(w, code, body)
}

// statusOf maps errors to HTTP codes.
func statusOf(err error) int {
	var de *core.DeployError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, core.ErrConflict), errors.Is(err, core.ErrNotDeployed):
		return http.StatusConflict
	case errors.As(err, &de):
		return http.StatusUnprocessableEntity
	}
	if _, ok := bundle.IsValidation(err); ok {
		return http.StatusUnprocessableEntity
	}
	msg := err.Error()
	if strings.Contains(msg, "invalid slug") || strings.Contains(msg, "unknown visibility") || strings.Contains(msg, "must ") || strings.Contains(msg, "already exists") {
		return http.StatusBadRequest
	}
	if strings.Contains(msg, "operator") || strings.Contains(msg, "not from the console") {
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func fail(w http.ResponseWriter, err error) { writeErr(w, statusOf(err), err) }

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func intParam(r *http.Request, name string) (int, error) {
	n, err := strconv.Atoi(r.PathValue(name))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// --- handlers ---

func (s *Server) status(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var sys any
	if s.System != nil {
		sys = s.System.Status(r.Context())
	}
	writeJSON(w, 200, map[string]any{"ok": true, "system": sys, "upload_max_bytes": s.Svc.UploadLimit()})
}

func (s *Server) listFlats(w http.ResponseWriter, r *http.Request, _ core.Via) {
	fs, err := s.Svc.ListFlats(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	q := strings.ToLower(r.URL.Query().Get("q"))
	out := fs[:0]
	for _, f := range fs {
		if q == "" || strings.Contains(f.Slug, q) || strings.Contains(strings.ToLower(f.Name), q) {
			out = append(out, f)
		}
	}
	writeJSON(w, 200, map[string]any{"flats": out})
}

func (s *Server) getFlat(w http.ResponseWriter, r *http.Request, _ core.Via) {
	f, err := s.Svc.GetFlat(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, f)
}

func (s *Server) createFlat(w http.ResponseWriter, r *http.Request, via core.Via) {
	var in struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	f, err := s.Svc.CreateFlat(r.Context(), in.Slug, in.Name, via)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, f)
}

func (s *Server) saveVersion(w http.ResponseWriter, r *http.Request, via core.Via) {
	slugName := r.PathValue("slug")
	lim := bundle.Limits{MaxBytes: s.Svc.UploadLimit()}
	files, err := bundle.FromArchive(r.Body, lim)
	if err != nil {
		fail(w, err)
		return
	}
	q := r.URL.Query()
	dirty, _ := strconv.ParseBool(q.Get("git_dirty"))
	meta := core.SaveMeta{GitSHA: q.Get("git_sha"), GitDirty: dirty, Message: q.Get("message")}
	v, err := s.Svc.SaveVersion(r.Context(), slugName, files, meta, via)
	if err != nil {
		fail(w, err)
		return
	}
	out := map[string]any{"version": v}
	if deploy, _ := strconv.ParseBool(q.Get("deploy")); deploy {
		res, err := s.Svc.Deploy(r.Context(), slugName, v.Number, via)
		if err != nil {
			body := ErrorBody{Error: err.Error()}
			var de *core.DeployError
			if errors.As(err, &de) && de.Cause == nil {
				h := de.Health
				body.Health = &h
			}
			writeJSON(w, statusOf(err), map[string]any{"version": v, "deploy_error": body})
			return
		}
		out["deploy"] = res
	}
	writeJSON(w, 201, out)
}

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request, _ core.Via) {
	vs, err := s.Svc.ListVersions(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"versions": vs})
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request, _ core.Via) {
	n, err := intParam(r, "n")
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	v, err := s.Svc.GetVersion(r.Context(), r.PathValue("slug"), n)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) versionFile(w http.ResponseWriter, r *http.Request, _ core.Via) {
	n, err := intParam(r, "n")
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	p, err := s.Svc.VersionFile(r.PathValue("slug"), n, r.PathValue("path"))
	if err != nil {
		fail(w, err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		fail(w, err)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

func versionBody(r *http.Request) (int, error) {
	var in struct {
		Version int `json:"version"`
	}
	if err := decode(r, &in); err != nil {
		return 0, err
	}
	return in.Version, nil
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request, via core.Via) {
	n, err := versionBody(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if n <= 0 {
		writeErr(w, 400, errors.New(`body must be {"version": <n>} with the saved version to deploy`))
		return
	}
	res, err := s.Svc.Deploy(r.Context(), r.PathValue("slug"), n, via)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request, via core.Via) {
	n, err := versionBody(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	res, err := s.Svc.Rollback(r.Context(), r.PathValue("slug"), n, via)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) deployments(w http.ResponseWriter, r *http.Request, _ core.Via) {
	ds, err := s.Svc.Deployments(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deployments": ds})
}

func (s *Server) openPreview(w http.ResponseWriter, r *http.Request, _ core.Via) {
	n, err := versionBody(r)
	if err != nil || n <= 0 {
		writeErr(w, 400, errors.New(`body must be {"version": <n>}`))
		return
	}
	p, err := s.Svc.OpenPreview(r.Context(), r.PathValue("slug"), n)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, p)
}

func (s *Server) listPreviews(w http.ResponseWriter, r *http.Request, _ core.Via) {
	ps, err := s.Svc.ListPreviews(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"previews": ps})
}

func (s *Server) closePreview(w http.ResponseWriter, r *http.Request, _ core.Via) {
	if err := s.Svc.ClosePreview(r.Context(), r.PathValue("host")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"closed": r.PathValue("host")})
}

func (s *Server) visibility(w http.ResponseWriter, r *http.Request, via core.Via) {
	var in struct {
		Visibility string `json:"visibility"`
		Reason     string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	res, err := s.Svc.SetVisibility(r.Context(), r.PathValue("slug"), store.Visibility(in.Visibility), via, in.Reason)
	if err != nil {
		fail(w, err)
		return
	}
	code := 200
	if res.Status == "pending_approval" {
		code = 202
	}
	writeJSON(w, code, res)
}

func (s *Server) rename(w http.ResponseWriter, r *http.Request, via core.Via) {
	var in struct {
		Slug string `json:"slug"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	f, err := s.Svc.RenameSlug(r.Context(), r.PathValue("slug"), in.Slug, via)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, f)
}

func (s *Server) setName(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeErr(w, 400, errors.New(`body must be {"name": "..."}`))
		return
	}
	f, err := s.Svc.RenameDisplay(r.Context(), r.PathValue("slug"), strings.TrimSpace(in.Name))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, f)
}

func (s *Server) deleteFlat(w http.ResponseWriter, r *http.Request, via core.Via) {
	res, err := s.Svc.Delete(r.Context(), r.PathValue("slug"), via, r.URL.Query().Get("reason"))
	if err != nil {
		fail(w, err)
		return
	}
	code := 200
	if res.Status == "pending_approval" {
		code = 202
	}
	writeJSON(w, code, res)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, _ core.Via) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	evs, err := s.Svc.Events(r.Context(), r.PathValue("slug"), q.Get("kind"), after, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"events": evs})
}

func (s *Server) secrets(w http.ResponseWriter, r *http.Request, _ core.Via) {
	names, err := s.Svc.SecretNames(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"secrets": names, "note": "values are never returned; changes apply on the next deploy"})
}

// operatorVia allows secret writes from the console or the CLI on this host.
func operatorVia(r *http.Request, via core.Via) (core.Via, error) {
	if via == core.ViaConsole {
		return via, nil
	}
	if via == core.ViaCLI && isLoopback(r) {
		return via, nil
	}
	return via, errors.New("secret values are set by the operator: use the web console or `flats secret set` on the Flats host")
}

func (s *Server) putSecret(w http.ResponseWriter, r *http.Request, via core.Via) {
	v, err := operatorVia(r, via)
	if err != nil {
		writeErr(w, 403, err)
		return
	}
	var in struct {
		Value string `json:"value"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Svc.SetSecret(r.Context(), r.PathValue("slug"), r.PathValue("name"), in.Value, v); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"name": r.PathValue("name"), "status": "stored; redeploy to apply"})
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request, via core.Via) {
	v, err := operatorVia(r, via)
	if err != nil {
		writeErr(w, 403, err)
		return
	}
	if err := s.Svc.DeleteSecret(r.Context(), r.PathValue("slug"), r.PathValue("name"), v); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": r.PathValue("name")})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request, _ core.Via) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 365 {
		days = 30
	}
	pv, err := s.Svc.PageViews(r.Context(), r.PathValue("slug"), days)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"page_views": pv, "note": "request counts of HTML pages; Portal does not pass visitor IPs, so unique visitors are not counted"})
}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request, _ core.Via) {
	a, err := s.Svc.GetApproval(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request, _ core.Via) {
	as, err := s.Svc.ListApprovals(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"approvals": as})
}

func (s *Server) decide(approve bool) func(w http.ResponseWriter, r *http.Request, via core.Via) {
	return func(w http.ResponseWriter, r *http.Request, _ core.Via) {
		a, err := s.Svc.Decide(r.Context(), r.PathValue("id"), approve)
		if err != nil {
			code := statusOf(err)
			writeJSON(w, code, map[string]any{"error": err.Error(), "approval": a})
			return
		}
		writeJSON(w, 200, a)
	}
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, _ core.Via) {
	st, err := s.Svc.Settings(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"settings": st, "defaults": core.Defaults})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var in map[string]string
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	st, err := s.Svc.UpdateSettings(r.Context(), in)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"settings": st})
}

var _ = time.Second
