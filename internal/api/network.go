package api

import (
	"errors"
	"github.com/gosuda/flats/internal/core"
	"net/http"
)

const networkNote = "Operator-managed JavaScript server HTTP(S) origins; empty denies server fetch. At most 32 exact public origins using HTTP port 80 or HTTPS port 443; no wildcards or private targets. Updates apply on the next deploy/redeploy, rollback or data restoration after any required approval, Flats host restart, or new preview. Running workers and automatic restarts retain captured grants. After clearing permissions, redeploy to revoke live access. Browser fetch follows browser CORS/CSP independently."

func (s *Server) networkPolicy(w http.ResponseWriter, r *http.Request, _ core.Via) {
	policy, err := s.Svc.NetworkPolicy(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	if policy.Origins == nil {
		policy.Origins = []string{}
	}
	writeJSON(w, 200, map[string]any{"origins": policy.Origins, "updated_at": policy.UpdatedAt, "note": networkNote})
}

func (s *Server) putNetworkPolicy(w http.ResponseWriter, r *http.Request, via core.Via) {
	if via != core.ViaConsole && !(via == core.ViaCLI && isLoopback(r)) {
		writeErr(w, 403, errors.New("network permissions are set by the operator: use the web console or `flats network set` on the Flats host"))
		return
	}
	var in struct {
		Origins []string `json:"origins"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if in.Origins == nil {
		writeErr(w, 400, errors.New("origins must be an array; use [] to clear permissions"))
		return
	}
	if err := s.Svc.SetNetworkPolicy(r.Context(), r.PathValue("slug"), in.Origins, via); err != nil {
		fail(w, err)
		return
	}
	s.networkPolicy(w, r, via)
}
