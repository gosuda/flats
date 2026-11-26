package store

import (
	"context"
	"errors"
	"fmt"
)

// SealedValue is one sealed value as a re-seal reads it: a flat secret, or the value of an operator key when Flat is
// "", and Name is then the id of the key.
type SealedValue struct {
	Flat, Name        string
	Nonce, Ciphertext []byte
}

func (v SealedValue) String() string {
	if v.Flat == "" {
		return "operator key " + v.Name
	}
	return "secret " + v.Name + " of " + v.Flat
}

// Reseal describes a rotation of the host key to Store.Reseal. From and To are the key IDs of the old and of the new
// key. Out says that the new key went to a file of the operator and not to the key path, which the event records.
// Seal opens one value with the old key and returns it sealed with the new key under a new nonce. The store has no
// key, so the caller gives the function.
type Reseal struct {
	From, To string
	Out      bool
	Seal     func(SealedValue) (nonce, ciphertext []byte, err error)
	Audit    Audit
}

// Reseal seals every sealed value again in one transaction (docs/spec/secret-key-rotation.md, rule 15): the flat
// secrets in the order of flat and name, then the operator key records that hold a value in the order of their id.
// Each value gets the nonce and the ciphertext that r.Seal returns and the key ID r.To. The time of the last change,
// the source and the last error stay, because the value did not change, and a record without a value is not touched.
// The transaction also writes the key.rotate event (rule 30), with the counts that Reseal returns, also when there
// is no sealed value. An error of r.Seal, of a write or of the commit leaves every row as it was and writes no event.
func (s *Store) Reseal(ctx context.Context, r Reseal) (secrets, keys int, err error) {
	if r.Seal == nil {
		return 0, 0, errors.New("reseal: no function that seals a value")
	}
	if r.To == "" {
		return 0, 0, errors.New("reseal: no key ID of the new key")
	}
	_, err = s.immediate(ctx, func(x executor) (bool, error) {
		// Each list is read whole before the first write of its table: one connection holds the transaction.
		values, err := sealedValues(ctx, x, `SELECT flat, name, nonce, ciphertext FROM secrets ORDER BY flat, name`)
		if err != nil {
			return false, err
		}
		for _, v := range values {
			nonce, ciphertext, err := resealed(r, v)
			if err != nil {
				return false, err
			}
			if _, err := x.ExecContext(ctx, `UPDATE secrets SET nonce=?, ciphertext=?, key_id=? WHERE flat=? AND name=?`,
				nonce, ciphertext, r.To, v.Flat, v.Name); err != nil {
				return false, err
			}
		}
		secrets = len(values)
		values, err = sealedValues(ctx, x, `SELECT '', id, nonce, ciphertext FROM host_keys WHERE ciphertext IS NOT NULL ORDER BY id`)
		if err != nil {
			return false, err
		}
		for _, v := range values {
			nonce, ciphertext, err := resealed(r, v)
			if err != nil {
				return false, err
			}
			if _, err := x.ExecContext(ctx, `UPDATE host_keys SET nonce=?, ciphertext=?, key_id=? WHERE id=?`,
				nonce, ciphertext, r.To, v.Name); err != nil {
				return false, err
			}
		}
		keys = len(values)
		e := r.Audit.event(AuditKeyRotate)
		e.Data = AuditData(map[string]any{"from": r.From, "to": r.To, "secrets": secrets, "keys": keys, "out": r.Out})
		return true, appendAudit(ctx, x, e)
	})
	if err != nil {
		return 0, 0, err
	}
	return secrets, keys, nil
}

// resealed calls r.Seal for v. An empty nonce or ciphertext is an error: written to an operator key record, it would
// turn the record into one without a value.
func resealed(r Reseal, v SealedValue) (nonce, ciphertext []byte, err error) {
	nonce, ciphertext, err = r.Seal(v)
	if err != nil {
		return nil, nil, err
	}
	if len(nonce) == 0 || len(ciphertext) == 0 {
		return nil, nil, fmt.Errorf("reseal: the new seal of %s is empty", v)
	}
	return nonce, ciphertext, nil
}

// sealedValues reads the sealed values that query selects as flat, name, nonce and ciphertext.
func sealedValues(ctx context.Context, x executor, query string) ([]SealedValue, error) {
	rows, err := x.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SealedValue
	for rows.Next() {
		var v SealedValue
		if err := rows.Scan(&v.Flat, &v.Name, &v.Nonce, &v.Ciphertext); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
