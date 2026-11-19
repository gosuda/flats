package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The bindings of docs/spec/injected-fetch-secrets.md: an injected secret is a flat secret with an origin and a
// header. The host adds the header to each fetch of the flat to that origin, and the value never goes into env.

// MaxInjectedSecrets is the number of injected secrets that one flat can have (rule 8).
const MaxInjectedSecrets = 32

// Binding is the origin and the header of an injected secret. The zero Binding is a readable secret. The store
// keeps the origin normalized and the header canonical, as the service gives them.
type Binding struct {
	Origin string
	Header string
}

// Injected reports whether a row is an injected secret: one of its two binding columns is not empty (rule 1).
func (b Binding) Injected() bool { return b.Origin != "" || b.Header != "" }

// Same reports whether two bindings are the same (rule 3).
func (b Binding) Same(o Binding) bool {
	return b.Origin == o.Origin && strings.EqualFold(b.Header, o.Header)
}

// SecretPut says how PutSecretChecked may write a secret.
type SecretPut struct {
	// Inject is the binding that the set names, or nil.
	Inject *Binding
	// HeaderValue says that the caller checked the plain value, and that it can be the value of an injected
	// secret. The store has no key, so it cannot check the value itself.
	HeaderValue bool
}

// InjectReason is why PutSecretChecked refused a set.
type InjectReason int

const (
	// InjectReadable: the set names a binding, and the name holds a readable secret.
	InjectReadable InjectReason = iota + 1
	// InjectOtherBinding: the set names a binding, and the name holds an injected secret with another binding.
	InjectOtherBinding
	// InjectTaken: another injected secret of the flat has the binding.
	InjectTaken
	// InjectLimit: the flat has MaxInjectedSecrets injected secrets.
	InjectLimit
	// InjectValue: the secret is injected, and the value cannot be a header value.
	InjectValue
)

// InjectRefusal is a set that rules 7 and 8 refuse. The write transaction changed nothing.
type InjectRefusal struct {
	Reason InjectReason
	// Secret is the other secret for InjectTaken.
	Secret string
	// Binding is the stored binding for InjectOtherBinding and InjectValue, and the binding of the set for
	// InjectTaken and for a new name.
	Binding Binding
	// New says that nothing was stored under the name.
	New bool
}

func (e *InjectRefusal) Error() string {
	switch e.Reason {
	case InjectReadable:
		return "the secret is readable"
	case InjectOtherBinding:
		return fmt.Sprintf("the secret is sent as %s to %s", e.Binding.Header, e.Binding.Origin)
	case InjectTaken:
		return fmt.Sprintf("secret %q is sent as %s to %s", e.Secret, e.Binding.Header, e.Binding.Origin)
	case InjectLimit:
		return fmt.Sprintf("the flat has %d injected secrets", MaxInjectedSecrets)
	default:
		return "the value cannot be the value of an injected secret"
	}
}

// addFetchBindings is the migration of rule 32. Every row gets the empty binding, so it stays a readable secret. The
// unique index refuses a second injected row of a flat with the same binding (rule 9).
func addFetchBindings(ctx context.Context, x executor) error {
	if err := addColumn(ctx, x, "secrets", "fetch_origin", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumn(ctx, x, "secrets", "fetch_header", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	_, err := x.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS secrets_fetch_binding
 ON secrets(flat, fetch_origin, fetch_header COLLATE NOCASE) WHERE fetch_origin <> '' OR fetch_header <> ''`)
	return err
}

// PutSecretChecked stores an encrypted secret by rules 2, 7, 8 and 9 of the spec, and writes its secret.set audit
// event, in one write transaction. It returns the binding of the stored row. A refusal is an *InjectRefusal, and it
// changes nothing. The client secret of sign-in is not an ordinary secret: a set of its name gets ErrSignInSecretRow
// (docs/spec/portal-sign-in.md, rule 30).
func (s *Store) PutSecretChecked(ctx context.Context, flat string, sec SealedSecret, put SecretPut, a Audit) (Binding, error) {
	var result Binding
	_, err := s.immediate(ctx, func(x executor) (bool, error) {
		uid, err := flatUID(ctx, x, flat)
		if err != nil {
			return false, err
		}
		var stored Binding
		var use string
		existed := true
		err = x.QueryRowContext(ctx, `SELECT fetch_origin, fetch_header, use FROM secrets WHERE flat=? AND name=?`, flat, sec.Name).
			Scan(&stored.Origin, &stored.Header, &use)
		if errors.Is(err, sql.ErrNoRows) {
			existed = false
		} else if err != nil {
			return false, err
		}
		if use != "" {
			return false, ErrSignInSecretRow
		}
		result = Binding{}
		switch {
		case !existed && put.Inject == nil:
		case !existed:
			if err := checkNewBinding(ctx, x, flat, sec.Name, *put.Inject, put.HeaderValue); err != nil {
				return false, err
			}
			result = *put.Inject
		case !stored.Injected() && put.Inject != nil:
			return false, &InjectRefusal{Reason: InjectReadable}
		case !stored.Injected():
		case put.Inject != nil && !put.Inject.Same(stored):
			return false, &InjectRefusal{Reason: InjectOtherBinding, Binding: stored}
		case !put.HeaderValue:
			return false, &InjectRefusal{Reason: InjectValue, Binding: stored}
		default:
			result = stored
		}
		// A replaced value keeps the binding of the row: the update writes no binding column.
		if _, err := x.ExecContext(ctx, `INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at,key_id,fetch_origin,fetch_header) VALUES(?,?,?,?,?,?,?,?)
 ON CONFLICT(flat,name) DO UPDATE SET nonce=excluded.nonce, ciphertext=excluded.ciphertext, updated_at=excluded.updated_at, key_id=excluded.key_id`,
			flat, sec.Name, sec.Nonce, sec.Ciphertext, unix(sec.UpdatedAt), sec.KeyID, result.Origin, result.Header); err != nil {
			return false, envWriteError(err)
		}
		e := a.event(AuditSecretSet)
		e.Flat, e.FlatID, e.Name = flat, uid, sec.Name
		data := map[string]any{"created": !existed}
		if result.Injected() {
			data["origin"], data["header"] = result.Origin, result.Header
		}
		e.Data = AuditData(data)
		return true, appendAudit(ctx, x, e)
	})
	if err != nil {
		return Binding{}, err
	}
	return result, nil
}

// checkNewBinding applies rules 7 and 8 to a new injected secret, inside the write transaction.
func checkNewBinding(ctx context.Context, x executor, flat, name string, b Binding, headerValue bool) error {
	if !headerValue {
		return &InjectRefusal{Reason: InjectValue, Binding: b, New: true}
	}
	var other string
	err := x.QueryRowContext(ctx, `SELECT name FROM secrets WHERE flat=? AND name<>? AND fetch_origin=? AND fetch_header=? COLLATE NOCASE ORDER BY name LIMIT 1`,
		flat, name, b.Origin, b.Header).Scan(&other)
	if err == nil {
		return &InjectRefusal{Reason: InjectTaken, Secret: other, Binding: b, New: true}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var n int
	if err := x.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets WHERE flat=? AND use='' AND (fetch_origin<>'' OR fetch_header<>'')`, flat).Scan(&n); err != nil {
		return err
	}
	if n >= MaxInjectedSecrets {
		return &InjectRefusal{Reason: InjectLimit, New: true}
	}
	return nil
}
