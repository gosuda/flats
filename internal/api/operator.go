package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"time"
)

const operatorCookie = "flats_operator"
const operatorSessionLifetime = 8 * time.Hour

// OperatorAuthority authenticates management decisions independently of agent
// access and browser headers. The embedding application must provision the
// credential through an operator-controlled channel unavailable to agents.
// It must never generate or return that credential from an HTTP read or tool.
// This is credential separation, not protection from an OS administrator or an
// agent that can read the operator's secrets or the server/browser memory.
type OperatorAuthority struct {
	credential [32]byte
	mu         sync.Mutex
	sessions   map[[32]byte]time.Time
}

// NewOperatorAuthority accepts a separately provisioned high-entropy credential
// (at least 32 bytes). Only its hash and hashes of session cookies are retained.
// Constructing a new authority invalidates existing sessions, including restart.
func NewOperatorAuthority(credential string) (*OperatorAuthority, error) {
	if len(credential) < 32 {
		return nil, errors.New("operator credential must contain at least 32 bytes of independently provisioned entropy")
	}
	return &OperatorAuthority{credential: sha256.Sum256([]byte(credential)), sessions: make(map[[32]byte]time.Time)}, nil
}

type operatorContextKey struct{}

type operatorProof struct {
	authority *OperatorAuthority
	session   [32]byte
}

// ValidateDecision is the core.Config.ValidateOperatorDecision callback. A
// request proof is created only after a valid console session, never from Via,
// client headers, tailnet identity, or an arbitrary context string.
func (a *OperatorAuthority) ValidateDecision(ctx context.Context) error {
	proof, ok := ctx.Value(operatorContextKey{}).(operatorProof)
	if a == nil || !ok || proof.authority != a || !a.validSession(proof.session) {
		return errors.New("validated operator decision authority required")
	}
	return nil
}

func (a *OperatorAuthority) validSession(hash [32]byte) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	expiry, ok := a.sessions[hash]
	if !ok || !time.Now().Before(expiry) {
		delete(a.sessions, hash)
		return false
	}
	return true
}

func (a *OperatorAuthority) authorize(r *http.Request) bool {
	if a == nil {
		return false
	}
	cookie, err := r.Cookie(operatorCookie)
	if err != nil || len(cookie.Value) != 43 {
		return false
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	if !a.validSession(hash) {
		return false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), operatorContextKey{}, operatorProof{a, hash}))
	return true
}

func operatorError(w http.ResponseWriter, category, message string) {
	writeJSON(w, http.StatusForbidden, ErrorBody{Error: message, Category: category})
}

func (s *Server) requireOperator(w http.ResponseWriter, r *http.Request) bool {
	if s.Operator == nil {
		operatorError(w, "operator_unavailable", "operator authority is not configured; decisions are disabled")
		return false
	}
	if !s.Operator.authorize(r) {
		operatorError(w, "operator_required", "a separately authorized operator session is required")
		return false
	}
	return true
}

// operatorSession validates an operator-entered credential. Headers provide
// CSRF protection only; they never establish operator authority.
func (s *Server) operatorSession(w http.ResponseWriter, r *http.Request) {
	if err := consoleRequest(r); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	if s.Operator == nil {
		operatorError(w, "operator_unavailable", "operator authority is not configured; decisions are disabled")
		return
	}
	if r.TLS == nil && !isLoopback(r) {
		operatorError(w, "operator_secure_transport_required", "operator credentials require HTTPS or a loopback connection")
		return
	}
	var in struct {
		Credential string `json:"credential"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hash := sha256.Sum256([]byte(in.Credential))
	if subtle.ConstantTimeCompare(hash[:], s.Operator.credential[:]) != 1 {
		operatorError(w, "invalid_operator_credential", "operator credential was not accepted")
		return
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeErr(w, http.StatusInternalServerError, errors.New("could not establish operator session"))
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	expiry := time.Now().Add(operatorSessionLifetime)
	a := s.Operator
	a.mu.Lock()
	for key, until := range a.sessions {
		if !time.Now().Before(until) {
			delete(a.sessions, key)
		}
	}
	if len(a.sessions) >= 64 {
		a.mu.Unlock()
		writeErr(w, http.StatusTooManyRequests, errors.New("operator session limit reached; sign out an existing session or wait for expiry"))
		return
	}
	a.sessions[sha256.Sum256([]byte(token))] = expiry
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: operatorCookie, Value: token, Path: "/console/api", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: int(operatorSessionLifetime.Seconds())})
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "operator_authorized"})
}

func (s *Server) operatorLogout(w http.ResponseWriter, r *http.Request) {
	if err := consoleRequest(r); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	if !s.requireOperator(w, r) {
		return
	}
	cookie, _ := r.Cookie(operatorCookie)
	s.Operator.mu.Lock()
	delete(s.Operator.sessions, sha256.Sum256([]byte(cookie.Value)))
	s.Operator.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: operatorCookie, Path: "/console/api", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "operator_signed_out"})
}

// DecisionIdentity is the nonsecret audit identity for a validated operator
// proof. Tailnet login is supplemental audit context from trusted middleware,
// never the source of authority. Local sessions share the configured operator
// credential and therefore identify a role, not an individually verified human.
func (a *OperatorAuthority) DecisionIdentity(ctx context.Context) string {
	if a.ValidateDecision(ctx) != nil {
		return ""
	}
	if login := approverOf(ctx); login != "" {
		return login
	}
	return "local operator"
}
