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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/bundle"
	"github.com/gosuda/flats/internal/contenttype"
	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/slug"
	"github.com/gosuda/flats/internal/store"
)

// System describes the host for /api/status and the console.
type System interface {
	Status(ctx context.Context) any
}

// ProviderAdmin is a System that lets the console turn a network provider
// on or off for the whole host (network.permitted in config.json). Local is
// always on; every other provider stays off until the operator turns it on.
type ProviderAdmin interface {
	NetworkProviders(ctx context.Context) any // the provider list
	// SetNetworkProvider saves the change; a non-empty ifMatch must be the
	// current settings ETag. It returns the new ETag.
	SetNetworkProvider(ctx context.Context, id string, enabled bool, ifMatch string) (string, error)
}

// ZrokAccountAdmin is a System that lets the console connect the host to a
// zrok account with an account token, and disconnect it. The token is sent
// once and never returned.
type ZrokAccountAdmin interface {
	ConnectZrok(ctx context.Context, token string) error
	// DisconnectZrok unregisters the environment; force forgets it locally
	// even when zrok refuses its token.
	DisconnectZrok(ctx context.Context, force bool) error
}

// KeyAdmin is a System whose console manages the operator keys. Values are
// sent once and never returned.
type KeyAdmin interface {
	// HostKeys describes every operator key, never with its value.
	HostKeys(ctx context.Context) (any, error)
	SetTailscaleKey(ctx context.Context, key string) error
	DeleteTailscaleKey(ctx context.Context) error
	SetNetbirdKey(ctx context.Context, key string) error
	DeleteNetbirdKey(ctx context.Context) error
	// DismissNetbirdLegacyPeer hides the notice about the shared NetBird
	// peer an earlier version registered.
	DismissNetbirdLegacyPeer(ctx context.Context) error
}

// ConsoleDomainAdmin is a System whose console manages its own Tailscale
// and NetBird domains. provider is "tailscale" or "netbird". Changes are
// guarded by the settings ETag.
type ConsoleDomainAdmin interface {
	ConsoleDomains(ctx context.Context) (any, error)
	AddConsoleDomain(ctx context.Context, provider, name, ifMatch string) (etag string, err error)
	// DeleteConsoleDomain removes name. When the request came through name,
	// redirect is a remaining console address and after, run once the
	// response is written, retires the node.
	DeleteConsoleDomain(ctx context.Context, provider, name, ifMatch string) (etag, redirect string, after func(), err error)
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
					if err := consoleRequest(r); err != nil {
						writeErr(w, http.StatusForbidden, err)
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
		h("GET /client", s.clientManifest)
		h("GET /flats", s.listFlats)
		h("GET /flats/{slug}", s.getFlat)
		h("GET /flats/{slug}/document", s.getDocument)
		h("GET /flats/{slug}/draft", s.getDraft)
		h("POST /flats/{slug}/draft", s.saveVersion)
		h("PUT /flats/{slug}/draft", s.saveVersion)
		h("POST /flats/{slug}/publish", s.publish)
		h("POST /flats/{slug}/providers", s.providers)
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
		h("GET /flats/{slug}/env", s.listEnv)
		h("PUT /flats/{slug}/env/{name}", s.putEnv)
		h("DELETE /flats/{slug}/env/{name}", s.deleteEnv)
		h("GET /flats/{slug}/network", s.networkPolicy)
		h("PUT /flats/{slug}/network", s.putNetworkPolicy)
		h("GET /flats/{slug}/secrets", s.secrets)
		h("PUT /flats/{slug}/secrets/{name}", s.putSecret)
		h("DELETE /flats/{slug}/secrets/{name}", s.deleteSecret)
		h("GET /flats/{slug}/stats", s.stats)
		h("GET /flats/{slug}/snapshots", s.snapshots)
		h("GET /approvals/{id}", s.getApproval)
		h("GET /approvals", s.listApprovals)
		if console {
			h("POST /flats/{slug}/versions", s.saveVersion)
			h("POST /approvals/{id}/approve", s.decide(true))
			h("POST /approvals/{id}/reject", s.decide(false))
			h("POST /flats/{slug}/name", s.setName)
			h("PUT /flats/{slug}/listing", s.setListing)
			h("GET /flats/{slug}/revision-quota", s.revisionQuota)
			h("PUT /flats/{slug}/revision-quota", s.putRevisionQuota)
			h("GET /providers", s.hostProviders)
			h("PUT /providers/{id}", s.setHostProvider)
			h("PUT /providers/zrok/account", s.connectZrok)
			h("DELETE /providers/zrok/account", s.disconnectZrok)
			h("GET /keys", s.hostKeys)
			h("PUT /keys/tailscale", s.setTailscaleKey)
			h("DELETE /keys/tailscale", s.deleteTailscaleKey)
			h("PUT /keys/netbird", s.setNetbirdKey)
			h("DELETE /keys/netbird", s.deleteNetbirdKey)
			h("DELETE /keys/netbird/legacy-peer", s.dismissNetbirdLegacyPeer)
			h("GET /console-domains", s.consoleDomains)
			h("POST /console-domains", s.addConsoleDomain)
			h("DELETE /console-domains/{provider}/{name}", s.deleteConsoleDomain)
			h("GET /settings", s.getSettings)
			h("PUT /settings", s.putSettings)
			h("POST /settings/impact", s.settingsImpact)
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

// DecisionError retains the persisted approval and its typed execution cause.
type DecisionError struct {
	ErrorBody
	Approval store.Approval `json:"approval"`
}

func errorBody(err error) ErrorBody {
	body := ErrorBody{Error: err.Error()}
	body.Category = core.ErrorCategory(err)

	if v, ok := bundle.IsValidation(err); ok {
		body.Problems = v.Problems
	}
	var de *core.DeployError
	if errors.As(err, &de) && body.Category == "" {
		if de.Cause != nil {
			body.Category = "runtime_start_failed"
		} else {
			body.Category = "health_check_failed"
		}
	}
	if errors.As(err, &de) && de.Cause == nil {
		h := de.Health
		body.Health = &h
	}
	return body
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, errorBody(err))
}

// statusOf maps errors to HTTP codes: typed errors first, then a small
// fallback for messages of errors that are not typed yet.
func statusOf(err error) int {
	var de *core.DeployError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, core.ErrConfigChanged):
		return http.StatusPreconditionFailed
	case errors.Is(err, core.ErrInvalid), errors.Is(err, slug.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, core.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, core.ErrConflict), errors.Is(err, core.ErrNotDeployed), errors.Is(err, core.ErrUnavailable),
		errors.Is(err, core.ErrStaleApproval), errors.Is(err, core.ErrProviderNotPermitted),
		errors.Is(err, core.ErrProviderNotReady), errors.Is(err, core.ErrPublicStopUnconfirmed),
		errors.Is(err, core.ErrUnchangedContent), errors.Is(err, core.ErrProviderInUse):
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

func fail(w http.ResponseWriter, err error) {
	var pending *core.PendingApproval
	if errors.As(err, &pending) {
		writeJSON(w, http.StatusAccepted, pending.ActionResult)
		return
	}
	writeErr(w, statusOf(err), err)
}

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
	writeJSON(w, 200, FlatResponse{FlatView: f, Draft: f.Draft})
}

// FlatResponse makes the absence of a Draft explicit for console consumers.
type FlatResponse struct {
	core.FlatView
	Draft *store.Draft `json:"draft"`
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

// SaveResponse keeps the archive-upload compatibility object and the current
// Draft separate. A pending request is never represented as a live version.
type SaveResponse struct {
	Version store.Version      `json:"version"`
	Draft   store.Draft        `json:"draft"`
	Deploy  *core.ActionResult `json:"deploy,omitempty"`
	// Flat is set for a document, which runs at once: it has no Draft or
	// publish step, so deploy is ignored.
	Flat *core.FlatView `json:"flat,omitempty"`
	core.ActionResult
}

func (s *Server) saveVersion(w http.ResponseWriter, r *http.Request, via core.Via) {
	q := r.URL.Query()
	dirty, _ := strconv.ParseBool(q.Get("git_dirty"))
	meta := core.SaveMeta{GitSHA: q.Get("git_sha"), GitDirty: dirty, Message: q.Get("message")}
	if values, present := q["expected_revision"]; present {
		if len(values) != 1 {
			writeErr(w, 400, errors.New("expected_revision must be a single nonnegative integer"))
			return
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 0 {
			writeErr(w, 400, errors.New("expected_revision must be a nonnegative integer"))
			return
		}
		meta.ExpectedRevision, meta.CheckRevision = n, true
	}
	files, err := bundle.FromArchive(r.Body, bundle.Limits{MaxBytes: s.Svc.UploadLimit()})
	if err != nil {
		fail(w, err)
		return
	}
	v, err := s.Svc.SaveVersion(r.Context(), r.PathValue("slug"), files, meta, via)
	if err != nil {
		fail(w, err)
		return
	}
	if f, err := s.Svc.GetFlat(r.Context(), r.PathValue("slug")); err == nil && f.Type == contenttype.Docs {
		writeJSON(w, http.StatusCreated, SaveResponse{Version: v, Flat: &f})
		return
	}
	draft, err := s.Svc.GetDraft(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	out := SaveResponse{Version: v, Draft: draft}
	if deploy, _ := strconv.ParseBool(q.Get("deploy")); deploy {
		res, err := s.Svc.RequestPublish(r.Context(), r.PathValue("slug"), v.Revision, v.Hash, via)
		if err != nil {
			writeJSON(w, statusOf(err), struct {
				SaveResponse
				DeployError ErrorBody `json:"deploy_error"`
			}{out, errorBody(err)})
			return
		}
		out.ActionResult, out.Deploy = res, &res
		writeJSON(w, http.StatusAccepted, out)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) getDraft(w http.ResponseWriter, r *http.Request, _ core.Via) {
	draft, err := s.Svc.GetDraft(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

// PublishRequest freezes the selected current revision and optional content
// hash. Revision zero selects current, and cannot substitute a stale revision.
type PublishRequest struct {
	Revision int    `json:"revision"`
	Hash     string `json:"hash,omitempty"`
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request, via core.Via) {
	var in PublishRequest
	if err := decode(r, &in); err != nil || in.Revision < 0 {
		writeErr(w, 400, errors.New("publish body requires a nonnegative revision and optional hash"))
		return
	}
	res, err := s.Svc.RequestPublish(r.Context(), r.PathValue("slug"), in.Revision, in.Hash, via)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// ProviderPermissionRequest grants usage only; it never publishes or changes
// visibility.
type ProviderPermissionRequest struct {
	Provider  string `json:"provider"`
	Permitted bool   `json:"permitted"`
}

func (s *Server) providers(w http.ResponseWriter, r *http.Request, via core.Via) {
	if via != core.ViaConsole {
		writeErr(w, http.StatusForbidden, errors.New("provider permissions are set by the operator in the web console"))
		return
	}
	var in ProviderPermissionRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Svc.SetProviderPermission(r.Context(), r.PathValue("slug"), in.Provider, in.Permitted, via); err != nil {
		fail(w, err)
		return
	}
	s.getFlat(w, r, via)
}

// setListing changes whether the flat's Portal route appears in relay
// listings: {"listing": "default"|"hidden"|"listed"}. Console only.
func (s *Server) setListing(w http.ResponseWriter, r *http.Request, via core.Via) {
	var in struct {
		Listing string `json:"listing"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	fv, err := s.Svc.SetPortalListing(r.Context(), r.PathValue("slug"), in.Listing, via)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, fv)
}

// revisionQuota returns a design's revision and pin quota overrides and
// the host defaults. Console only.
func (s *Server) revisionQuota(w http.ResponseWriter, r *http.Request, _ core.Via) {
	q, err := s.Svc.RevisionQuotas(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, q)
}

// putRevisionQuota stores a design's overrides: {"revision_quota": n|null,
// "pin_quota": n|null}; null follows the host default. Console only.
func (s *Server) putRevisionQuota(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var in struct {
		RevisionQuota *int `json:"revision_quota"`
		PinQuota      *int `json:"pin_quota"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Svc.SetRevisionQuotas(r.Context(), r.PathValue("slug"), in.RevisionQuota, in.PinQuota); err != nil {
		fail(w, err)
		return
	}
	s.revisionQuota(w, r, core.ViaConsole)
}

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request, _ core.Via) {
	vs, err := s.Svc.ListVersions(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	if vs == nil {
		vs = []store.Version{}
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
	if n < 0 {
		writeErr(w, 400, fmt.Errorf("%w: body must be {\"version\": <n>} with a positive version number; save the draft as a new version first, then deploy that version", core.ErrInvalid))
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

type PreviewRequest struct {
	Target  string `json:"target,omitempty"`
	Version int    `json:"version"`
}

func (s *Server) openPreview(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var in PreviewRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if in.Version < 0 || (in.Target != "" && in.Target != "draft" && in.Target != "version") || (in.Target == "draft" && in.Version != 0) || (in.Target == "version" && in.Version == 0) {
		writeErr(w, 400, errors.New("preview target must be draft with version 0, or a published positive version"))
		return
	}
	p, err := s.Svc.OpenPreview(r.Context(), r.PathValue("slug"), in.Version)
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
	writeJSON(w, 200, map[string]any{"secrets": names, "note": "values are never returned. Changes apply to live on the next deploy/redeploy, rollback or data restoration (after any required approval), or Flats host restart. New previews capture current settings; automatic worker restarts reuse their captured settings."})
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
	writeJSON(w, 200, map[string]any{"name": r.PathValue("name"), "status": "stored; redeploy to apply", "note": "Changes apply to live on the next deploy/redeploy, rollback or data restoration (after any required approval), or Flats host restart. New previews capture current settings; automatic worker restarts reuse their captured settings."})
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
	writeJSON(w, 200, map[string]any{"deleted": r.PathValue("name"), "note": "Changes apply to live on the next deploy/redeploy, rollback or data restoration (after any required approval), or Flats host restart. New previews capture current settings; automatic worker restarts reuse their captured settings."})
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

func (s *Server) decide(approve bool) func(w http.ResponseWriter, r *http.Request, via core.Via) {
	return func(w http.ResponseWriter, r *http.Request, _ core.Via) {
		who := approverOf(r.Context())
		ctx := r.Context()
		if who != "" {
			ctx = core.WithActor(ctx, who)
		}
		a, err := s.Svc.Decide(ctx, r.PathValue("id"), approve)
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
		out := a
		if err != nil {
			writeJSON(w, statusOf(err), DecisionError{ErrorBody: errorBody(err), Approval: out})
			return
		}
		writeJSON(w, 200, out)
	}
}

func (s *Server) providerAdmin(w http.ResponseWriter) (ProviderAdmin, bool) {
	a, ok := s.System.(ProviderAdmin)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("this host does not manage network providers"))
	}
	return a, ok
}

func (s *Server) hostProviders(w http.ResponseWriter, r *http.Request, _ core.Via) {
	if a, ok := s.providerAdmin(w); ok {
		writeJSON(w, 200, map[string]any{"providers": a.NetworkProviders(r.Context())})
	}
}

func (s *Server) setHostProvider(w http.ResponseWriter, r *http.Request, _ core.Via) {
	a, ok := s.providerAdmin(w)
	if !ok {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decode(r, &in); err != nil || in.Enabled == nil {
		writeErr(w, 400, errors.New(`body must be {"enabled": true|false}`))
		return
	}
	etag, err := a.SetNetworkProvider(r.Context(), r.PathValue("id"), *in.Enabled, ifMatch(r))
	if err != nil {
		fail(w, err)
		return
	}
	if etag != "" {
		w.Header().Set("ETag", strconv.Quote(etag))
	}
	writeJSON(w, 200, map[string]any{"providers": a.NetworkProviders(r.Context()), "etag": etag})
}

func (s *Server) zrokAccountAdmin(w http.ResponseWriter) (ZrokAccountAdmin, ProviderAdmin, bool) {
	z, ok := s.System.(ZrokAccountAdmin)
	p, pok := s.System.(ProviderAdmin)
	if !ok || !pok {
		writeErr(w, http.StatusNotFound, errors.New("this host does not manage a zrok account"))
		return nil, nil, false
	}
	return z, p, true
}

// connectZrok enables a zrok environment with the operator's account token.
func (s *Server) connectZrok(w http.ResponseWriter, r *http.Request, _ core.Via) {
	z, p, ok := s.zrokAccountAdmin(w)
	if !ok {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Token) == "" {
		writeErr(w, 400, errors.New(`body must be {"token": "<zrok account token>"}`))
		return
	}
	if err := z.ConnectZrok(r.Context(), strings.TrimSpace(in.Token)); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"providers": p.NetworkProviders(r.Context())})
}

// disconnectZrok unregisters the host's zrok environment.
func (s *Server) disconnectZrok(w http.ResponseWriter, r *http.Request, _ core.Via) {
	z, p, ok := s.zrokAccountAdmin(w)
	if !ok {
		return
	}
	if err := z.DisconnectZrok(r.Context(), r.URL.Query().Get("force") == "1"); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"providers": p.NetworkProviders(r.Context())})
}

func (s *Server) keyAdmin(w http.ResponseWriter) (KeyAdmin, bool) {
	k, ok := s.System.(KeyAdmin)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("this host does not manage operator keys"))
	}
	return k, ok
}

func (s *Server) writeKeys(w http.ResponseWriter, r *http.Request, k KeyAdmin) {
	keys, err := k.HostKeys(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"keys": keys})
}

// hostKeys lists the operator keys: state, source and time, never values.
func (s *Server) hostKeys(w http.ResponseWriter, r *http.Request, _ core.Via) {
	if k, ok := s.keyAdmin(w); ok {
		s.writeKeys(w, r, k)
	}
}

// setTailscaleKey stores the Tailscale auth key new tailnet nodes use.
func (s *Server) setTailscaleKey(w http.ResponseWriter, r *http.Request, _ core.Via) {
	k, ok := s.keyAdmin(w)
	if !ok {
		return
	}
	var in struct {
		Key string `json:"key"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Key) == "" {
		writeErr(w, 400, errors.New(`body must be {"key": "<Tailscale auth key>"}`))
		return
	}
	if err := k.SetTailscaleKey(r.Context(), in.Key); err != nil {
		fail(w, err)
		return
	}
	s.writeKeys(w, r, k)
}

// deleteTailscaleKey removes the stored Tailscale auth key.
func (s *Server) deleteTailscaleKey(w http.ResponseWriter, r *http.Request, _ core.Via) {
	s.keyAction(w, r, KeyAdmin.DeleteTailscaleKey)
}

// setNetbirdKey stores the NetBird setup key new NetBird peers register with.
func (s *Server) setNetbirdKey(w http.ResponseWriter, r *http.Request, _ core.Via) {
	k, ok := s.keyAdmin(w)
	if !ok {
		return
	}
	var in struct {
		Key string `json:"key"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Key) == "" {
		writeErr(w, 400, errors.New(`body must be {"key": "<NetBird setup key>"}`))
		return
	}
	if err := k.SetNetbirdKey(r.Context(), in.Key); err != nil {
		fail(w, err)
		return
	}
	s.writeKeys(w, r, k)
}

// deleteNetbirdKey removes the stored NetBird setup key.
func (s *Server) deleteNetbirdKey(w http.ResponseWriter, r *http.Request, _ core.Via) {
	s.keyAction(w, r, KeyAdmin.DeleteNetbirdKey)
}

// dismissNetbirdLegacyPeer hides the notice about the old shared peer.
func (s *Server) dismissNetbirdLegacyPeer(w http.ResponseWriter, r *http.Request, _ core.Via) {
	s.keyAction(w, r, KeyAdmin.DismissNetbirdLegacyPeer)
}

// keyAction runs a body-less key change and answers with the key list.
func (s *Server) keyAction(w http.ResponseWriter, r *http.Request, do func(KeyAdmin, context.Context) error) {
	k, ok := s.keyAdmin(w)
	if !ok {
		return
	}
	if err := do(k, r.Context()); err != nil {
		fail(w, err)
		return
	}
	s.writeKeys(w, r, k)
}

func (s *Server) consoleDomainAdmin(w http.ResponseWriter) (ConsoleDomainAdmin, bool) {
	d, ok := s.System.(ConsoleDomainAdmin)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("this host does not manage console domains"))
	}
	return d, ok
}

func (s *Server) consoleDomains(w http.ResponseWriter, r *http.Request, _ core.Via) {
	d, ok := s.consoleDomainAdmin(w)
	if !ok {
		return
	}
	v, err := d.ConsoleDomains(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

// addConsoleDomain starts a console node and adds it to the list.
func (s *Server) addConsoleDomain(w http.ResponseWriter, r *http.Request, _ core.Via) {
	d, ok := s.consoleDomainAdmin(w)
	if !ok {
		return
	}
	var in struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeErr(w, 400, errors.New(`body must be {"provider": "tailscale" or "netbird", "name": "<host name>"}`))
		return
	}
	if in.Provider == "" {
		in.Provider = "tailscale"
	}
	etag, err := d.AddConsoleDomain(r.Context(), in.Provider, strings.TrimSpace(in.Name), ifMatch(r))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(etag))
	writeJSON(w, 200, map[string]any{"etag": etag})
}

// deleteConsoleDomain removes a console domain and logs its node out. When
// the request came through that domain, the node is logged out only after
// the response is written, since that closes this connection.
func (s *Server) deleteConsoleDomain(w http.ResponseWriter, r *http.Request, _ core.Via) {
	d, ok := s.consoleDomainAdmin(w)
	if !ok {
		return
	}
	etag, redirect, after, err := d.DeleteConsoleDomain(r.Context(), r.PathValue("provider"), r.PathValue("name"), ifMatch(r))
	if err != nil {
		fail(w, err)
		return
	}
	if etag != "" {
		w.Header().Set("ETag", strconv.Quote(etag))
	}
	writeJSON(w, 200, map[string]any{"etag": etag, "redirect": redirect})
	if after != nil {
		after()
	}
}

// restartKeys are settings `flats serve` reads only at startup.
var restartKeys = []string{core.SetPortalRelays, core.SetPortalDiscover, core.SetPortalMaxRelay}

const restartNote = "portal relay settings are saved to config.json and apply after `flats serve` restarts"

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, _ core.Via) {
	// The ETag is read first: a save in between leaves it older than the
	// settings, so a change based on them is refused rather than accepted.
	cfg, err := s.Svc.SettingsConfig()
	if err != nil {
		fail(w, err)
		return
	}
	st, err := s.Svc.Settings(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	setETag(w, cfg)
	writeJSON(w, 200, map[string]any{"settings": st, "defaults": core.Defaults, "apply_on_restart": restartKeys, "note": restartNote, "config": cfg})
}

// setETag sends the settings ETag, the hash of config.json as the host last
// read or wrote it.
func setETag(w http.ResponseWriter, cfg *core.ConfigView) {
	if cfg != nil && cfg.ETag != "" {
		w.Header().Set("ETag", strconv.Quote(cfg.ETag))
	}
}

// ifMatch returns the If-Match entity tag without quotes; "" when absent.
func ifMatch(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if u, err := strconv.Unquote(v); err == nil {
		return u
	}
	return v
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var in map[string]string
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	u, err := s.Svc.UpdateSettingsMatch(r.Context(), in, ifMatch(r))
	if err != nil {
		fail(w, err)
		return
	}
	// applied lists the changed settings in effect now; restart_required
	// those that take effect only after a restart, so the console can say
	// which is which instead of "saved".
	applied, restart := []string{}, []string{}
	for _, k := range u.Changed {
		if slices.Contains(restartKeys, k) {
			restart = append(restart, k)
		} else {
			applied = append(applied, k)
		}
	}
	out := map[string]any{"settings": u.Settings, "applied": applied, "restart_required": restart}
	if u.ETag != "" {
		out["etag"] = u.ETag
		w.Header().Set("ETag", strconv.Quote(u.ETag))
	}
	if len(restart) > 0 {
		out["note"] = restartNote
	}
	writeJSON(w, 200, out)
}

// settingsImpact reports what the next pruning would remove if the
// candidate settings were saved. It changes nothing.
func (s *Server) settingsImpact(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var in map[string]string
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	impact, err := s.Svc.RetentionImpact(r.Context(), in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"impact": impact})
}

var _ = time.Second

func (s *Server) getDocument(w http.ResponseWriter, r *http.Request, _ core.Via) {
	document, err := s.Svc.GetDocument(r.Context(), r.PathValue("slug"), r.URL.Query().Get("doc"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, document)
}
