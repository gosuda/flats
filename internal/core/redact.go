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

func newRedactor(env map[string]string) func(string) string {
	seen := map[string]bool{}
	var pats []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			pats = append(pats, p)
		}
	}
	addEncoded := func(v string) {
		add(v)
		add(url.QueryEscape(v))
		add(url.PathEscape(v))
		add(base64.StdEncoding.EncodeToString([]byte(v)))
		add(base64.RawStdEncoding.EncodeToString([]byte(v)))
		add(base64.URLEncoding.EncodeToString([]byte(v)))
		add(base64.RawURLEncoding.EncodeToString([]byte(v)))
		add(hex.EncodeToString([]byte(v)))
		add(strings.ToUpper(hex.EncodeToString([]byte(v))))
	}
	for _, v := range env {
		addEncoded(v)
		if q, err := json.Marshal(v); err == nil {
			add(string(q[1 : len(q)-1]))
			// Worker settings travel through JSON. Legacy invalid UTF-8 bytes
			// normalize to replacement runes when decoded; output may contain
			// that normalized value or an encoding of it instead of raw bytes.
			var normalized string
			if json.Unmarshal(q, &normalized) == nil && normalized != v {
				addEncoded(normalized)
				if normalizedJSON, err := json.Marshal(normalized); err == nil {
					add(string(normalizedJSON[1 : len(normalizedJSON)-1]))
				}
			}
		}
	}
	if len(pats) == 0 {
		return func(s string) string { return s }
	}
	// Longest first, so a value is never partly replaced by a shorter one.
	slices.SortFunc(pats, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	var args []string
	for _, p := range pats {
		args = append(args, p, redacted)
	}
	r := strings.NewReplacer(args...)
	return r.Replace
}
