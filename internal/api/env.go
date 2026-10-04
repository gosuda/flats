package api

import (
	"errors"
	"net/http"

	"github.com/gosuda/flats/internal/core"
	"github.com/gosuda/flats/internal/store"
)

const envNote = "ordinary values are readable by management clients; use secrets for credentials. Changes apply on the next deploy or runtime restart, and never enter frontend bundles"

func (s *Server) listEnv(w http.ResponseWriter, r *http.Request, _ core.Via) {
	vars, err := s.Svc.ListEnv(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	if vars == nil {
		vars = []store.EnvVar{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"env": vars, "note": envNote})
}

func (s *Server) putEnv(w http.ResponseWriter, r *http.Request, via core.Via) {
	var in struct {
		Value *string `json:"value"`
	}
	if err := decode(r, &in); err != nil || in.Value == nil {
		writeErr(w, http.StatusBadRequest, errors.New("body requires a string value (empty string is allowed)"))
		return
	}
	if err := s.Svc.SetEnv(r.Context(), r.PathValue("slug"), r.PathValue("name"), *in.Value, via); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("name"), "status": "stored", "note": envNote})
}

func (s *Server) deleteEnv(w http.ResponseWriter, r *http.Request, via core.Via) {
	if err := s.Svc.DeleteEnv(r.Context(), r.PathValue("slug"), r.PathValue("name"), via); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("name"), "note": envNote})
}
