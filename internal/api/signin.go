package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gosuda/flats/internal/core"
)

// The routes of the Portal sign-in of a flat (docs/spec/portal-sign-in.md, rules 13 to 16, 25, 49, 54 and 56). A
// remote caller is a caller of /api that is not on the loopback address: core cannot tell it from a loopback one, so
// these routes refuse it themselves.

// hostCaller allows the console and a caller of /api on the loopback address, with the CLI header or without it.
func hostCaller(r *http.Request, via core.Via, refusal string) error {
	if via == core.ViaConsole || isLoopback(r) {
		return nil
	}
	return errors.New(refusal)
}

// getSignIn returns the stored sign-in of a flat with its entries (rule 56), so a remote caller gets 403.
func (s *Server) getSignIn(w http.ResponseWriter, r *http.Request, via core.Via) {
	if err := hostCaller(r, via, "the sign-in of a flat names who can open it: read it with `flats signin show` on the Flats host or in the console"); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	v, err := s.Svc.GetSignIn(r.Context(), r.PathValue("slug"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// requestSignIn requests a sign-in setting (rules 13 to 16). The approval in the answer of /api holds no entry
// (rule 58); the console keeps them.
func (s *Server) requestSignIn(w http.ResponseWriter, r *http.Request, via core.Via) {
	if err := hostCaller(r, via, "sign-in is changed with `flats signin set` or `flats signin off` on the Flats host, and each change waits for the operator in the console"); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	var body json.RawMessage
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%w: the body must be a JSON object with the member provider", core.ErrInvalid))
		return
	}
	req, err := core.DecodeSignInRequest(body)
	if err != nil {
		fail(w, err)
		return
	}
	res, err := s.Svc.RequestSignIn(r.Context(), r.PathValue("slug"), req, via)
	if err != nil {
		fail(w, err)
		return
	}
	if res.Approval != nil && via != core.ViaConsole {
		a := core.WithoutSignInEntries(*res.Approval)
		res.Approval = &a
	}
	writeActionResult(w, res)
}

// signInSecretVia allows only a caller with the CLI header on the loopback address to set or remove the client secret
// of sign-in (rule 25). The console has no route for it.
func signInSecretVia(r *http.Request, via core.Via) error {
	if via == core.ViaCLI && isLoopback(r) {
		return nil
	}
	return errors.New("the sign-in client secret is set by the operator with `flats signin secret set` and removed with `flats signin secret rm` on the Flats host")
}

func (s *Server) putSignInSecret(w http.ResponseWriter, r *http.Request, via core.Via) {
	if err := signInSecretVia(r, via); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	var in struct {
		Value    string `json:"value"`
		Issuer   string `json:"issuer"`
		ClientID string `json:"client_id"`
	}
	if err := decode(r, &in); err != nil {
		// The decoder can quote the body, which holds the value.
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%w: the body must be {\"value\": ..., \"issuer\": ..., \"client_id\": ...} with three strings; nothing changed", core.ErrInvalid))
		return
	}
	res, err := s.Svc.SetSignInSecret(r.Context(), r.PathValue("slug"), in.Value, in.Issuer, in.ClientID, via)
	if err != nil {
		fail(w, err)
		return
	}
	writeActionResult(w, res)
}

func (s *Server) deleteSignInSecret(w http.ResponseWriter, r *http.Request, via core.Via) {
	if err := signInSecretVia(r, via); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	res, err := s.Svc.DeleteSignInSecret(r.Context(), r.PathValue("slug"), via)
	if err != nil {
		fail(w, err)
		return
	}
	writeActionResult(w, res)
}

// issueSignInLink issues an access link (rules 49 to 52). Only the console can; /api has the route so that its callers
// get 403, not 404. The link is in this answer only.
func (s *Server) issueSignInLink(w http.ResponseWriter, r *http.Request, via core.Via) {
	if via != core.ViaConsole {
		writeErr(w, http.StatusForbidden, errors.New("only the console issues an access link, on the Sign-in row of the Settings tab of the flat"))
		return
	}
	subject, expires, err := decodeSignInLink(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	link, err := s.Svc.IssueSignInLink(r.Context(), r.PathValue("slug"), subject, expires, via)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, link)
}

// decodeSignInLink reads {"subject": ..., "expires": ...}. expires is an RFC 3339 time; a missing or empty member is
// the zero time, which core takes as the default of 7 days. Another member, or a value of another type, is refused.
// No message repeats the text of the caller.
func decodeSignInLink(r *http.Request) (string, time.Time, error) {
	var members map[string]json.RawMessage
	if err := decode(r, &members); err != nil {
		return "", time.Time{}, fmt.Errorf("%w: the body must be {\"subject\": ..., \"expires\": ...}", core.ErrInvalid)
	}
	for name := range members {
		if name != "subject" && name != "expires" {
			return "", time.Time{}, fmt.Errorf("%w: a request for an access link has the members subject and expires only", core.ErrInvalid)
		}
	}
	str := func(name string) (string, bool, error) {
		raw, ok := members[name]
		if !ok {
			return "", false, nil
		}
		var v string
		if string(raw) == "null" || json.Unmarshal(raw, &v) != nil {
			return "", true, fmt.Errorf("%w: member %s must be a string", core.ErrInvalid, name)
		}
		return v, true, nil
	}
	subject, ok, err := str("subject")
	if err != nil {
		return "", time.Time{}, err
	}
	if !ok {
		return "", time.Time{}, fmt.Errorf("%w: member subject is missing: a link names one subject of the allowlist", core.ErrInvalid)
	}
	raw, _, err := str("expires")
	if err != nil || raw == "" {
		return subject, time.Time{}, err
	}
	expires, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: member expires must be an RFC 3339 time, such as 2026-01-02T15:04:05Z", core.ErrInvalid)
	}
	return subject, expires, nil
}

// revokeSignIn ends every session and every access link of a flat (rule 54).
func (s *Server) revokeSignIn(w http.ResponseWriter, r *http.Request, via core.Via) {
	if err := hostCaller(r, via, "the sessions and access links of a flat are ended with `flats signin revoke` on the Flats host or in the console"); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	res, err := s.Svc.RevokeSignIn(r.Context(), r.PathValue("slug"), via)
	if err != nil {
		fail(w, err)
		return
	}
	writeActionResult(w, res)
}
