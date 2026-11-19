package core

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/portal-tunnel/v2/sdk/appauth"

	"github.com/gosuda/flats/internal/store"
)

// HideSignInEntries returns text with each entry of allow, in its raw form and in its Go-quoted form, replaced by
// <entry>. An error of the SDK quotes an entry that it refuses (`principal %q`), and only the console and the caller
// who sent the entries may see one (docs/spec/portal-sign-in.md, rule 58): every text of an SDK error that can reach
// an endpoint detail, a log line, an approval result or a flat event goes through it.
func HideSignInEntries(text string, allow []string) string {
	olds := make([]string, 0, 2*len(allow))
	for _, e := range allow {
		if e != "" {
			olds = append(olds, strconv.Quote(e), e)
		}
	}
	if len(olds) == 0 {
		return text
	}
	// The replacer takes the first old string that matches at a position, so a longer form wins over a part of it.
	slices.SortFunc(olds, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	pairs := make([]string, 0, 2*len(olds))
	for _, old := range slices.Compact(olds) {
		pairs = append(pairs, old, "<entry>")
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// sdkConfig is the gate configuration of in without its client secret, identity and hosts, for the checks of the SDK.
// The issuer and the client ID go into it for every provider, so that the SDK refuses them next to credential.
func sdkConfig(in store.SignIn) appauth.Config {
	cfg := appauth.Config{Provider: in.Provider, OIDCIssuerURL: in.Issuer, OIDCClientID: in.ClientID}
	if in.Provider == store.SignInOIDC {
		cfg.EmailList = in.Allow
	} else {
		cfg.Allow = in.Allow
	}
	return cfg
}

// sdkNormalizeAllow normalizes an allowlist as appauth.Config.Normalize does for provider (docs/spec/portal-sign-in.md,
// rules 2 and 3).
func sdkNormalizeAllow(provider string, values []string) ([]string, error) {
	cfg, err := sdkConfig(store.SignIn{Provider: provider, Allow: values}).Normalize()
	if err != nil {
		return nil, err
	}
	return cfg.Allowed(), nil
}

// sdkValidate is appauth.Validate for a gate of in: what the SDK refuses at the build of a gate, the host refuses
// before it stores a setting (rule 4).
func sdkValidate(in store.SignIn) error {
	return appauth.Validate(sdkConfig(in))
}
