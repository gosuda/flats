package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// autopilotView is the console's view of the Autopilot settings.
type autopilotView struct {
	Enabled   bool               `json:"enabled"`
	Allowed   []core.AutopilotOp `json:"allowed"`
	Guide     string             `json:"guide"`
	GuideMax  int                `json:"guide_max"`
	UpdatedAt *time.Time         `json:"updated_at,omitempty"`
}

func writeAutopilot(w http.ResponseWriter, a core.Autopilot) {
	v := autopilotView{Enabled: a.Enabled, Allowed: a.Allowed.List(), Guide: a.Guide, GuideMax: core.AutopilotGuideMax}
	if !a.UpdatedAt.IsZero() {
		t := a.UpdatedAt.UTC()
		v.UpdatedAt = &t
	}
	writeJSON(w, 200, map[string]any{"autopilot": v})
}

func invalidBody(w http.ResponseWriter, format string, args ...any) {
	writeErr(w, http.StatusBadRequest, fmt.Errorf("%w: "+format, append([]any{core.ErrInvalid}, args...)...))
}

// consoleActor names who sent a console request, for the host log, from
// the network it arrived on as agent decisions are classified: the external
// origin, the tailnet user or node and the NetBird peer of a console domain,
// else the console listener's caller by its socket address. Identities are
// quoted.
func consoleActor(r *http.Request) string {
	ctx := r.Context()
	if origin := ExternalOrigin(ctx); origin != "" {
		return "the console via external origin " + origin + " (no authenticated identity)"
	}
	who := approverOf(ctx)
	if fqdn, ok := strings.CutPrefix(who, "netbird:"); ok {
		return "NetBird peer " + strconv.Quote(fqdn)
	}
	if who != "" {
		return "tailnet user " + strconv.Quote(who)
	}
	if prov, _, ok := core.ConsoleDomainOf(ctx); ok {
		if prov == "netbird" {
			if peer := core.NetbirdPeerOf(ctx); peer != "" {
				return "NetBird peer " + strconv.Quote(peer)
			}
			return "the console on a NetBird console domain (unknown peer)"
		}
		login, node := core.TailnetPeerOf(ctx)
		switch {
		case login != "":
			return "tailnet user " + strconv.Quote(login)
		case node != "":
			return "tailnet node " + strconv.Quote(node)
		}
		return "the console on a Tailscale console domain (no tailnet identity)"
	}
	src, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return "the console listener from an unknown address (no authenticated identity)"
	}
	addr := src.Addr().Unmap()
	switch {
	case addr.IsLoopback():
		return "the console on the loopback listener (no tailnet identity)"
	case IsHostAddr(addr):
		return "the console on this host from " + addr.String() + " (no tailnet identity)"
	}
	return "the console listener from " + addr.String() + " (no authenticated identity)"
}

func (s *Server) getAutopilot(w http.ResponseWriter, r *http.Request, _ core.Via) {
	a, err := s.Svc.StoredAutopilot(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeAutopilot(w, a)
}

// setAutopilotEnabled turns Autopilot on, with the operations the
// confirmation dialog listed, or off: {"enabled": true,
// "confirmed_allowed": [...]} or {"enabled": false}.
func (s *Server) setAutopilotEnabled(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var in struct {
		Enabled          *bool           `json:"enabled"`
		ConfirmedAllowed json.RawMessage `json:"confirmed_allowed"`
	}
	if err := decode(r, &in); err != nil {
		invalidBody(w, "%v", err)
		return
	}
	if in.Enabled == nil {
		invalidBody(w, `body must be {"enabled": true, "confirmed_allowed": [...]} or {"enabled": false}`)
		return
	}
	var confirmed *core.OpSet
	switch {
	case *in.Enabled && in.ConfirmedAllowed == nil:
		invalidBody(w, "turning Autopilot on needs confirmed_allowed, the operations the confirmation listed")
		return
	case !*in.Enabled && in.ConfirmedAllowed != nil:
		invalidBody(w, "confirmed_allowed is only for turning Autopilot on")
		return
	case *in.Enabled:
		var ids []string
		if bytes.Equal(in.ConfirmedAllowed, []byte("null")) || json.Unmarshal(in.ConfirmedAllowed, &ids) != nil {
			invalidBody(w, "confirmed_allowed must be a list of operation IDs")
			return
		}
		set, err := core.ParseOpSet(ids)
		if err != nil {
			fail(w, err)
			return
		}
		confirmed = &set
	}
	a, err := s.Svc.SetAutopilotEnabled(r.Context(), *in.Enabled, confirmed, consoleActor(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeAutopilot(w, a)
}

// setAutopilotOp allows or disallows one operation: {"allowed": true|false}.
func (s *Server) setAutopilotOp(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var in struct {
		Allowed *bool `json:"allowed"`
	}
	if err := decode(r, &in); err != nil {
		invalidBody(w, "%v", err)
		return
	}
	if in.Allowed == nil {
		invalidBody(w, `body must be {"allowed": true|false}`)
		return
	}
	a, err := s.Svc.SetAutopilotOp(r.Context(), core.AutopilotOp(r.PathValue("op")), *in.Allowed, consoleActor(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeAutopilot(w, a)
}

// setAutopilotGuide stores the operation guide: {"guide": "..."}.
func (s *Server) setAutopilotGuide(w http.ResponseWriter, r *http.Request, _ core.Via) {
	var in struct {
		Guide *string `json:"guide"`
	}
	if err := decode(r, &in); err != nil {
		invalidBody(w, "%v", err)
		return
	}
	if in.Guide == nil {
		invalidBody(w, `body must be {"guide": "..."}`)
		return
	}
	a, err := s.Svc.SetAutopilotGuide(r.Context(), *in.Guide, consoleActor(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeAutopilot(w, a)
}
