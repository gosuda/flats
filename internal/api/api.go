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

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

// System describes the host for /api/status and the console.
type System interface {
	Status(ctx context.Context) any
}

// Server serves /api and /console/api.
type Server struct {
	Svc      *core.Service
	System   System
	Operator *OperatorAuthority
}

// Handler returns the API mux (mount at /).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /console/api/operator/session", s.operatorSession)
	mux.HandleFunc("DELETE /console/api/operator/session", s.operatorLogout)
	for _, prefix := range []string{"/api", "/console/api"} {
		console := prefix == "/console/api"
		h := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, via core.Via)) {
			method, path, _ := strings.Cut(pattern, " ")
			mux.HandleFunc(method+" "+prefix+path, func(w http.ResponseWriter, r *http.Request) {
				via := core.ViaAPI
				if console {
					if err := consoleRequest(r); err != nil {
						writeErr(w, http.StatusForbidden, err)
						return
					}
					if !safeMethod(r.Method) && !s.requireOperator(w, r) {
						return
					}
					via = core.ViaConsole
				} else {
					if code, err := agentRequest(r); err != nil {
						writeErr(w, code, err)
						return
					}
					if r.Header.Get("X-Flats-Client") == "cli" {
						via = core.ViaCLI
					}
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
		h("GET /flats/{slug}/snapshots", s.snapshots)
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

// --- helpers ---

// ErrorBody is the JSON error shape.
type ErrorBody struct {
	Error    string             `json:"error"`
	Category string             `json:"category,omitempty"`
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

// statusOf maps errors to HTTP codes: typed errors first, then a small
// fallback for messages of errors that are not typed yet.
func statusOf(err error) int {
	var de *core.DeployError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, core.ErrInvalid), errors.Is(err, slug.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, core.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, core.ErrConflict), errors.Is(err, core.ErrNotDeployed), errors.Is(err, core.ErrUnavailable):
		return http.StatusConflict
	case errors.As(err, &de):
		return http.StatusUnprocessableEntity
	}
	if _, ok := bundle.IsValidation(err); ok {
		return http.StatusUnprocessableEntity
	}
	// Fallback for core errors that do not wrap a sentinel yet.
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "invalid slug"), strings.HasPrefix(msg, "unknown visibility"), strings.Contains(msg, " must "):
		return http.StatusBadRequest
	case strings.Contains(msg, "already exists"):
		return http.StatusConflict
	case strings.Contains(msg, "by the operator"), strings.Contains(msg, "only the operator"), strings.Contains(msg, "not from the console"):
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
	var in struct {
		Version     int  `json:"version"`
		RestoreData bool `json:"restore_data"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	res, err := s.Svc.RollbackWithData(r.Context(), r.PathValue("slug"), in.Version, in.RestoreData, via)
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
	pages, err := s.Svc.TopPages(r.Context(), r.PathValue("slug"), days)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"page_views": pv,
		"top_pages":  pages,
		"note":       "Page views count GET page requests, including extensionless API routes. Unique visitors are not tracked. Per-page counts start with this update; query strings are excluded.",
	})
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request, _ core.Via) {
	if _, err := s.Svc.GetFlat(r.Context(), r.PathValue("slug")); err != nil {
		fail(w, err)
		return
	}
	snaps, err := s.Svc.Snapshots(r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"snapshots": snaps, "note": "taken automatically before each deploy of a flat with a database; rollback with restore_data puts back the one taken before the current version"})
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

// decision is an approval as returned by approve/reject, with the tailnet
// login of the operator who decided when the request came through the
// console node (empty on the loopback listener, which has no identity).
type decision struct {
	store.Approval
	DecidedBy string `json:"decided_by,omitempty"`
}

func (s *Server) decide(approve bool) func(w http.ResponseWriter, r *http.Request, via core.Via) {
	return func(w http.ResponseWriter, r *http.Request, _ core.Via) {
		a, err := s.Svc.Decide(r.Context(), r.PathValue("id"), approve)
		who := approverOf(r.Context())
		decided := err == nil || (a.Status == "failed" && !errors.Is(err, core.ErrConflict))
		// A deleted flat's events are gone with it, so an approved delete
		// reports its approver only in the response.
		if a.ID != "" && decided && !(a.Action == "delete" && a.Status == "approved") {
			src := "tailnet user " + who
			if who == "" {
				src = "the console on the loopback listener (no tailnet identity)"
			}
			s.Svc.Event(r.Context(), a.Flat, "info", "approval",
				fmt.Sprintf("approval %s (%s) %s by %s", a.ID, a.Action, a.Status, src),
				map[string]string{"approval": a.ID, "status": a.Status, "decided_by": who})
		}
		out := decision{Approval: a, DecidedBy: who}
		if err != nil {
			writeJSON(w, statusOf(err), map[string]any{"error": err.Error(), "approval": out})
			return
		}
		writeJSON(w, 200, out)
	}
}

// restartKeys are settings `flats serve` reads only at startup.
var restartKeys = []string{core.SetPortalRelays, core.SetPortalDiscover, core.SetPortalMaxRelay}

const restartNote = "portal relay settings apply after `flats serve` restarts; a --relays flag overrides portal_relays"

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, _ core.Via) {
	st, err := s.Svc.Settings(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"settings": st, "defaults": core.Defaults, "apply_on_restart": restartKeys, "note": restartNote})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var in map[string]string
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	before, err := s.Svc.Settings(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	st, err := s.Svc.UpdateSettings(r.Context(), in)
	if err != nil {
		fail(w, err)
		return
	}
	// restart_required lists the changed settings that take effect only
	// after a restart, so the console can say so instead of "saved".
	restart := []string{}
	for _, k := range restartKeys {
		if _, ok := in[k]; ok && st[k] != before[k] {
			restart = append(restart, k)
		}
	}
	out := map[string]any{"settings": st, "restart_required": restart}
	if len(restart) > 0 {
		out["note"] = restartNote
	}
	writeJSON(w, 200, out)
}

var _ = time.Second
