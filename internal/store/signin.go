package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The sign-in of docs/spec/portal-sign-in.md: the setting of a flat is five columns of flats, and the client secret of
// an oidc setting is the row of secrets with the use signin.

// Providers of a sign-in setting (rule 1). The empty provider is sign-in off.
const (
	SignInOIDC       = "oidc"
	SignInCredential = "credential"
)

// SignInSecretName is the name of the client secret of sign-in (rule 24). It is not a valid name of an environment
// variable, so `flats secret set` cannot make a readable secret of that name.
const SignInSecretName = "oidc-client-secret"

// SecretUseSignIn is the use of the row of the client secret. Every other row has the empty use (rule 72).
const SecretUseSignIn = "signin"

// Kinds of the audit events of sign-in (rules 65 and 67).
const (
	AuditSignInSet    = "signin.set"
	AuditSignInLink   = "signin.link"
	AuditSignInRevoke = "signin.revoke"
)

// Causes of a signin.revoke event that a new token secret of a rename or a deletion writes (rule 67).
const (
	SignInRevokeRename = "rename"
	SignInRevokeDelete = "delete"
)

// ErrSignInSecretName refuses a set of the client secret while a readable secret has its name (rule 31).
var ErrSignInSecretName = errors.New("a readable secret has the name of the sign-in client secret")

// ErrSignInSecretRow refuses a change of the client secret through the commands of ordinary secrets (rule 31).
var ErrSignInSecretRow = errors.New("the secret is the client secret of sign-in")

// SignIn is the sign-in setting of a flat. The store keeps it as the service gives it: normalized by rules 2 to 4,
// with Allow in byte order and without duplicates.
type SignIn struct {
	Provider string   `json:"provider"`
	Issuer   string   `json:"issuer,omitempty"`
	ClientID string   `json:"client_id,omitempty"`
	Allow    []string `json:"allow"`
}

// MarshalJSON writes an empty list as [], so that a frozen setting has no null member.
func (s SignIn) MarshalJSON() ([]byte, error) {
	type plain SignIn
	if s.Allow == nil {
		s.Allow = []string{}
	}
	return json.Marshal(plain(s))
}

// On reports whether sign-in is on.
func (s SignIn) On() bool { return s.Provider != "" }

// Same reports whether two settings are the same (rule 4).
func (s SignIn) Same(o SignIn) bool {
	return s.Provider == o.Provider && s.Issuer == o.Issuer && s.ClientID == o.ClientID && slices.Equal(s.Allow, o.Allow)
}

// SignInBinding is the issuer and the client ID that the client secret is bound to (rule 27).
type SignInBinding struct {
	Issuer   string
	ClientID string
}

// SignInSecretInfo is what the views may show of the client secret: its binding and the time of its set.
type SignInSecretInfo struct {
	SignInBinding
	UpdatedAt time.Time
}

// SignInState is the stored sign-in of a flat, read in one transaction.
type SignInState struct {
	FlatID         string
	Visibility     Visibility
	PublicInstance bool
	SignIn         SignIn
	Seq            int64
	// Secret is nil when no client secret is stored.
	Secret *SignInSecretInfo
	// PublicGrants are the public providers that the flat has a grant for, in byte order (rule 16).
	PublicGrants []string
}

// SignInExcludes reports whether provider is a public provider other than Portal, which the visitors reach without
// a sign-in (rule 8).
func SignInExcludes(provider string) bool {
	switch provider {
	case ProviderFunnel, ProviderZrok, ProviderCFQuick, ProviderNetbirdExpose, ProviderProxyPublic:
		return true
	}
	return false
}

// SignInReason is why a write refused, or found stale, a change that sign-in concerns.
type SignInReason int

const (
	// SignInGrantRefused: a grant of a provider that SignInExcludes while sign-in is on (rule 8, first case).
	SignInGrantRefused SignInReason = iota + 1
	// SignInPublicRefused: a change to Public while sign-in is on and such a provider is granted (rule 8, second case).
	SignInPublicRefused
	// SignInOnRefused: sign-in on while such a provider is granted (rule 8, third case).
	SignInOnRefused
	// SignInOtherFlat: the flat was deleted and created again after the request (rule 18).
	SignInOtherFlat
	// SignInSeqChanged: another sign-in change applied after the request (rules 18 and 19).
	SignInSeqChanged
	// SignInGrantsChanged: a public provider was granted or revoked after the request (rule 18).
	SignInGrantsChanged
)

// SignInConflict is a write that sign-in refused or found stale. The write transaction changed nothing.
type SignInConflict struct {
	Reason SignInReason
	// Providers are the granted providers that SignInExcludes, in byte order, or the provider of a refused grant.
	Providers []string
	// Public and PublicInstance describe the flat, so that the refusal names the next action (rules 8 and 73).
	Public         bool
	PublicInstance bool
	// Seq is the stored sequence for SignInSeqChanged, and Grants the stored public grants for SignInGrantsChanged.
	Seq    int64
	Grants []string
}

func (e *SignInConflict) Error() string {
	switch e.Reason {
	case SignInGrantRefused:
		return "sign-in is on, and " + strings.Join(e.Providers, ", ") + " serves visitors without it"
	case SignInPublicRefused, SignInOnRefused:
		return "sign-in cannot be on while " + strings.Join(e.Providers, ", ") + " is granted"
	case SignInOtherFlat:
		return "the flat was deleted and created again"
	case SignInSeqChanged:
		return fmt.Sprintf("the sign-in changed (sequence %d)", e.Seq)
	default:
		return "the public grants changed"
	}
}

// addSignIn is the migration of rule 72. Every flat gets sign-in off, and every secret the empty use, also a row with
// the name of the client secret, which stays a readable secret. The unique index keeps one client secret per flat.
func addSignIn(ctx context.Context, x executor) error {
	for _, c := range []struct{ table, col, decl string }{
		{"flats", "signin_provider", "TEXT NOT NULL DEFAULT ''"},
		{"flats", "signin_issuer", "TEXT NOT NULL DEFAULT ''"},
		{"flats", "signin_client_id", "TEXT NOT NULL DEFAULT ''"},
		{"flats", "signin_allow", "TEXT NOT NULL DEFAULT '[]'"},
		{"flats", "signin_seq", "INTEGER NOT NULL DEFAULT 0"},
		{"secrets", "use", "TEXT NOT NULL DEFAULT '' CHECK (use IN ('', 'signin'))"},
		{"secrets", "signin_issuer", "TEXT NOT NULL DEFAULT ''"},
		{"secrets", "signin_client_id", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumn(ctx, x, c.table, c.col, c.decl); err != nil {
			return err
		}
	}
	_, err := x.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS secrets_signin ON secrets(flat) WHERE use='signin'`)
	return err
}

// publicGrantsQuery lists the public providers that a flat has a grant for. ORDER BY uses the binary collation, which
// is byte order.
const publicGrantsQuery = `SELECT provider FROM flat_providers WHERE flat=? AND permitted=1 AND provider IN ('` +
	ProviderFunnel + `','` + ProviderPortal + `','` + ProviderZrok + `','` + ProviderCFQuick + `','` + ProviderNetbirdExpose + `','` + ProviderProxyPublic +
	`') ORDER BY provider`

func publicGrants(ctx context.Context, x executor, flat string) ([]string, error) {
	rows, err := x.QueryContext(ctx, publicGrantsQuery, flat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// excluded returns the grants that SignInExcludes.
func excluded(grants []string) []string {
	var out []string
	for _, g := range grants {
		if SignInExcludes(g) {
			out = append(out, g)
		}
	}
	return out
}

// PublicGrants returns the public providers that a flat has a grant for, in byte order.
func (s *Store) PublicGrants(ctx context.Context, flat string) ([]string, error) {
	return publicGrants(ctx, s.db, flat)
}

// signInRow is the flat row as a sign-in write reads it.
type signInRow struct {
	uid            string
	visibility     Visibility
	publicInstance bool
	in             SignIn
	seq            int64
}

func readSignInRow(ctx context.Context, x executor, flat string) (signInRow, error) {
	var r signInRow
	var uid sql.NullString
	var vis, allow string
	err := x.QueryRowContext(ctx, `SELECT uid,visibility,created_public_instance,signin_provider,signin_issuer,signin_client_id,signin_allow,signin_seq FROM flats WHERE slug=?`, flat).
		Scan(&uid, &vis, &r.publicInstance, &r.in.Provider, &r.in.Issuer, &r.in.ClientID, &allow, &r.seq)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("flat %q: %w", flat, ErrNotFound)
	}
	if err != nil {
		return r, err
	}
	r.uid, r.visibility = uid.String, Visibility(vis)
	if err := json.Unmarshal([]byte(allow), &r.in.Allow); err != nil || r.in.Allow == nil {
		return r, fmt.Errorf("flat %q: the stored sign-in allowlist cannot be read", flat)
	}
	return r, nil
}

// GetSignIn reads the sign-in of a flat, the binding of its client secret and its public grants in one transaction.
func (s *Store) GetSignIn(ctx context.Context, flat string) (SignInState, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SignInState{}, err
	}
	defer tx.Rollback()
	r, err := readSignInRow(ctx, tx, flat)
	if err != nil {
		return SignInState{}, err
	}
	st := SignInState{FlatID: r.uid, Visibility: r.visibility, PublicInstance: r.publicInstance, SignIn: r.in, Seq: r.seq}
	var info SignInSecretInfo
	var at int64
	err = tx.QueryRowContext(ctx, `SELECT signin_issuer,signin_client_id,updated_at FROM secrets WHERE flat=? AND use='signin'`, flat).
		Scan(&info.Issuer, &info.ClientID, &at)
	switch {
	case err == nil:
		info.UpdatedAt = fromUnix(at)
		st.Secret = &info
	case !errors.Is(err, sql.ErrNoRows):
		return SignInState{}, err
	}
	if st.PublicGrants, err = publicGrants(ctx, tx, flat); err != nil {
		return SignInState{}, err
	}
	return st, nil
}

// signInOn reports whether the flat has sign-in on, inside a write transaction. A flat that does not exist has none.
func signInOn(ctx context.Context, x executor, flat string) (bool, error) {
	var provider string
	err := x.QueryRowContext(ctx, `SELECT signin_provider FROM flats WHERE slug=?`, flat).Scan(&provider)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return provider != "", err
}

// refusePublic is the check of rule 8, second case, inside the transaction of a change to Public.
func refusePublic(ctx context.Context, x executor, flat string) error {
	on, err := signInOn(ctx, x, flat)
	if err != nil || !on {
		return err
	}
	grants, err := publicGrants(ctx, x, flat)
	if err != nil {
		return err
	}
	if others := excluded(grants); len(others) > 0 {
		return &SignInConflict{Reason: SignInPublicRefused, Providers: others}
	}
	return nil
}

// SignInCommit is an approved sign-in change: step 2 of rule 20. FlatID, Seq and Grants are the frozen ones.
type SignInCommit struct {
	Flat     string
	FlatID   string
	Seq      int64
	Grants   []string
	To       SignIn
	Approval string
	Receipt  json.RawMessage
	Audit    Audit
}

// CommitSignIn writes the setting, the next sequence, the receipt of the approval and the signin.set audit event of
// rule 65 in one write transaction, only while the flat ID, the sequence and the public grants are the frozen ones and
// rule 8 allows the setting (rules 9 and 18). It returns the setting that it replaced. A refusal is a
// *SignInConflict, and it changes nothing.
func (s *Store) CommitSignIn(ctx context.Context, c SignInCommit) (SignIn, error) {
	var from SignIn
	_, err := s.immediate(ctx, func(x executor) (bool, error) {
		r, err := readSignInRow(ctx, x, c.Flat)
		if err != nil {
			return false, err
		}
		if r.uid != c.FlatID {
			return false, &SignInConflict{Reason: SignInOtherFlat}
		}
		if r.seq != c.Seq {
			return false, &SignInConflict{Reason: SignInSeqChanged, Seq: r.seq}
		}
		grants, err := publicGrants(ctx, x, c.Flat)
		if err != nil {
			return false, err
		}
		if !slices.Equal(grants, c.Grants) {
			return false, &SignInConflict{Reason: SignInGrantsChanged, Grants: grants}
		}
		if others := excluded(grants); c.To.On() && len(others) > 0 {
			return false, &SignInConflict{Reason: SignInOnRefused, Providers: others, Public: r.visibility.Public(), PublicInstance: r.publicInstance}
		}
		allow := c.To.Allow
		if allow == nil {
			allow = []string{}
		}
		list, err := json.Marshal(allow)
		if err != nil {
			return false, err
		}
		res, err := x.ExecContext(ctx, `UPDATE flats SET signin_provider=?,signin_issuer=?,signin_client_id=?,signin_allow=?,signin_seq=signin_seq+1 WHERE slug=? AND signin_seq=?`,
			c.To.Provider, c.To.Issuer, c.To.ClientID, string(list), c.Flat, c.Seq)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return false, errors.New("sign-in changed concurrently")
		}
		res, err = x.ExecContext(ctx, `UPDATE approvals SET result_data=? WHERE id=? AND status='applying'`, string(c.Receipt), c.Approval)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return false, errors.New("approval no longer applying")
		}
		e := c.Audit.event(AuditSignInSet)
		e.Flat, e.FlatID = c.Flat, r.uid
		e.Data = signInSetData(r.in, c.To, c.Approval)
		if err := appendAudit(ctx, x, e); err != nil {
			return false, err
		}
		from = r.in
		return true, nil
	})
	if err != nil {
		return SignIn{}, err
	}
	return from, nil
}

// signInSide is one side of the data of a signin.set event: the setting without its entries.
type signInSide struct {
	Provider string `json:"provider"`
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	Count    int    `json:"count"`
}

func sideOf(in SignIn) signInSide {
	return signInSide{Provider: in.Provider, Issuer: in.Issuer, ClientID: in.ClientID, Count: len(in.Allow)}
}

type signInSetRow struct {
	From      signInSide `json:"from"`
	To        signInSide `json:"to"`
	Added     []string   `json:"added"`
	Removed   []string   `json:"removed"`
	Approval  string     `json:"approval"`
	Truncated bool       `json:"truncated,omitempty"`
}

// cutIssuer is the length that each issuer of a signin.set row keeps when the two issuers alone do not fit: two
// issuers of the longest form that rule 2 allows, 2048 bytes each, are the whole data of a row.
const cutIssuer = 512

// signInSetData is the data of a signin.set event (rule 65). When added and removed do not fit, the row keeps the
// first entries that fit and says truncated, as network.set does. When the two issuers do not fit either, each keeps
// its first cutIssuer bytes, under the same mark: the change must be able to apply, and the row must be written in its
// transaction.
func signInSetData(from, to SignIn, approval string) json.RawMessage {
	row := signInSetRow{From: sideOf(from), To: sideOf(to), Approval: approval}
	row.Added, row.Removed = diffSorted(to.Allow, from.Allow), diffSorted(from.Allow, to.Allow)
	if b := auditJSON(row); len(b) <= MaxAuditData {
		return b
	}
	added, removed := row.Added, row.Removed
	row.Added, row.Removed, row.Truncated = []string{}, []string{}, true
	base := auditJSON(row)
	if len(base) > MaxAuditData {
		row.From.Issuer, row.To.Issuer = row.From.Issuer[:min(len(row.From.Issuer), cutIssuer)], row.To.Issuer[:min(len(row.To.Issuer), cutIssuer)]
		base = auditJSON(row)
	}
	room := MaxAuditData - len(base)
	row.Added, room = fitEntries(added, room)
	if len(row.Added) == len(added) {
		row.Removed, _ = fitEntries(removed, room)
	}
	return auditJSON(row)
}

// fitEntries returns the first entries whose JSON text, with the commas between them, has at most room bytes, and the
// room that is left.
func fitEntries(entries []string, room int) ([]string, int) {
	for i, e := range entries {
		size := len(auditJSON(e))
		if i > 0 {
			size++
		}
		if size > room {
			return entries[:i], room
		}
		room -= size
	}
	return entries, room
}

// diffSorted returns the entries of a that b does not have. Both lists are in byte order.
func diffSorted(a, b []string) []string {
	out := []string{}
	j := 0
	for _, e := range a {
		for j < len(b) && b[j] < e {
			j++
		}
		if j < len(b) && b[j] == e {
			continue
		}
		out = append(out, e)
	}
	return out
}

// auditJSON encodes v without the HTML escapes of encoding/json, which make an & or a < six bytes: an issuer and an
// entry may hold them, and the data of a row has at most MaxAuditData bytes.
func auditJSON(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// SignInSecret returns the sealed client secret of a flat with its binding, and whether there is one.
func (s *Store) SignInSecret(ctx context.Context, flat string) (SealedSecret, bool, error) {
	var sec SealedSecret
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT name,nonce,ciphertext,updated_at,key_id,use,signin_issuer,signin_client_id FROM secrets WHERE flat=? AND use='signin'`, flat).
		Scan(&sec.Name, &sec.Nonce, &sec.Ciphertext, &at, &sec.KeyID, &sec.Use, &sec.SignIn.Issuer, &sec.SignIn.ClientID)
	if errors.Is(err, sql.ErrNoRows) {
		return SealedSecret{}, false, nil
	}
	if err != nil {
		return SealedSecret{}, false, err
	}
	sec.UpdatedAt = fromUnix(at)
	return sec, true, nil
}

// PutSignInSecret stores the client secret of a flat with its binding and writes its secret.set audit event in one
// write transaction (rules 25 and 66). It reports whether the flat had no client secret before. A readable secret with
// the name of the client secret refuses the set with ErrSignInSecretName and changes nothing (rule 31). sec.Name is
// not read: the row always has SignInSecretName.
func (s *Store) PutSignInSecret(ctx context.Context, flat string, sec SealedSecret, b SignInBinding, a Audit) (bool, error) {
	created := false
	_, err := s.immediate(ctx, func(x executor) (bool, error) {
		uid, err := flatUID(ctx, x, flat)
		if err != nil {
			return false, err
		}
		if uid == "" {
			return false, fmt.Errorf("flat %q: %w", flat, ErrNotFound)
		}
		var use string
		err = x.QueryRowContext(ctx, `SELECT use FROM secrets WHERE flat=? AND name=?`, flat, SignInSecretName).Scan(&use)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			created = true
		case err != nil:
			return false, err
		case use != SecretUseSignIn:
			return false, ErrSignInSecretName
		}
		if _, err := x.ExecContext(ctx, `INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at,key_id,use,signin_issuer,signin_client_id) VALUES(?,?,?,?,?,?,?,?,?)
 ON CONFLICT(flat,name) DO UPDATE SET nonce=excluded.nonce, ciphertext=excluded.ciphertext, updated_at=excluded.updated_at, key_id=excluded.key_id,
 signin_issuer=excluded.signin_issuer, signin_client_id=excluded.signin_client_id`,
			flat, SignInSecretName, sec.Nonce, sec.Ciphertext, unix(sec.UpdatedAt), sec.KeyID, SecretUseSignIn, b.Issuer, b.ClientID); err != nil {
			return false, envWriteError(err)
		}
		e := a.event(AuditSecretSet)
		e.Flat, e.FlatID, e.Name = flat, uid, SignInSecretName
		e.Data = auditJSON(map[string]any{"created": created, "use": SecretUseSignIn, "issuer": b.Issuer, "client_id": b.ClientID})
		return true, appendAudit(ctx, x, e)
	})
	return created && err == nil, err
}

// DeleteSignInSecret removes the client secret of a flat and writes its secret.delete audit event in one write
// transaction (rule 66). A flat without one changes nothing and gets ErrNotFound.
func (s *Store) DeleteSignInSecret(ctx context.Context, flat string, a Audit) error {
	_, err := s.immediate(ctx, func(x executor) (bool, error) {
		uid, err := flatUID(ctx, x, flat)
		if err != nil {
			return false, err
		}
		res, err := x.ExecContext(ctx, `DELETE FROM secrets WHERE flat=? AND use='signin'`, flat)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return false, fmt.Errorf("sign-in client secret of %q: %w", flat, ErrNotFound)
		}
		e := a.event(AuditSecretDelete)
		e.Flat, e.FlatID, e.Name = flat, uid, SignInSecretName
		e.Data = signInUseData(nil)
		return true, appendAudit(ctx, x, e)
	})
	return err
}

// signInUseData adds "use": "signin" to the fields of a secret.delete event (rule 66).
func signInUseData(fields map[string]any) json.RawMessage {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["use"] = SecretUseSignIn
	return AuditData(fields)
}

// SignInLink is what the signin.link event of an issued access link records (rule 67). It holds no credential and no
// link.
type SignInLink struct {
	Subject   string
	Host      string
	ExpiresAt time.Time
}

// SignInLinkData is the data of a signin.link event: the subject, the host name and the expiry in UTC, in RFC 3339.
func SignInLinkData(l SignInLink) json.RawMessage {
	return auditJSON(map[string]any{"subject": l.Subject, "host": l.Host, "expires_at": l.ExpiresAt.UTC().Format(time.RFC3339)})
}

// SignInRevokeData is the data of a signin.revoke event: {} for a revocation of the operator, and the cause of a new
// token secret that a rename or a deletion gave (SignInRevokeRename, SignInRevokeDelete).
func SignInRevokeData(cause string) json.RawMessage {
	if cause == "" {
		return json.RawMessage(`{}`)
	}
	return AuditData(map[string]any{"cause": cause})
}

// AppendSignInLink writes the signin.link event of an access link of a flat.
func (s *Store) AppendSignInLink(ctx context.Context, flat string, l SignInLink, a Audit) error {
	return s.appendFlatAudit(ctx, flat, AuditSignInLink, SignInLinkData(l), a)
}

// AppendSignInRevoke writes the signin.revoke event of a revocation of the operator (rule 54), after the write of the
// identity file passed. The new token secret of a rename or a deletion writes its event in the transaction of that
// change instead, with SignInRevokeData of its cause.
func (s *Store) AppendSignInRevoke(ctx context.Context, flat string, a Audit) error {
	return s.appendFlatAudit(ctx, flat, AuditSignInRevoke, SignInRevokeData(""), a)
}

func (s *Store) appendFlatAudit(ctx context.Context, flat, kind string, data json.RawMessage, a Audit) error {
	_, err := s.immediate(ctx, func(x executor) (bool, error) {
		uid, err := flatUID(ctx, x, flat)
		if err != nil {
			return false, err
		}
		e := a.event(kind)
		e.Flat, e.FlatID, e.Data = flat, uid, data
		return true, appendAudit(ctx, x, e)
	})
	return err
}
