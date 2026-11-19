# Portal sign-in

Status: implemented. The operator guide is [Portal sign-in](../networking.md#portal-sign-in), the architecture is under
[Exposure](../design.md#exposure) in the design, and the routes, commands, MCP texts and console screens are in
[Surfaces](../surfaces.md).

## Summary

A Public flat that Portal serves is open to anyone who has its URL. This spec adds a sign-in setting to each flat. The
operator chooses a provider and an allowlist, and the sign-in gate of the Portal SDK (package `sdk/appauth` of
`github.com/gosuda/portal-tunnel/v2` v2.7.4) lets only the visitors on the allowlist open the Portal URL of the flat.
Flats still decides which flat is Public. The gate decides who can open it on Portal.

The first version has two providers. With `oidc`, a visitor signs in at an OpenID Connect provider of the operator. With
`credential`, the operator gives an access link to each named subject. Each change of the setting waits for the approval
of the operator in the console. Sign-in protects the Portal URL of the flat only. It does not protect the private routes
of the flat, its previews, the console, MCP or the API.

The request is issue #221. Host-wide sign-in (#222) and the authentication of operators and agents (#139) are other
issues.

## Before this change

- Portal is a public provider for "anyone on the internet, through a Portal relay" (`docs/networking.md`). One Public
  flat can have several public providers at once, and each gets the same handler, `siteHandler(slug, true)`
  (`ensureLifecycle` in `internal/core/lifecycle.go`, `servePublic` in `internal/expose/provider/manager.go`).
  `publicAvailable` accepts any of the six public providers.
- The Portal provider runs one exposure for each slug. Its identity is the file `portal/<slug>.json`, which
  `identity.LoadOrCreate` makes the first time (`loadIdentity` in `internal/expose/portal/portal.go`). The identity
  holds a token secret, from which the SDK derives its signing keys. `Net.Serve` keeps the identity in a local variable,
  and the entry does not keep it. A renamed flat gets a new identity and a new canonical host name, because the file
  name is the slug ([design](../design.md#exposure), "Portal hostnames").
- When `Net.Serve` gets a slug that it serves, it replaces the handler in the `handlerBox` of the entry and keeps the
  registration. A new entry serves `publicHandler(e.handler)` with `sdk.RunHTTP`. `publicHandler` removes client copies
  of `Tailscale-User-*`.
- `PublicNet.Serve(ctx, slug, h, hidden)` and `ExposureRequest` carry no sign-in setting (`internal/core/expose.go`).
- The public site handler reads the flat for each request and answers 503 when the flat is not Public (`serveSite` in
  `internal/core/service.go`). `withRouteHeaders` (`internal/core/contenttype.go`) removes client copies of
  `X-Flats-Access`, `X-Flats-Health`, `X-Flats-Host-Op`, `X-Flats-Public-Access` and `X-Flats-Access-Epoch`, and sets
  `X-Flats-Access` to `public` or `private` by route. The docs worker makes a request read-only unless it has
  `x-flats-access: private`.
- A preview is private only. `servePublic` refuses a preview, `servePrivate` refuses a public provider for it, and
  `previewHandler` sets `X-Flats-Access: private` (`internal/core/route_epoch.go`).
- A visibility change is an approval (`requestVisibility`, `visibilityParams`, `requestFrozen` and
  `applyVisibilityApproval` in `internal/core/lifecycle.go`). It freezes the starting visibility and level, the access
  sequence, the provider key, and the live version or the flat ID. The console decides it, or an Autopilot agent when
  the operator allows that operation. `AutopilotNeeds` (`internal/core/autopilot.go`) refuses an action that it does not
  know, so an agent can neither approve nor reject such an approval ([Autopilot](autopilot.md), rule 6).
- Only the console grants or revokes a provider for a flat (`SetProviderPermission`).
- A flat secret is a sealed row of the table `secrets`, with the key `(flat, name)` and, since schema 28, the binding
  columns of [injected fetch secrets](injected-fetch-secrets.md). Schema 28 is the latest (`internal/store/migrate.go`).
  An activation reads every secret row of the flat into `env` or into the injected list
  (`Store.RuntimeSettingsSnapshot`). `flats secret rotate-key`, `restore-key` and `reset`, `flats backup` and the
  deletion of a flat treat every row.
- The audit log has the kinds of [audit events](audit-events.md) and of [event webhook](event-webhook.md), rule 21. The
  webhook sends `approval.created`, `approval.decided`, `visibility.changed` and `network.changed`.
- A change to Private ends the open connections of the public routes of a flat (`revokeRoute` and the route epoch in
  `internal/core/route_epoch.go`, called from `internal/core/lifecycle.go`). A public handler binds the epoch when the
  host makes it, and a cancelled epoch answers 503. A handler swap in the Portal entry does not touch a request that
  runs.
- The loopback API takes a caller as the CLI when it sends the header `X-Flats-Client: cli` from the loopback address
  (`internal/api/api.go`, `isLoopback`). Any local process can send that header, also one of another account.
- `StopSlug` of the Portal provider stops the route and keeps `portal/<slug>.json`
  (`internal/expose/provider/manager.go`), and an existing identity file wins at the next load. So a later flat with the
  same slug gets the same identity and token secret. The Tailscale provider removes the state of a slug for that reason.
- A Portal relay routes a visitor connection by the SNI of its TLS client hello and forwards the bytes
  (`portal/server.go` and `portal/proxy.go` of the SDK module). For an exposure without a cache, the tenant TLS ends in
  the process of the exposure, so a relay that follows the protocol forwards ciphertext and does not change the `Host`
  header of a request. The relay holds the certificate key and signs the TLS handshakes (keyless TLS), so the SDK keeps
  the relay in its trust boundary: a relay that misuses that key can end the TLS itself. The MITM self-probes of the
  SDK only log a warning, because Flats sets no ban.
- Flats pins the SDK v2.7.4 (`go.mod`). No code imports `sdk/appauth`. In that package:
  - `New(next, id, cfg)` wraps a handler. It reads no network and derives its keys from the identity, also from its
    token secret. A gate that is built from the same identity and the same provider settings accepts the sessions of an
    earlier gate. The gate checks the allowlist on every request. It answers the paths under `/_portal/auth/` itself and
    passes the other requests of a signed-in visitor to `next`.
  - `Check(ctx, cfg)` fetches the OIDC discovery document and the signing keys.
  - A session lasts at most 24 hours, in the cookie `__Host-portal_access`.
  - `PublicHosts` lists the hosts of the application. A `Since` on a host makes the gate refuse a session or a sign-in
    from before that time. The gate refuses a request for a host name that is not in the list.
  - `PrincipalFromContext` gives the provider, the subject and the verified email of the visitor.
  - `IssueCredential(id, host, subject, expiresAt)` makes a credential for one host and one identity, and
    `CredentialRedeemURL` puts it into the fragment of a link, so it reaches no relay and no server log.
  - An empty allowlist opens `credential` to any credential of the identity. `oidc` refuses an empty allowlist, accepts
    an `http` issuer on a loopback host, works without a client secret, asks for the scopes `openid`, `email` and
    `profile`, and uses the callback URL `https://<request host>/_portal/auth/callback`.
  - `IdentityHeaders` sets only `X-Portal-User` and `X-Portal-Auth`, and no email.

## Scope

In scope: the sign-in setting of a flat, its storage, its request and its approval, the refusal of other public
providers, the OIDC client secret, the gate on the Portal route, the check of each request, the headers of the visitor,
the access links of `credential`, the views of the setting in the API, the CLI, MCP and the console, the audit kinds,
the webhook event, the schema, and the docs.

Out of scope:

- The providers `siwe`, `saml` and `email` of the SDK.
- Sign-in on any route other than the Portal route of the current slug: the other public providers, the private routes,
  previews, the rename redirect of an old slug (rule 41), the console, MCP and the API.
- The rights of a private visitor for a signed-in visitor. A signed-in visitor is a Public visitor (decision 3).
- A console form that requests a change or sets the client secret.
- Revocation of one access link. `flats signin revoke` ends every session and every link of the flat (rule 54).
- Host-wide sign-in (#222).
- Protection of the Portal identity file with the host key (see "Known limits").

## Decisions

The owner of the repository decided five points:

1. Sign-in applies to Portal only. While a flat has sign-in on, the flat is Public through Portal alone. The host
   refuses to make the flat Public with another public provider (`tailscale-funnel`, `zrok`, `cloudflare-quick`,
   `netbird-expose` or `proxy-public`), to grant another public provider, and to turn sign-in on while another public
   provider is granted. Each refusal names the next action. The host never falls back to open.
2. The first version has two providers: `oidc` (client ID, issuer, sealed client secret, an allowlist of verified
   emails, `@domain` entries or `sub:` subjects) and `credential` (access links for the named subjects of the
   allowlist).
3. A visitor who passed the sign-in stays a Public visitor. The flat still sees `X-Flats-Access: public`. Documents,
   boards and designs stay read-only, or open to guest editing as the Public level says. Revisions stay private.
4. Flat code gets the identity of the visitor in trusted headers. The host passes the provider, the subject and the
   email of the signed-in visitor. It removes any client copy of those headers on every route, public and private.
   Without a sign-in the headers are absent.
5. Only the console issues an access link (rule 49). The CLI, the API and MCP cannot. A link names one subject of the
   approved allowlist and expires after at most 90 days. The loopback API takes any local process that sends the CLI
   header as the CLI, also an agent of another account, so an agent on the host could otherwise widen access alone. The
   owner first chose the CLI without an approval, on the claim that a CLI user can read `portal/<slug>.json`. The
   design review showed that claim false for an agent of another account, and the owner then chose the console.

The author of this spec decided these points, and each can change before the implementation:

- The setting is five columns of `flats`, and the client secret is a row of `secrets` with the new columns `use`,
  `signin_issuer` and `signin_client_id`, in schema 29 (rule 72). The check of each request reads the flat row that it
  reads today, and a rename, a deletion and a backup already move, remove and copy that row.
- Every change of the setting needs an approval, also off and each change of the allowlist (rule 16). Every change can
  widen access, and off opens the Portal URL to anyone.
- An Autopilot agent can neither approve nor reject a sign-in approval (rule 17). The allowlist says who can read the
  flat, and an agent that decides the request of another agent still widens access without a person.
- The CLI and the loopback API request a change (rule 13). MCP only reads the setting, and no MCP output holds an entry
  of the allowlist, also not an approval or a log line (rules 57 and 58). The console shows the setting, decides the approval,
  and issues the access links (rules 49, 63 and 64). This follows [injected fetch secrets](injected-fetch-secrets.md),
  and it keeps the MCP agents read-only.
- The OIDC client secret is a flat secret with the use `signin`, which the CLI sets and removes without an approval
  (rules 24 to 32). It is bound to the issuer and the client ID that the operator names when it sets the value, and a
  gate sends it only to that issuer (rule 27). So an approved change of the issuer never sends the old secret to the new
  issuer. The secret proves Flats to the provider and decides the access of nobody: a wrong value makes the sign-in
  fail, and it lets no other visitor in.
- `credential` needs at least one subject on the allowlist, which the SDK does not need (rule 3). The approved allowlist
  limits who a link can name (decision 5, rules 49 to 55).
- `flats signin revoke <slug>` gives the identity a new token secret, which ends every session and every access link of
  the flat and keeps its host names (rule 54). The deletion of a flat and a rename do the same for the old identity
  (rules 42 and 41). The SDK derives no other key from the token secret, and a credential has no date, so a new token
  secret is the only way to end a link before it expires.
- An access link names the canonical host name only (rule 51). An alias can change hands and come back, and a link on
  the alias would work again with it.
- The Portal provider builds the gate where the identity is, around the handler box of the entry (rule 33). So a
  redeploy or a rollback replaces the handler inside the gate. A gate that cannot be built gives the flat no Portal
  route (rule 35). A failed check of the provider is a warning, and the gate stays (rule 37).
- The public site handler checks each request against the stored setting (rule 44), and an applied sign-in change ends
  the open public connections of the flat (rule 20). A code path that opens a public route without the gate, a gate
  that an older setting built, and a connection that an older setting let in, then give no access.
- `PublicHosts` comes from every relay that the exposure reports, whatever its state, with the `AliasSince` of the SDK
  for each alias (rule 34), as the `portal-tunnel` command does. A session of an earlier holder of an alias does not
  count, and a relay that reports `connecting` while it still forwards does not give 421.
- The rename redirect of an old slug has no gate (rule 41). It sends no content, its target has the gate, and the old
  identity gets a new token secret at the rename.
- The issuer is `https` only (rule 2). The host fetches the issuer itself, and an `http` issuer on the loopback address
  can reach a local service. The host stores the issuer without a trailing `/`, because the SDK compares issuers without it
  on both sides, so one stored form serves the gate, the binding of rule 27 and the sessions of rule 39.
- An allowlist entry has visible ASCII characters and the space only, a list has at most 100 entries, and the host
  stores it in byte order (rule 4). An entry goes into a header, the console and the approval page, and the order makes
  two equal lists equal.
- The headers are `X-Flats-Visitor-Provider`, `X-Flats-Visitor-Subject` and `X-Flats-Visitor-Email`, and the host also
  removes client copies of the Portal identity headers on every route (rules 45 and 46). Flat code that reads the names
  of Portal must not trust a client copy on a route without the gate.
- The audit log gets `signin.set`, `signin.link` and `signin.revoke`, and a client secret change writes `secret.set` and
  `secret.delete` with `"use": "signin"`. The webhook gets one type, `signin.changed`, with three operations, and its
  body names no entry and no subject (rules 65 to 69). This follows rule 5 of [event webhook](event-webhook.md): a body
  names no login or address of a person.
- The Portal provider writes the token secret into an identity file that has none (rule 36). For such a file the SDK
  makes a new random token secret at each load, and every session and link would end at each restart.

## The setting

1. The sign-in setting of a flat has a provider and the fields of that provider. The provider is `oidc`, `credential` or
   empty. Sign-in is on when the provider is not empty. A new flat has sign-in off.
2. An `oidc` setting has an issuer, a client ID and an allowlist. The issuer is an `https` URL with a host name and
   without user information, query or fragment, of at most 2048 bytes. The host removes spaces at the ends and a
   trailing `/`, and stores that form. The SDK compares an issuer with the issuer of the discovery document without a
   trailing `/` on both sides, so two issuers that differ only in it are the same issuer for the gate, for rule 27 and
   for rule 39. The host refuses an `http` issuer, also on a loopback host, which the SDK accepts. The client ID
   has 1 to 256 bytes of visible ASCII. Each entry of the allowlist is a verified email address, an `@domain` entry or
   a `sub:<subject>` entry. The host normalizes each entry as `appauth.Config.Normalize` does: an email and a domain in
   lower case, a subject byte for byte.
3. A `credential` setting has an allowlist of subjects. The host normalizes each subject as the SDK does (spaces at the
   ends are removed). The SDK opens an empty credential allowlist to any credential of the identity. The host refuses an
   empty list.
4. An allowlist has 1 to 100 entries. Each entry has only visible ASCII characters and the space. A credential subject
   has 1 to 256 bytes, and an `oidc` entry at most 256 bytes without the prefix `sub:`. The host removes duplicates after
   the normalization and stores the list in byte order. Two settings are the same when their providers, issuers, client
   IDs and stored lists are equal. Each setting that the host stores also passes `appauth.Validate`, so no value that the
   SDK refuses reaches the gate.
5. An `oidc` setting can use a client secret (rules 24 to 32). Without one, the SDK uses the setting as a public client
   with PKCE.
6. `flats.db` stores the setting with the flat. A redeploy, a rollback, a data restore, a rename and a host restart keep
   it. The deletion of a flat removes it. Only an approval changes the setting (rule 16). Only rules 25 and 28 change
   the client secret.

## Where sign-in applies

7. Sign-in applies to the Portal route of the current slug of the flat. On a Private flat, or on a flat without a Portal
   grant, the setting has no effect until the flat is Public on Portal. So an operator can turn sign-in on before the
   flat becomes Public, and the Portal URL is not open between the two changes.
8. While sign-in is on, the flat is Public through Portal alone (decision 1). The host refuses these operations with 409
   and changes nothing:
   - a grant of `tailscale-funnel`, `zrok`, `cloudflare-quick`, `netbird-expose` or `proxy-public` to the flat. The
     message says that visitors on that provider do not sign in, and that sign-in goes off only with `flats signin off
     <slug>` and an approval in the console;
   - a visibility request, or the application of a visibility approval, that makes the flat Public while one of those
     providers is granted. The message names the provider and says to revoke it under Networks on the Settings tab of
     the flat, which works while the flat is Private;
   - a sign-in request, or the application of a sign-in approval, that turns sign-in on while one of those providers is
     granted. The message names the provider. For a Private flat it says to revoke the provider under Networks. For a
     Public flat it says to make the flat Private first, then to revoke the provider, because the host refuses a
     revocation while the route of the provider is registered.
9. The store checks the grants and the setting, and writes the grant or the setting, in one write transaction. So a
   grant and a sign-in change that run at the same time give the result of one of the two orders.
10. While sign-in is on, the host asks no public provider other than Portal to serve the flat, also when a stored grant
    permits one. Rule 44 refuses a request that arrives on such a route.
11. A signed-in visitor is a Public visitor (decision 3). The flat gets `X-Flats-Access: public`. Documents, designs,
    boards and notebooks stay read-only, or open to guest editing as the Public level says. Revisions stay private.
12. Sign-in does not protect the private routes of the flat (Local, Tailscale, NetBird and `proxy`), its previews, the
    console, MCP or the API. A visitor on a private route keeps the rights of a private visitor, as today.

## Requests and approvals

```text
flats signin show <slug> [--json]
flats signin set <slug> oidc --issuer URL --client-id ID --allow ENTRY [--allow ENTRY ...] [--reason TEXT]
flats signin set <slug> credential --allow SUBJECT [--allow SUBJECT ...] [--reason TEXT]
flats signin off <slug> [--reason TEXT]
flats signin secret set <slug> --issuer URL --client-id ID
flats signin secret rm <slug>
flats signin revoke <slug>
```

A missing flag, a flag that the provider does not take, and a second positional argument are usage errors (exit
status 2). `flats help signin` names each form, says that sign-in protects the Portal URL only, and says that the
console issues the access links.

13. `flats signin set` and `flats signin off` call `POST /api/flats/{slug}/signin`. The route takes a caller on the
    loopback address, with the CLI header or without it, and the console. A remote caller gets 403. MCP has no tool that
    requests a change, and the console has no form for it.
14. The body is `{"provider": "oidc", "issuer": ..., "client_id": ..., "allow": [...], "reason": ...}`, `{"provider":
    "credential", "allow": [...], "reason": ...}`, or `{"provider": "", "reason": ...}` for off. `reason` is optional
    text of at most 1 KiB. A member that the provider does not take, a member that is missing, and a value that breaks
    rules 2 to 4 (also through `appauth.Validate`) give 400. The message names the member and the rule that it breaks.
    The message for an issuer does not repeat the text of the caller, because it can hold user information or a query.
15. A request for the setting that is stored returns `done` with the message `sign-in unchanged` and makes no approval.
16. Each other request makes a pending approval with the action `set_signin` through `requestFrozen` and changes
    nothing. A second equal request while the first one waits returns the open approval. The approval freezes:
    - `flat_id`, the ID of the flat;
    - `seq`, the sign-in sequence of the flat (rule 19);
    - `from`, the stored setting;
    - `to`, the requested setting after rules 2 to 4;
    - `public_grants`, the sorted list of the public providers that the flat has a grant for. Rule 8 needs only these,
      so an unrelated change of the host (an account or a key of a provider) does not make the approval stale.
    The frozen parameters hold no client secret.
17. Only the operator decides a `set_signin` approval, in the console. `AutopilotNeeds` refuses the action as unknown,
    so an Autopilot agent can neither approve nor reject it, and the Autopilot tab has no switch for it.
18. The host applies an approved `set_signin` only when the flat ID, the sign-in sequence and the public grants are
    still the frozen ones and rule 8 allows the change. Otherwise the approval fails as stale. The message names what
    changed and says to request the change again. A grant or a revocation of a public provider for the flat, another
    sign-in change and a new flat under the same slug make an approval stale. A redeploy, a rollback, a visibility
    change, a client secret change, a revocation of the sessions (rule 54) and a change of the host settings of a
    provider do not.
19. The sign-in sequence of a flat starts at 0 and grows by 1 with each applied sign-in change. A sign-in change of a
    flat and back to the first setting still makes an earlier request stale.
20. The host applies an approval in this order:
    1. It builds the configuration of the gate from the frozen setting and the stored client secret, and checks it with
       `appauth.Validate`. When that fails, the approval fails and nothing changes.
    2. It writes the setting, the new sequence, the audit row of rule 65 and the receipt of the approval in one
       transaction, with the check of rule 9, as `CommitVisibility` writes its receipt.
    3. It ends the open connections of the public routes of the flat, as a change to Private ends them
       (`revokeRoute`). A visitor who holds a WebSocket or a long request reconnects through the gate.
    4. Step 3 cancels the epoch of every public route of the flat, so the host serves every public route that the flat
       uses again, with a new public handler that binds the new route epoch. On the Portal route, the Portal provider
       builds the new gate around that handler and puts it in place of the old one (rules 33 and 35). So an allowed
       visitor gets the flat again, and the cancelled epoch of step 3 answers only the connections that were open.
    5. It writes the flat event and sends the webhook event of rule 68.
21. Between steps 2 and 4, rule 44 refuses each request that the old gate lets in, when the new setting is on. After a
    change to off, rule 44 passes every request, also one that the old gate let in, and that gives no visitor more than
    the change gives. When step 4 fails, the Portal route
    of the flat stops (rule 35). The approval is then `approved`, and its result says that the setting is stored and
    the Portal route is stopped, with the reason. When the host stops after step 2, `resumeApplying` finds the receipt
    at the next start, does steps 3 to 5 from the stored setting, and ends the approval as `approved` with one stored
    change and one audit row. It sends no webhook event, because `resumeApplying` runs inside `core.New`, before the
    host holds the sender ([event webhook](event-webhook.md), rules 1 and 17). When the stored setting cannot be read
    at that start, the approval stays `applying`, and the next start resumes it. When the frozen parameters cannot be
    read, the result and the flat event say that the setting before the change is unknown, never that it was off.
    When the host stops before step 2 ends, nothing is stored, and the approval applies again from step 1. Steps 3 to
    5 do not depend on the request of the decision: when the client drops it after step 2, the host still does them,
    so the Portal route stays in service with the gate of the stored setting. The last write of the approval uses that
    request, so the approval then stays `applying` until the next start, which ends it as above.
22. The result of an applied approval says `done` and names the provider, the issuer and the number of entries. It never
    holds an entry of the allowlist, so that the flat event has none either.
23. Each approval that freezes the visibility of a flat (`set_visibility`, `activate`, `rollback` and `create_version`)
    also freezes the sign-in sequence of the flat. A sign-in change makes it stale, so such an approval never applies
    under a notice of rule 61, or under an Autopilot decision, that knew another sign-in setting.

## The client secret

24. The OIDC client secret is a flat secret with the use `signin` and the name `oidc-client-secret`. A flat has at most
    one. The name is not a valid name of an environment variable, so `flats secret set` cannot make a secret of that
    name today.
25. `flats signin secret set <slug> --issuer URL --client-id ID` reads the value as `flats secret set` does (from a
    terminal without echo, or from a pipe with one trailing newline removed) and calls `PUT /api/flats/{slug}/signin/
    secret` with `{"value": ..., "issuer": ..., "client_id": ...}`. `flats signin secret rm <slug>` calls `DELETE` on
    the same route. Only a loopback caller with the CLI header can call the route, as `operatorVia` allows for `cli`.
    Every other caller of `/api` gets 403: a remote caller, with the CLI header or without it, and a loopback caller
    without it. The console has no route for it, so a console request gets 404.
26. A value has 1 to 4096 bytes of visible ASCII, without a space or a control character. The issuer and the client ID
    follow rule 2. Another value gives 400, and the stored value stays. The SDK removes spaces at the ends of a client
    secret, and the sent bytes must be the stored bytes.
27. The stored value is bound to the issuer and the client ID of its set. An `oidc` gate uses the value only when its
    binding equals the issuer and the client ID of the setting. When the setting is `oidc` and a stored value has
    another binding, the Portal provider builds no gate (rule 35), and the detail says to set the client secret again
    for the issuer of the setting. So an approved change of the issuer never sends the value to the new issuer. The
    approval page of such a change names the issuer and the client ID that the stored client secret is bound to, which
    can be those of the stored setting or others (rule 29), and says that the Portal route stops until the operator sets
    the secret for the issuer of the requested setting, with the command that does it. The Sign-in row of rule 63 says
    the same of a stored `oidc` setting whose client secret has another binding, and that its Portal route is stopped.
28. A set or a removal needs no approval. It applies at once. When sign-in is `oidc` and the flat has a Portal route,
    the Portal provider builds the gate again with the new value (rule 33). When that fails, rule 35 stops the route.
    When the host cannot read the flat or its routes to build the gate again, it stops the Portal route as rule 35
    does, so no gate keeps the earlier value, and the message says that the route is stopped and why. The flat event
    and the build after the store commit do not depend on the request: when the client drops it, for example when the
    operator interrupts `flats signin secret set`, the host still builds the gate, and the Portal route stays in
    service.
29. A set is allowed while sign-in is off or `credential`, and for another issuer than the stored one. The value then
    waits for an `oidc` setting with its binding. Only an `oidc` gate reads it. A set while sign-in is `oidc` with
    another issuer or client ID stops the Portal route at once (rule 27), until the setting or the binding agrees.
30. No text and no interface of the host holds the value: `env`, the injected list, a `fetch`, a log line, a flat event,
    an audit row, a webhook body, the export, an approval, the outputs of the API, MCP, the CLI and the console, and the
    status. An activation reads only the secret rows with an empty use, so the value never reaches a worker. `flats
    secret ls`, `GET /flats/{slug}/secrets`, MCP `list_secrets` and the console list of secrets do not list it. The
    sign-in view says whether a value is stored, for which issuer and client ID, and when it was set (rule 56).
31. With 409, `flats secret rm <slug> oidc-client-secret`, its route and the delete of a secret in the console refuse to
    remove the sign-in secret, and the message names `flats signin secret rm <slug>`. A secret row with the name
    `oidc-client-secret` and an empty use can exist from before the name check of `flats secret set`. It stays a
    readable secret, the migration keeps it, and `flats signin secret set` refuses with 409 while it exists. The message
    names `flats secret rm <slug> oidc-client-secret`.
32. The commands that treat every secret treat this one. `flats secret rotate-key` seals it again and keeps its use and
    its binding. `flats secret restore-key` opens it and counts it. `flats secret reset` removes it, and its list names
    it under the flat. `flats backup` copies it. A rename keeps it. The deletion of the flat removes it with a
    `secret.delete` event.

## The gate

33. The Portal provider builds the gate with `appauth.New(next, id, cfg)`:
    - `id` is the Portal identity of the slug. The entry keeps it from now on;
    - `next` is the handler that the entry serves today, `publicHandler` of its handler box, in a wrapper that marks
      each request with the sign-in sequence that the gate was built from (rule 44);
    - `cfg` has the provider, the allowlist in `EmailList` for `oidc` or in `Allow` for `credential`, the issuer, the
      client ID and the opened client secret of rule 27 for `oidc`, `IdentityHeaders` false, no `IdentityProof`, and the
      `PublicHosts` of rule 34.
34. `PublicHosts` gives, on each call, every relay that the exposure reports (`Exposure.Relays`), whatever its state:
    its canonical URL with a zero `Since`, and its alias URL, when it has one, with the `AliasSince` of the SDK (the
    time when this exposure came to hold the alias). The `portal-tunnel` command of the SDK does the same, and it does
    not take the relay state as a signal. A relay that reports `connecting` while it still forwards keeps its host
    names in the list. A lost alias leaves the list, and an alias that comes back gets a new `Since`. The gate answers a
    request for another host name with 421.
35. When the setting cannot be read, the client secret cannot be opened or has another binding (rule 27), rule 36
    cannot keep the token secret of the identity, or `appauth.New` fails, the Portal provider builds no gate and does
    not serve the flat on Portal. For a new entry it opens no route. For an entry that it serves, it stops the route and
    unregisters it from its relays. It never serves the flat without the gate while sign-in is on. The Portal endpoint
    of the flat has the state `error` and the detail `sign-in gate: <reason>`, with the next action. The host log has
    one line with the slug and the reason. The other routes of the flat stay. A visibility approval that needs the
    Portal route fails with that reason, as it fails today when no public route opens. The next build tries again: a
    deploy, a grant, a restart, a sign-in change or a client secret change.
36. Before the first build for an identity, the Portal provider checks that the identity file holds a token secret. When
    the file has none, the provider writes the file again with the identity that it loaded, atomically and with mode
    0600 (on Windows the access list of the SDK). The address, the keys and the canonical host name stay the same. When
    the write fails, rule 35 applies. Once the rename of the write passed, the file holds the new content: a failed
    sync of the folder after it is a warning line in the host log with the path, and the write passes, so the gate in
    service and the file hold the same token secret.
37. After each build of an `oidc` gate, the Portal provider runs `appauth.Check` in the background, with a limit of 15
    seconds, outside the lock of the slug and outside the path of a request. A failure writes one warning line in the
    host log and the detail `sign-in provider check failed: <reason>` on the Portal endpoint, until a later build passes
    the check. The gate stays as it was built and never lets a visitor in without a sign-in. A `credential` gate has no
    check.
38. The Portal provider builds the gate when it opens a Portal route for a flat with sign-in on (an approved deploy or
    visibility change, a grant, a host start, a rename), when a sign-in change applies (rule 20), when the client
    secret changes (rule 28), and after a new token secret of rule 54. A redeploy, a rollback, a data restore
    and a replacement of the worker put a new handler into the handler box and keep the gate. When sign-in goes off,
    the provider removes the gate and serves the handler box as today. Each open of the Portal route of a flat carries
    the stored setting, which the host reads under the lock of the flat, or the mark `redirect` of rule 41, and the host
    makes no other open. An open that carries neither never removes a gate: only an applied change to off and an open
    with the mark `redirect` remove it. Only a call of the Portal provider from outside the service, through the
    `Serve` of `PublicNet`, could make such an open. For a slug that the provider does not serve yet, it has no gate to
    keep and serves the handler box without one, and rule 44 refuses each of its requests while sign-in is on.
    The provider never puts a gate built from a lower sequence in place of a gate built from a higher one, so an open
    that read the setting before a change cannot leave a gate that rule 44 refuses until the next change.
39. A gate keeps the sessions of the gate before it when both have the same identity, the same token secret and the same
    provider, issuer and client ID, and the gate checks the allowlist on every request. So a redeploy, a rollback, a
    host restart, a change of the allowlist and a client secret change keep the session of a visitor who stays on the
    allowlist. A visitor who leaves the allowlist loses access at the next request after rule 20 applies, also on a
    connection that was open (rule 20, step 3). A change of the provider, the issuer or the client ID ends every
    session. A new token secret ends every session and every access link (rules 41, 42 and 54). A session on an alias
    host name ends when the alias leaves and comes back (rule 34). An `oidc` session lasts as the SDK gives it: at
    least 15 minutes and at most 24 hours, after the life of the ID token.
40. The gate answers its own endpoints, the exact paths that `sdk/appauth` serves under `/_portal/auth/` (login,
    challenge, verify, redeem, the OIDC callback, logout, the SAML endpoints and the email endpoints, as
    `types/paths.go` names them). While sign-in is on, a request for one of those paths on the Portal URL never reaches the flat. Another path
    under `/_portal/auth/` is a path of the flat behind the gate, as any other path.
41. A rename of a flat gives the old identity a new token secret, also while sign-in is off, because links of an
    earlier `credential` setting can exist. The rename writes it before it commits: when the write fails, the rename
    fails, changes nothing, and its message names the identity file. When the rename fails after the write passed, the
    flat stays as it was, but every session and access link of the old identity ended: the host writes the row of rule
    67 with `{"cause": "rename"}` and a flat event that says so, and the message of the failed rename says so too. So
    no session and no access link of the old host names works again: the old slug serves only the redirect, and a
    later gate of the old identity, such as the gate of a flat that takes the old slug, refuses them. The open of the
    redirect route of the
    old slug carries the mark `redirect`, removes the gate of that entry, and serves only the redirect, before and
    after a host restart. The redirect sends no content of the flat, and its target has the gate of the new identity.
    A later sign-in change of the flat does not reach the old slug.
42. The deletion of a flat gives its Portal identity a new token secret, when the identity file exists. The deletion
    writes it first: when the write fails, the deletion fails, changes nothing, and its message names the identity
    file. When the deletion fails after the write passed, the flat stays, but every session and access link of it
    ended: the host writes the row of rule 67 with `{"cause": "delete"}` and a flat event that says so, and the message
    of the failed deletion says so too. A later flat with the same slug keeps the host name of the identity, and no
    session or link of the deleted flat works there.
43. A flat with sign-in on is served without a relay cache. Flats sets no cache on a Portal exposure, so the tenant TLS
    ends in the Flats process and the relay forwards only ciphertext (`portal/server.go` routes by SNI,
    `portal/proxy.go` bridges the bytes). A future option that sets a cache for a flat refuses sign-in, as the
    `portal-tunnel` command refuses that pair.

## Each request

44. The public site handler reads the setting and the sequence with the flat, for each request, as it reads the
    visibility. While sign-in is on, it passes a request to the flat only when the request carries the principal of a
    gate and the sequence of that gate is the stored sequence:
    - a request without a principal gets 403 with the text `This flat needs sign-in. Open it at its Portal address.`;
    - a request whose gate has another sequence gets 503 with `Retry-After: 5` and the text `The sign-in of this flat is
      changing. Try again.`
    This holds on every public provider. While sign-in is off, the check passes each request, as today.
45. For a request that passes rule 44 with a principal, the host sets:

    | Header | Value |
    | --- | --- |
    | `X-Flats-Visitor-Provider` | `oidc` or `credential` |
    | `X-Flats-Visitor-Subject` | the allowlist principal: an email, `sub:<subject>`, or the credential subject |
    | `X-Flats-Visitor-Email` | the verified email of the visitor, only when the provider vouched for one |

    The values are ASCII, because of rule 4 and because the SDK refuses an email that is not ASCII. Without a sign-in,
    the three headers are absent.
46. On every route that runs flat code (the public site, the private site and a preview), the host removes each
    spelling of the three headers of rule 45, and of `X-Portal-User`, `X-Portal-Auth`, `X-Portal-Proof` and
    `X-Portal-Access-Credential`, from the request of the client before flat code runs. A spelling differs in case or
    has `_` for `-`. So a client copy never reaches flat code, also on a route without the gate. A host operation of the
    docs worker is a request that the host builds, without a header of a client, and it goes through the same removal.
47. A static file, an asset of a document and a WebSocket upgrade pass the same gate and the same check as a page. The
    gate lets a same-origin WebSocket upgrade with a session through, and refuses a cross-origin one.
48. A preview has no public route (`servePublic` refuses it). So a visitor on Portal cannot open a preview, with sign-in
    or without it. A preview gets no headers of rule 45.

## Access links

49. Only the console issues an access link (decision 5). The Sign-in row of a flat with `credential` (rule 63) has the
    form Issue a link: a subject chosen from the allowlist, and an expiry as a whole number and a unit (minutes, hours
    or days), 7 days unless the operator changes it. The form calls `POST /flats/{slug}/signin/links` with
    `{"subject": ..., "expires": ...}`, which is a console-only route, as the decision of an approval is. `expires` is
    an RFC 3339 time, and the console sends it in UTC with whole seconds. A missing or empty `expires` is the default of
    rule 50. A missing `subject`, another member and a member of another type give 400. A caller of `/api` gets 403, on
    the loopback address with or without the CLI header, and from a remote address. The CLI has no command for it, and
    MCP has no route or tool for it.
50. The host refuses the link and changes nothing:
    - with 409, when the provider of the flat is not `credential`;
    - with 400, when the subject, after the normalization of rule 3, is not on the allowlist;
    - with 409, when the flat has no Portal route, or no relay of the exposure has a canonical host name. The message
      says that the flat must be Public on Portal with a ready relay;
    - with 400, when the expiry is less than 1 minute or more than 90 days from now.
    The default expiry is 7 days. The expiry is a time. The host truncates it to whole seconds in UTC, because a
    credential holds seconds, before it checks the limits.
51. The host makes the credential with `appauth.IssueCredential` from the identity of the entry, the canonical host name
    of the first relay of the exposure, the subject and the expiry, and the link with `appauth.CredentialRedeemURL`. A
    link never names an alias. The response is `{"url": ..., "subject": ..., "host": ..., "expires_at": ...}`. The
    console shows the link once, with a copy button and the text `Anyone who has this link can open <slug> as <subject>
    until <time>. It is not shown again.` `<time>` is the expiry in the date format of the console, the medium date and
    the short time of the locale of the browser. The console shows the link as text, never as a link that the browser
    could open and keep in its history.
52. The link and the credential are in that response only. No log line, flat event, audit row, webhook body, export
    line, MCP output or other API output holds them, and the console keeps no copy after the dialog closes.
53. A link works on its host name only and for the identity and the token secret of the flat. These end it: its
    expiry; a new token secret (rules 41, 42 and 54); the removal of its subject from the allowlist, for as long as the
    subject stays off the list. A change of the relay makes it fail on the new canonical host name, and it works again
    when its relay comes back. When the subject comes back on the
    allowlist, its earlier links work again, because a credential has no date. The docs say to run `flats signin
    revoke` to end them for good. A machine client can send the credential in `X-Portal-Access-Credential` instead of a
    session, as the SDK allows.
54. `flats signin revoke <slug>` calls `POST /api/flats/{slug}/signin/revoke`, which a loopback caller and the console
    can call, and the console has the button End all sessions and links on the Sign-in row. It needs no approval,
    because it narrows access. The host gives the identity of the flat a new token secret with the write of rule 36,
    ends the open connections of the public routes of the flat (as rule 20, step 3), and builds the gate again (rule
    38). Every session and every access link of the flat then ends, and the host names stay. The steps after the write
    of the token secret do not depend on the request: when the client drops it, the host still writes the row of rule
    67, ends the connections and builds the gate again, and the Portal route stays in service. A revocation of a flat
    without a Portal identity file returns `done` with the message `no Portal identity`. When the public routes do not
    open again, the message says `its public routes did not open again` and their state: the Portal route is stopped
    because its gate cannot be built (rule 35), or the host cannot read the flat or its routes, and then it stops the
    Portal route as rule 35 does, and every other public route of the flat answers 503 on the cancelled epoch until a
    deploy, a grant or a restart serves it again. The console shows such a message as an error.
55. The issue of a link writes the audit row of rule 67, a revocation writes the row of rule 67, and each sends the
    webhook event of rule 68.

## Views of the setting

56. `GET /api/flats/{slug}/signin` returns `{"provider": ..., "issuer": ..., "client_id": ..., "allow": [...],
    "client_secret": {"name": "oidc-client-secret", "issuer": ..., "client_id": ..., "updated_at": ...}, "hosts": [...],
    "callback_urls": [...], "seq": N}`. `client_secret` is `null` when no value is stored. `hosts` are the host names of the
    `PublicHosts` of rule 34. `callback_urls` are `https://<host>/_portal/auth/callback` for each host name when the
    provider is `oidc`, and empty otherwise. The console and a loopback caller can read the route. A remote caller gets
    403.
57. The flat view (`GET /flats`, `GET /flats/{slug}`, MCP `list_flats` and `get_flat`) has the member `sign_in` with
    `provider`, `issuer`, `client_id`, `allow_count` (the number of entries), and `client_secret` with the name
    `oidc-client-secret` when a value is stored, while sign-in is on. It has no member `sign_in` while sign-in is off.
    It holds no entry of the allowlist. The descriptions of the MCP tools say that `sign_in` protects the Portal URL
    only.
58. No MCP output and no answer to a remote caller holds an entry of the allowlist. Every MCP output that holds an
    approval (`get_approval`, `approve_request` and `reject_request` while Autopilot is on, and the pending result of a
    request tool), and every answer of `/api` that holds one, for every caller (`GET /api/approvals`,
    `GET /api/approvals/{id}` and the answer of `POST /api/flats/{slug}/signin`), show the `from` and `to` of a
    `set_signin` approval with the provider, the issuer, the client ID and the number of entries. The flat event that
    `requestFrozen` writes for such a request holds that counted form, so `get_logs` and `GET /api/flats/{slug}/logs`
    show no entry. An error of the SDK quotes an entry that it refuses, which a setting that a hand wrote into
    `flats.db` or a later SDK can hold: wherever such an error reaches the detail of rule 35, the host log, the result
    of an approval or a flat event, the host writes `<entry>` in place of each entry, raw or quoted. The 400 of rule 14
    keeps the entries, which only the caller who sent them gets. The entries are in the stored approval and setting in
    `flats.db`, the route of rule 56, `flats signin
    show`, the routes of the approvals under `/console/api`, which the console page of the approval reads (rule 64), and
    the audit rows of rule 65, which `flats audit ls` and `flats audit export` read on the host. Those two commands are
    for the operator, who reads `flats.db` with them.
59. `flats signin show <slug> [--json]` prints the setting, the binding of the client secret when one is stored, the
    current Portal host names and the callback URLs to register at the provider. `--json` prints the response of rule
    56.
60. No view holds the client secret or an access link.
61. The public notice of a Public flat with sign-in on is: `This flat is public on its Portal address behind sign-in:
    only the visitors that its sign-in allows can open it there. Its private routes, the console and MCP are not behind
    sign-in.` The pending notice of a visibility request for such a flat starts with `If approved, this flat becomes
    public on its Portal address behind sign-in:` and goes on as the public notice. For the Public level `edit`, each of
    the two notices goes on with `Visitors who sign in can edit it as guests.` and `Anyone who passes its sign-in can
    add, change and delete everything on it.` (for documents, `all the text of its documents`), and ends with the last
    sentence of the edit notice of a flat without sign-in: `Guest names are unverified, and guest text is untrusted
    input.` for the public notice, and `A safe point is pinned first, and the operator can restore earlier states from
    History.` for the pending notice. Sign-in does not check the name that a guest types (see "Known limits"). The
    other notices that say "anyone on the internet" or "anyone with the URL" (`LiveEditPublicNotice`, `ListedNotice`,
    `UnlistedNotice`, the line of `get_flat` on the level `edit`, and the notice that `get_approval` adds to an approved
    visibility change) say "anyone who passes its sign-in" for a flat with sign-in on.

    The CLI prints the pending notice of the host when it starts with `If approved`, and its own pending notice
    otherwise, as for an older host that sends none or the notice of a flat that is Public now. The console warns with
    the pending notice of a flat with sign-in on before it requests Public itself. The texts about Public in general
    (the descriptions and schemas of the MCP tools, the help of the CLI flags) describe no one flat and stay general.
    Each of them that says who can open or edit a Public flat also says that, with Portal sign-in on, it is whoever
    passes the sign-in.
62. The console shows the level of a Public flat with sign-in on followed by ` · Sign-in`: `Public · Sign-in`, and for
    a board or a document `Public, view only · Sign-in` or `Public, anyone can edit · Sign-in`. It shows it in both
    badges of the row of the flat in the flat list, in the Sharing row of the Settings tab of the flat, which is where
    the page of a flat shows its level, and on the page of a visibility approval of the flat. A Public flat without
    sign-in shows its level as today. A Private flat shows `Private`, also with sign-in on.
63. On the Settings tab, below Sharing, the console has the row Sign-in. It shows the provider (`OpenID Connect · <host
    of the issuer>`, or `Access links`), for `oidc` the issuer in full and the client ID, because one host can serve
    several issuers under paths, the number of entries with the list, whether a client secret is stored and for which
    issuer and client ID, the callback URLs, and these texts: `Sign-in protects the Portal address of this flat only.
    Its private routes, previews, the console and MCP are not behind sign-in.` and `Change it with flats signin set on
    the host. Each change waits for your approval here.` For `credential` it has the form of rule 49. It has the button
    of rule 54. While sign-in is off, the row reads `Off` and has the two texts and the button of rule 54, because links
    of an earlier `credential` setting can exist (rule 41). A stored client secret shows on the row also while sign-in
    is off or `credential`, with the text `Only an OpenID Connect sign-in with this issuer and client ID uses it.` (rule
    29). A pending `set_signin` approval shows as `Waiting for approval` with View request, as for a visibility request.
64. The approval list and the approval page describe a `set_signin` approval: `Turn on sign-in for <slug>`, `Change the
    sign-in of <slug>`, or `Turn off sign-in for <slug>: anyone can open its Portal address`. The page shows the stored
    and the requested setting, the added and the removed entries, a warning when the change widens access, and the text
    of rule 27 when the stored client secret has another binding. A change widens access when it turns sign-in off, adds
    an entry while sign-in stays on, or changes the provider, the issuer or the client ID. Turning sign-in on adds every
    entry and narrows access, so it gives no warning. While sign-in is on, the Networks section disables the switch of
    each other public provider and shows the reason of rule 8.

## Audit and the webhook

65. `audit_events` gets the kind `signin.set`. The host writes it in the transaction of step 2 of rule 20, with `flat`,
    no `name`, `via` `console`, the actor of the console (empty when the host applies the approval without a console
    request, as `resumeApplying` does), and the data `{"from": {"provider": ..., "issuer": ..., "client_id": ...,
    "count": N}, "to": {"provider": ..., "issuer": ..., "client_id": ..., "count": M}, "added": [...], "removed": [...],
    "approval": ID}`. When `added` and `removed` do not fit into the 4 KiB of the data, the row holds the first entries
    that fit and `"truncated": true`, as `network.set` does. Two issuers of 2048 bytes (rule 2) fill the 4 KiB alone, so
    when the row without entries does not fit either, each issuer is cut to its first 512 bytes, and the row says
    `"truncated": true`. So the change applies with its row in its transaction.
66. A set of the client secret writes `secret.set` with `name` `oidc-client-secret`, `via` `cli`, and the data
    `{"created": true or false, "use": "signin", "issuer": ..., "client_id": ...}`. A removal writes `secret.delete` with
    `{"use": "signin"}`. `flats secret reset` and the deletion of a flat write `secret.delete` with `"use": "signin"` and
    the fields that they write today.
67. The issue of a link writes `signin.link` with `flat`, no `name`, `via` `console`, the actor of the console, and the
    data `{"subject": ..., "host": ..., "expires_at": ...}`. The row holds no credential and no link. A revocation
    writes `signin.revoke` with `flat`, the via of its caller (`cli`, `api` for a loopback caller without the CLI
    header, or `console`), and the data `{}`, after the write of the file passes. A new token secret of rule 41 or rule
    42 writes `signin.revoke` with the via of the rename or the deletion and `{"cause": "rename"}` or `{"cause":
    "delete"}`, in the transaction of that rename or deletion, or, when the change fails after the write and before its
    commit, after that failure and outside its transaction. The via of a rename is `cli`, `api` or `console`. The via
    of a deletion is `console`, or `autopilot` with the Autopilot record of the agent as the actor when an agent approved
    it under [Autopilot](autopilot.md) ([audit events](audit-events.md), rule 5). When the host
    removes a new board or notebook because the first write that created it failed, the via is the one of that write
    (`mcp`, `cli` or `api`).
68. The webhook gets the type `signin.changed`, with `state` `applied` and `console_url` `<console>/flats/<slug>`:

    | `operation` | When | `change` |
    | --- | --- | --- |
    | `set_signin` | an approval applied a sign-in change. The body has `approval_id` and `decided_by`. | `{"from": {"provider": ..., "count": N}, "to": {"provider": ..., "count": M}}` |
    | `issue_signin_link` | an access link was issued | `{"expires_at": ...}` |
    | `revoke_signin` | the operator ended the sessions and links of the flat (rule 54) | `{}` |

    A rename and a deletion send no `revoke_signin`: their audit row of rule 67 records the new token secret. No body
    holds an entry of the allowlist, a subject, the client secret, a credential or a link. A set or a removal
    of the client secret sends no webhook event, as no secret change does ([event webhook](event-webhook.md), "Scope"):
    the secret decides the access of nobody, and its audit row of rule 66 records it.
69. `approval.created` and `approval.decided` of a `set_signin` approval have the `operation` `set_signin`, as the other
    approvals have their action.

## Compatibility

70. A flat with sign-in off behaves as today, with these differences: rule 46 removes client copies of its seven
    headers on every route that runs flat code; a rename and the deletion of a flat give the Portal identity a new token
    secret (rules 41 and 42), which no flat without sign-in uses; and a rename or a deletion now fails, and changes
    nothing, when the identity file cannot be written. Flat code that read such a header from a client stops seeing it.
    The docs say so.
71. An older CLI has no `signin` command. An older host answers 404 to the routes of this spec, and the new CLI says
    that the host is older than the command.
72. Schema 29 adds to `flats` the columns `signin_provider`, `signin_issuer` and `signin_client_id` (text, empty by
    default), `signin_allow` (text, `[]` by default) and `signin_seq` (integer, 0 by default). It adds to `secrets` the
    columns `use` (text, empty by default, with the check `use IN ('', 'signin')`), `signin_issuer` and
    `signin_client_id` (text, empty by default), and a unique index on `flat` of the rows whose use is `signin`. The
    migration runs in the transaction of the version bump. Every flat gets sign-in off, and every secret keeps an empty
    use, also a row named `oidc-client-secret` (rule 31). An older release refuses a database of schema 29, as it
    refuses one of schema 28. The tests that name schema 28 as the latest change with it. The docs say to make a backup
    with `flats backup` before the upgrade, and that a return to an older release needs the copy from before the
    upgrade.
73. A flat that [public instance mode](public-instance.md) made Public on a default network other than Portal holds that
    grant. Rule 8 refuses sign-in on for it, and its message says to make the flat Private, revoke that grant, and turn
    sign-in on before the flat is Public again.

## Changes to other specs

74. These specs change with the implementation:
    - [audit events](audit-events.md): the table gets the rows `signin.set`, `signin.link` and `signin.revoke`, the row
      `secret.set` and the row `secret.delete` get `"use": "signin"`, rule 3 allows the fields of rules 65 to 67, rule 4
      names the channels of those rows, and the scope names this spec;
    - [event webhook](event-webhook.md): rule 1 gets the type `signin.changed`, rule 4 gets its `operation` values, its
      `change` objects and `approval_id` and `decided_by` for `set_signin`, the operation values of the approval types
      get `set_signin`, and the scope says that a change of the sign-in client secret, as every secret change, sends
      nothing;
    - [injected fetch secrets](injected-fetch-secrets.md): rule 1 says that a row with the use `signin` is neither
      readable nor injected, and rule 19 says that the capture reads only rows with an empty use;
    - [flats behind a reverse proxy](reverse-proxy-sites.md): the out-of-scope bullet on the authentication of visitors
      names this spec for Portal visitors and keeps #139 for operators, rule 4 says that `proxy-public` does not serve a
      flat with sign-in on, and rule 5 says that sign-in does not cover `proxy`;
    - [Autopilot](autopilot.md): rule 6 says that a `set_signin` approval has no switch, so agents can neither approve
      nor reject it;
    - [public instance mode](public-instance.md): a note on rule 73;
    - `docs/spec/README.md`: the row of this spec.
75. [Windows support](windows-support.md) does not change. The identity file keeps the access list that the SDK gives it
    (its rule 18), also after a write of rule 36, and this spec adds no file on disk.

## Docs

76. These docs and texts change:
    - `docs/networking.md`: the Portal row of the provider table, the section Visibility (Public is not access control,
      except on the Portal URL of a flat with sign-in on), and a new section on Portal sign-in. The section says what
      sign-in protects and what it does not (rule 12), how to register the callback URLs, that a relay change or a
      rename changes the host names, that explicit relays keep the host names stable, and how to issue and end access
      links (rules 49, 53 and 54);
    - `docs/design.md`: Exposure (the sentence "Public is never access control"), Secrets (the sign-in secret),
      Approvals (`set_signin`), and Server flats (the headers of rule 45 and rule 46);
    - `docs/surfaces.md`: the routes of rules 13, 25, 49, 54 and 56, the CLI row of `signin`, the flat view, the MCP
      texts and the console screens;
    - `docs/configuration.md`: the backup before the upgrade to schema 29, the audit kinds, and the webhook type;
    - the agent topics of server code and the generated `docs/runtime-api-v1.md`: the headers of rule 45;
      `docs/agent/topics/approvals.md`: the `set_signin` approval, which only the operator decides;
    - the texts in code: the MCP instructions (`docs/instructions.go`, the line that says that Public is not access
      control), the notices of rule 61, the MCP descriptions of `get_flat`, `list_flats` and `get_approval`, the CLI
      help, and the console texts of rules 62 to 64;
    - the tests that pin these texts change with them.

## Known limits

- Sign-in protects the Portal URL only. A visitor on a private route keeps the rights of a private visitor, also on a
  `proxy` route that the reverse proxy of the operator opens to many people.
- The provider needs one callback URL for each Portal host name of the flat. A relay that discovery replaces, a pin that
  moves, an alias that is lost or comes back, and a rename change the host names. Until the operator registers the new
  URL, nobody can sign in on that host name. Explicit relays keep the host names stable.
- The token secret of the Portal identity, in `portal/<slug>.json`, signs every session and every credential. The host
  key does not seal it, `flats secret rotate-key` does not change it, and `flats backup` copies it. Whoever reads the
  file, also an agent that runs as the account of Flats, can make a session or a link for any subject on the allowlist
  without Flats. Decision 5 covers the surfaces of Flats only. `flats signin revoke` gives the identity a new token
  secret.
- An access link cannot be revoked alone. Its subject is a label: whoever holds the link is that subject until the link
  expires, the subject leaves the allowlist, or the operator runs `flats signin revoke`. A subject that comes back on
  the allowlist makes its earlier links work again (rule 53).
- A rename and `flats signin revoke` end every session and every access link of the flat, also of the visitors who stay
  on the allowlist.
- A machine client, such as a service that sends webhooks to the flat, cannot sign in. Only a credential in
  `X-Portal-Access-Credential` lets it in.
- The gate answers some requests itself (sign-in pages, refusals). Analytics does not count them.
- The flat cannot serve the endpoint paths of the gate (rule 40) on its Portal URL while sign-in is on.
- `appauth.Check` runs only after a build. A provider that fails later shows only to the visitor who signs in.
- The SDK fetches the issuer from the host with its own HTTP client. The origin grants of the flat do not apply, and an
  issuer on a private network is reachable.
- The headers of rule 45 reach server flats only. A static flat cannot read them. A guest name on a board or a document
  is still the name that the visitor types.
- A short request that the gate passed before a sign-in change can finish after the change, or step 3 of rule 20 can
  cut it.
- The console has no form that turns sign-in off. When the identity provider fails, an operator away from the host can
  make the flat Private in the console, but cannot open it without sign-in.
- A relay holds the certificate key of its host names and signs the TLS handshakes. A relay that misuses the key can
  end the TLS itself and read sessions, credentials and content. The SDK checks for that with self-probes, and Flats
  only logs their warnings. A flat with sign-in on is as safe as its relays; explicit relays that the operator trusts
  are the safe choice.
- A program that can reach the console address can send console requests ([Autopilot](autopilot.md), rule 2), also
  to the link route of rule 49.
- A restore of an older backup brings back the token secret of that time, so the links that a later `flats signin
  revoke` ended work again until their expiry.

## Completion criteria

Test seams. The Portal provider has two seams that tests in package `portal` set: the function that makes an exposure
and the function that runs HTTP (the fields `expose` and `runHTTP` of `Net`). The check of rule 2 that refuses an `http`
issuer is in package `core`, so a test in package `portal` can use an OIDC provider on a loopback `http` server, which
the SDK accepts. A check failure uses an `https` issuer on a closed loopback port. A build failure uses a setting that a
test wrote into `flats.db`, a client secret sealed by another key or bound to another issuer, and an identity file
without a token secret in a folder that the test cannot write. A test that needs a folder that it cannot write runs on
Unix as a user that is not the superuser, and skips elsewhere. A host stop between two steps uses a seam that fails the
step after the commit. No test needs the network.

- The setting: the migration from schema 28 gives every flat sign-in off and every secret an empty use, also a row named
  `oidc-client-secret`; a reader of schema 28 refuses schema 29; each limit and each normalization of rules 2 to 4, with
  the order and the duplicates; an issuer is stored without its trailing `/`; each value that `appauth.Validate` refuses gives 400
  at the request; an `http` issuer on a loopback host is refused; a rename keeps the setting and the deletion removes
  it.
- The request and the approval: each 400 of rule 14 with its message; a remote caller gets 403 from the routes of rules
  13, 25, 54 and 56; MCP has no tool that requests a change, sets the client secret, issues a link or revokes; a request
  for the stored setting is `done` and makes no approval; a pending approval changes nothing on the Portal route; an
  equal request returns the open approval; the frozen parameters hold no client secret; each stale case of rule 18
  fails and names what changed, and a redeploy, a rollback, a visibility change, a client secret change and a change of
  the host settings of a provider leave the approval valid; a change and its reverse make an earlier request stale
  (rule 19); `AutopilotNeeds("set_signin", ...)` is unclassified, and an Autopilot decision is refused; a console
  decision applies the setting; the result and the flat event hold no entry; a stop after step 2 of rule 20 ends, after
  the next start, as `approved` with one stored change, one audit row, no webhook event and a gate from the stored
  setting; a sign-in change makes a pending `set_visibility`, `activate`, `rollback` and `create_version` approval
  stale (rule 23); a revocation of rule 54 leaves a pending `set_signin` approval valid.
- Portal only: each refusal of rule 8, with 409 and its message for a Private and a Public flat; a grant and a sign-in
  apply that run at the same time, many times, give the result of one order; a `flats.db` that a test wrote with
  sign-in on and a `tailscale-funnel` grant makes `ensureLifecycle` ask no Funnel route, and a request marked with
  `tailscale-funnel` gets the 403 of rule 44; the refusal of rule 73 for a flat of public instance mode.
- The gate build failure: each case of rule 35, also a client secret with another binding, opens no Portal route for a
  new entry (the fake exposure is never called and the fake HTTP run never gets the handler of the flat), stops the
  route of a served entry, shows the endpoint state `error` with the detail, writes one log line, and keeps the other
  routes; no case serves the flat without the gate; the next build after the cause is removed opens the route.
- The gate: a Portal route of a flat with sign-in on sends a request without a session to the sign-in page; a second
  `Serve` with a new handler keeps the gate and sends a signed-in request to the new handler; a removed entry loses
  access at the next request after the change; a request that the old gate let in gets the 503 of rule 44 until the new
  gate is in place; sign-in off serves the handler box without a gate; an identity file without a token secret gets one
  and keeps its address (rule 36); a failed check writes the warning and the detail, and the gate still refuses a
  visitor without a session; `PublicHosts` gives the canonical host names with a zero `Since`, the alias host names with
  their `AliasSince`, and the host names of a relay that reports `connecting`; a session from before the `Since` of an
  alias is refused, and a request for another host name gets 421; each endpoint path of rule 40 never reaches the flat,
  and another path under `/_portal/auth/` reaches it behind the gate; a reopen of the route without a setting keeps the
  gate; an open with a setting of a lower sequence keeps the gate of the higher one.
- Open connections: a WebSocket that is open on the Portal route ends at step 3 of rule 20 when sign-in goes on, when
  its subject leaves the allowlist, and at a revocation of rule 54, and a new upgrade goes through the gate; after each
  of those, an allowed visitor gets the page of the flat, not 503.
- Rename and deletion: a live rename gives the old identity a new token secret, so its sessions and links are refused,
  also for a flat with sign-in off that had links of an earlier `credential` setting; a failed write makes the rename
  and the deletion fail and change nothing; a failure after the write writes the row of rule 67 and a flat event, and
  its message says that the sessions and links ended;
  the old slug serves only the redirect, without a gate, before and after a host restart; a later sign-in change of the
  flat does not reach the old slug; a deletion gives the identity a new token secret, and a new flat with the same slug
  and the same subject on its allowlist refuses a link of the deleted flat and keeps the host name.
- A visitor outside the allowlist: with no session, with a session of a removed entry, and with a credential of a
  subject that is not on the list, the visitor cannot open a page, a static file or an asset of a document on the Portal
  route. A preview of the flat has no Portal route with sign-in on and Portal granted, and its private route serves no
  headers of rule 45.
- Survival: the setting and the gate survive a redeploy, a rollback and a host restart (a new `Net` on the same folder
  accepts a session of the canonical host name from before the restart); a rename keeps the setting and builds a gate
  with the new identity.
- Each request: the headers of rule 45 for `oidc` with an email entry, with a `sub:` entry, and for `credential`, and
  none without a sign-in; client copies of each header of rule 46, in mixed case, never reach flat code on the public
  route, the private route, a preview and the host operations of the docs worker; the docs worker sees `public` for a
  signed-in visitor; a same-origin WebSocket upgrade with a session passes, and a cross-origin one is refused.
- The client secret: a set and a removal through the CLI header on loopback, 403 for a remote caller and for a loopback
  caller without the CLI header, 404 from the console; each 400 of rule 26; a test stores a distinctive value and shows
  that no `env`, worker spec, log line, flat event, audit row, webhook
  body, export line, approval, API, MCP, CLI or console output and no status holds it, and that `flats secret ls`,
  `list_secrets` and the console list of secrets do not list it; after an approved change of the issuer, a fake issuer
  at the new address gets no token request that holds the value, and the route stops with the detail of rule 27;
  rule 31 refuses `flats secret rm` and the console delete of it, and a readable row named `oidc-client-secret` makes
  `flats signin secret set` refuse; `rotate-key` keeps its use and binding and opens it; `restore-key` counts it;
  `reset` names and removes it; a set while sign-in is `oidc` rebuilds the gate; a removal while the routes of the flat
  cannot be read stops the Portal route, and so does a revocation of rule 54, whose message names the state of the
  routes; a rebuild of rule 28 and an open of the public routes while the flat cannot be read stop the Portal route;
  a set, a removal, a revocation and an approved change whose request is cancelled after the commit leave the Portal
  route in service with a gate of the stored setting, and their messages name no stop; a set or a removal sends no
  webhook event.
- Access links: the route of rule 49 gives 403 to a loopback caller with and without the CLI header and to a remote
  caller, MCP has no tool for it, and the console issues the link; each refusal of rule 50 and each 400 of the body of
  rule 49; the link has the form of `CredentialRedeemURL` for the canonical host name; the default and the limits of the
  expiry, and its truncation to whole seconds; the link signs a visitor
  in on its host name and not on another host name of the flat; a link of a subject that left the allowlist is refused,
  and works again when the subject comes back; after `flats signin revoke` every session and every link is refused, the
  host names stay, and a new link works; the credential in `X-Portal-Access-Credential` lets a request in; the link is
  in no log line, flat event, audit row, webhook body or other output.
- The CLI: each form of the synopsis, each usage error with exit status 2, the help text, and the message of rule 71
  for an older host.
- The views: the route of rule 56 with its members and the binding of the client secret, and no value; `sign_in` in the
  flat view while sign-in is on, with `allow_count` and no entry, and absent while it is off; a distinctive entry is in
  no output of `get_flat`, `list_flats`, `get_approval`, `approve_request` (which refuses a `set_signin` approval) and
  `get_logs`, and in no answer of the approval and log routes to a remote caller; `flats signin show` with and
  without `--json`; the notices of rule 61, also for the level `edit`.
- Audit and the webhook: each row of rules 65 to 67 with its `via` and its data, in the transaction of its change where
  rule 67 says so (a failing audit write leaves the change undone), and after the write of the file for a revocation; `added` and `removed` that do not fit have `truncated`; each operation
  of rule 68 sends one `signin.changed` with the fields of its row and no entry, subject, secret, credential or link.
- The console: the UI test shows `Public · Sign-in` for a Public flat with sign-in on and `Public` for one without it,
  the Sign-in row with the texts of rule 63, also while sign-in is off, the link form with its default expiry and the
  link that it shows once and keeps no copy of, the button of rule 54, the description, the warnings and the binding
  text of rule 64, no warning for turning sign-in on, and the disabled switches of the other public providers; no
  console text holds the client secret.
- The docs: `docs/networking.md`, `docs/design.md` and the MCP descriptions of `get_flat` and `list_flats` say that
  sign-in protects the Portal URL and not the private routes, the console or MCP. The MCP instructions name a Public
  route behind Portal sign-in as the exception to "Public is NOT access control" and stay within the size limit of
  their tests. The doc checks pass, and the generated references match their sources.

## Open questions

None.
