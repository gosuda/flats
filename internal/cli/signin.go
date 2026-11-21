package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// `flats signin` of docs/spec/portal-sign-in.md ("Requests and approvals"): the Portal sign-in of a flat. The CLI
// requests a change, sets the client secret and ends the sessions; only the console issues an access link.

const signInAbout = "Manages the Portal sign-in of a flat. With oidc, a visitor signs in at your OpenID Connect provider and\n" +
	"must be on the allowlist (an email, an @domain or a sub:subject entry). With credential, a visitor opens an\n" +
	"access link for one subject of the allowlist. Sign-in protects the Portal URL of the flat only: its private\n" +
	"routes, previews, the console and MCP are not behind sign-in.\n\n" +
	"show prints the setting, the binding of the client secret, the Portal host names and the callback URLs to\n" +
	"register at the provider; --json prints the API response. set and off request a change, which waits for your\n" +
	"approval in the console (exit status 3). secret set reads the OIDC client secret from the terminal without\n" +
	"echo, or from a pipe, and binds it to the issuer and the client ID; secret rm removes it. Neither needs an\n" +
	"approval. revoke ends every sign-in session and every access link of the flat; its host names stay.\n\n" +
	"The console issues the access links, on the Sign-in row of the Settings tab of the flat. This command cannot\n" +
	"issue one."

// stringsFlag collects each value of a repeated flag.
type stringsFlag []string

func (f *stringsFlag) String() string { return strings.Join(*f, ",") }

func (f *stringsFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// signInView mirrors the response of GET /api/flats/{slug}/signin (rule 56).
type signInView struct {
	Provider     string   `json:"provider"`
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"client_id"`
	Allow        []string `json:"allow"`
	ClientSecret *struct {
		Name      string    `json:"name"`
		Issuer    string    `json:"issuer"`
		ClientID  string    `json:"client_id"`
		UpdatedAt time.Time `json:"updated_at"`
	} `json:"client_secret"`
	Hosts        []string `json:"hosts"`
	CallbackURLs []string `json:"callback_urls"`
	Seq          int64    `json:"seq"`
}

func (a *app) signin(args []string) error {
	if len(args) == 0 {
		return usagef("missing subcommand (show, set, off, secret or revoke)")
	}
	sub, args := args[0], args[1:]
	fs := a.flags("signin")
	switch sub {
	case "show":
		pos, err := a.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		return a.signInShow(pos[0])
	case "set":
		issuer := fs.String("issuer", "", "oidc: the https `URL` of the issuer")
		clientID := fs.String("client-id", "", "oidc: the client `ID` that the provider gave")
		var allow stringsFlag
		fs.Var(&allow, "allow", "an allowlist `ENTRY`, once for each (oidc: an email, @domain or sub:subject; credential: a subject)")
		reason := fs.String("reason", "", "why, shown to the operator with the approval")
		pos, err := a.parse(fs, args, 2, 2)
		if err != nil {
			return err
		}
		given := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
		body := map[string]any{"provider": pos[1], "allow": []string(allow)}
		switch pos[1] {
		case "oidc":
			for _, name := range []string{"issuer", "client-id", "allow"} {
				if !given[name] {
					return usagef("oidc needs --%s", name)
				}
			}
			body["issuer"], body["client_id"] = *issuer, *clientID
		case "credential":
			for _, name := range []string{"issuer", "client-id"} {
				if given[name] {
					return usagef("credential does not take --%s", name)
				}
			}
			if !given["allow"] {
				return usagef("credential needs --allow SUBJECT, once for each subject")
			}
		default:
			return usagef("the provider must be oidc or credential; `flats signin off %s` turns sign-in off", pos[0])
		}
		if *reason != "" {
			body["reason"] = *reason
		}
		return a.signInRequest(pos[0], body)
	case "off":
		reason := fs.String("reason", "", "why, shown to the operator with the approval")
		pos, err := a.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		body := map[string]any{"provider": ""}
		if *reason != "" {
			body["reason"] = *reason
		}
		return a.signInRequest(pos[0], body)
	case "secret":
		return a.signInSecret(fs, args)
	case "revoke":
		pos, err := a.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		return a.signInAction(http.MethodPost, pos[0], "/signin/revoke", nil)
	}
	return usagef("unknown signin subcommand %q (use show, set, off, secret or revoke)", sub)
}

func (a *app) signInSecret(fs *flag.FlagSet, args []string) error {
	if len(args) == 0 {
		return usagef("missing secret subcommand (set or rm)")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "set":
		issuer := fs.String("issuer", "", "the https `URL` of the issuer that the client secret belongs to")
		clientID := fs.String("client-id", "", "the client `ID` that the client secret belongs to")
		pos, err := a.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		given := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
		for _, name := range []string{"issuer", "client-id"} {
			if !given[name] {
				return usagef("secret set needs --%s: the client secret is bound to the issuer and the client ID", name)
			}
		}
		value, err := a.readSecret("the sign-in client secret of "+pos[0], "flats signin secret set "+pos[0]+" --issuer "+*issuer+" --client-id "+*clientID)
		if err != nil {
			return err
		}
		return a.signInAction(http.MethodPut, pos[0], "/signin/secret", map[string]string{"value": value, "issuer": *issuer, "client_id": *clientID})
	case "rm", "delete":
		pos, err := a.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		return a.signInAction(http.MethodDelete, pos[0], "/signin/secret", nil)
	}
	return usagef("unknown signin secret subcommand %q (use set or rm)", sub)
}

// signInRequest requests a sign-in setting; a change waits for the operator (exit 3).
func (a *app) signInRequest(slug string, body map[string]any) error {
	var res actionResult
	resp, err := a.client().call(a.ctx, http.MethodPost, "/api/flats/"+pathEscape(slug)+"/signin", nil, body, &res)
	if err != nil {
		return a.signInErr(err)
	}
	return a.action(resp, res, "")
}

// signInAction runs a sign-in route that applies at once and prints its message.
func (a *app) signInAction(method, slug, route string, body any) error {
	var res actionResult
	resp, err := a.client().call(a.ctx, method, "/api/flats/"+pathEscape(slug)+route, nil, body, &res)
	if err != nil {
		return a.signInErr(err)
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	fmt.Fprintln(a.out, res.Message)
	return nil
}

// signInErr explains the 404 of a host that is older than the sign-in routes (rule 71). The mux of such a host
// answers with text, and a 404 of this host, such as an unknown flat, is a JSON error that stays as it is.
func (a *app) signInErr(err error) error {
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound && !json.Valid(ae.Raw) {
		return fmt.Errorf("the Flats host at %s is older than `flats signin`: it has no sign-in routes. Update the host, then run the command again", a.url)
	}
	return err
}

func (a *app) signInShow(slug string) error {
	var v signInView
	resp, err := a.client().call(a.ctx, http.MethodGet, "/api/flats/"+pathEscape(slug)+"/signin", nil, nil, &v)
	if err != nil {
		return a.signInErr(err)
	}
	if a.jsonOut {
		a.emitRaw(resp)
		return nil
	}
	w := a.out
	switch v.Provider {
	case "":
		fmt.Fprintf(w, "Sign-in of %s is off: anyone can open its Portal URL.\n", slug)
	case "oidc":
		fmt.Fprintf(w, "Sign-in of %s: OpenID Connect\n  issuer: %s\n  client ID: %s\n", slug, v.Issuer, v.ClientID)
	case "credential":
		fmt.Fprintf(w, "Sign-in of %s: access links (credential)\n", slug)
	default:
		fmt.Fprintf(w, "Sign-in of %s: %s\n", slug, v.Provider)
	}
	if v.Provider != "" {
		fmt.Fprintf(w, "  allowlist (%d):\n", len(v.Allow))
		for _, e := range v.Allow {
			fmt.Fprintf(w, "    %s\n", e)
		}
	}
	switch cs := v.ClientSecret; {
	case cs != nil:
		fmt.Fprintf(w, "  client secret: %s, set %s, for issuer %s and client ID %s\n", cs.Name, cs.UpdatedAt.Local().Format(timeFmt), cs.Issuer, cs.ClientID)
		if v.Provider == "oidc" && (cs.Issuer != v.Issuer || cs.ClientID != v.ClientID) {
			fmt.Fprintf(w, "  warning: the client secret belongs to another issuer or client ID, so the Portal route stays stopped until you run `flats signin secret set %s --issuer %s --client-id %s`\n",
				slug, v.Issuer, v.ClientID)
		}
	case v.Provider == "oidc":
		fmt.Fprintln(w, "  client secret: none (the gate signs in as a public client with PKCE)")
	}
	if len(v.Hosts) == 0 {
		fmt.Fprintln(w, "  Portal host names: none (the flat has no Portal route now)")
	} else {
		fmt.Fprintf(w, "  Portal host names: %s\n", strings.Join(v.Hosts, ", "))
	}
	if v.Provider == "oidc" {
		if len(v.CallbackURLs) == 0 {
			fmt.Fprintln(w, "  callback URLs: none until the flat has a Portal route")
		} else {
			fmt.Fprintln(w, "  callback URLs to register at the provider:")
			for _, u := range v.CallbackURLs {
				fmt.Fprintf(w, "    %s\n", u)
			}
		}
	}
	fmt.Fprintln(w, "Sign-in protects the Portal URL of the flat only; its private routes, previews, the console and MCP are not behind sign-in. The console issues the access links.")
	return nil
}
