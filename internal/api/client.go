package api

import (
	"net/http"

	"github.com/gosuda/flats/internal/buildinfo"
	"github.com/gosuda/flats/internal/clientstate"
	"github.com/gosuda/flats/internal/core"
)

func (s *Server) clientManifest(w http.ResponseWriter, r *http.Request, _ core.Via) {
	out := clientstate.NewManifest(buildinfo.Get(), s.Svc.UploadLimit())
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
