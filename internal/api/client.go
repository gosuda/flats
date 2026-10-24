package api

import (
	"net/http"

	runtimeref "github.com/gosuda/flats/docs"
	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/clientstate"
	"github.com/gosuda/flats/internal/core"
)

func (s *Server) clientManifest(w http.ResponseWriter, r *http.Request, _ core.Via) {
	limit := s.Svc.UploadLimit()
	out := clientstate.NewManifest(buildinfo.Get(), limit)
	out.GuideRevision = runtimeref.EffectiveGuideRevision(limit, s.Svc.Autopilot().GuideState())
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	out.ServerURL = scheme + "://" + r.Host
	if origin := ExternalOrigin(r.Context()); origin != "" {
		out.ServerURL = origin
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
