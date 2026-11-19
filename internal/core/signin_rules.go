package core

import (
	"github.com/gosuda/portal-tunnel/v2/sdk/appauth"

	"github.com/gosuda/flats/internal/store"
)

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
