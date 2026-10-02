package core

import (
	"cmp"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
)

// redacted replaces secret values in text a flat produced.
const redacted = "[redacted]"

// newRedactor returns a function that removes every value of env, and common
// encodings of it, from output a flat's code produced (runtime logs, worker
// stderr, health check bodies). Secrets reach a flat only through env, so
// these are exactly the values its output can leak.
// minRedactLen is the shortest secret value that is redacted.
const minRedactLen = 6

func newRedactor(env map[string]string) func(string) string {
	seen := map[string]bool{}
	var pats []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			pats = append(pats, p)
		}
	}
	for _, v := range env {
		// Very short values ("1", "on") are not secrets in any useful sense
		// and would shred unrelated log text.
		if len(v) < minRedactLen {
			continue
		}
		add(v)
		add(url.QueryEscape(v))
		add(url.PathEscape(v))
		add(base64.StdEncoding.EncodeToString([]byte(v)))
		add(base64.RawStdEncoding.EncodeToString([]byte(v)))
		add(base64.URLEncoding.EncodeToString([]byte(v)))
		add(base64.RawURLEncoding.EncodeToString([]byte(v)))
		add(hex.EncodeToString([]byte(v)))
		add(strings.ToUpper(hex.EncodeToString([]byte(v))))
		if q, err := json.Marshal(v); err == nil {
			add(string(q[1 : len(q)-1]))
		}
	}
	if len(pats) == 0 {
		return func(s string) string { return s }
	}
	// Longest first, so a value is never partly replaced by a shorter one.
	slices.SortFunc(pats, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	args := make([]string, 0, 2*len(pats))
	for _, p := range pats {
		args = append(args, p, redacted)
	}
	r := strings.NewReplacer(args...)
	return r.Replace
}
