# Portal sign-in

Status: planned.

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

## Current state

Nothing of this spec is implemented. "Before this change" lists what exists. The SDK v2.7.4 with `sdk/appauth` is in
`go.mod` already.

## Scope

In scope: the sign-in setting of a flat, its storage, its request and its approval, the refusal of other public
providers, the OIDC client secret, the gate on the Portal route, the check of each request, the headers of the visitor,
the access links of `credential`, the views of the setting in the API, the CLI, MCP and the console, the audit kinds,
the webhook event, the schema, and the docs.

Out of scope:

- The providers `siwe`, `saml` and `email` of the SDK.
- Sign-in on any route other than the Portal route of the current slug: the other public providers, the private routes,
  previews, the rename redirect of an old slug (rule 39), the console, MCP and the API.
- The rights of a private visitor for a signed-in visitor. A signed-in visitor is a Public visitor (decision 3).
- A console form that requests a change, sets the client secret or issues an access link.
- Revocation of one access link. The removal of a subject from the allowlist ends every link of it (rule 49).
- Host-wide sign-in (#222).
- Protection of the Portal identity file with the host key (see "Known limits").

## Decisions

The owner of the repository decided five points:

1. Sign-in applies to Portal only. While a flat has sign-in on, the flat is Public through Portal alone. The host
   refuses to make the flat Public with another public provider (`tailscale-funnel`, `zrok`, `cloudflare-quick`,
   `netbird-expose` or `proxy-public`), to grant another public provider, and to turn sign-in on while another public
   provider is granted. Each refusal names the next action. The host never falls back to open.
2. The first version has two providers: `oidc` (client ID, issuer, sealed client secret, an allowlist of verified
   emails, `@domain` entries or `sub:` subjects) and `credential` (the operator issues access links with the CLI).
3. A visitor who passed the sign-in stays a Public visitor. The flat still sees `X-Flats-Access: public`. Documents,
   boards and designs stay read-only, or open to guest editing as the Public level says. Revisions stay private.
4. Flat code gets the identity of the visitor in trusted headers. The host passes the provider, the subject and the
   email of the signed-in visitor. It removes any client copy of those headers on every route, public and private.
   Without a sign-in the headers are absent.
5. The operator issues an access link with `flats signin link` on the host, without an approval. A link names one
   subject of the approved allowlist and expires after at most 90 days. MCP has no tool for it. Whoever can run the
   CLI on the host can also read `portal/<slug>.json`, whose token secret signs every session and link, so an approval
   would not stop that person (see "Known limits").

The author of this spec decided these points, and each can change before the implementation:

- The setting is five columns of `flats`, and the client secret is a row of `secrets` with a new column `use`, in
  schema 29 (rule 66). The check of each request reads the flat row that it reads today, and a rename, a deletion and a
  backup already move, remove and copy that row.
- Every change of the setting needs an approval, also off and each change of the allowlist (rule 16). Every change can
  widen access, and off opens the Portal URL to anyone.
- An Autopilot agent can neither approve nor reject a sign-in approval (rule 17). The allowlist says who can read the
  flat, and an agent that decides the request of another agent still widens access without a person.
- The CLI and the API on the loopback address request a change. MCP only reads the setting, without the entries of the
  allowlist (rule 52). The console shows it and
  decides the approval (rules 13, 17, 52 and 57). This follows [injected fetch secrets](injected-fetch-secrets.md), and
  it keeps the MCP agents read-only.
- The OIDC client secret is a flat secret with the use `signin`, which the CLI sets and removes without an approval
  (rules 23 to 30). The secret proves Flats to the provider and decides the access of nobody: a wrong value makes the
  sign-in fail, and it lets no other visitor in.
- `credential` needs at least one subject on the allowlist, which the SDK does not need (rule 3). The approved allowlist
  limits who a link can name (decision 5, rules 45 to 50), and a link cannot be revoked alone.
- The Portal provider builds the gate where the identity is, around the handler box of the entry (rule 31). So a
  redeploy or a rollback replaces the handler inside the gate. A gate that cannot be built gives the flat no Portal
  route (rule 33). A failed check of the provider is a warning, and the gate stays (rule 35).
- The public site handler checks each request against the stored setting (rule 40). A code path that opens a public
  route without the gate, or a gate that an older setting built, then gives no access.
- `PublicHosts` comes from the current Portal URLs, with a `Since` for each alias (rule 32). A session of an earlier
  holder of an alias does not count.
- The rename redirect of an old slug stays without a gate (rule 39). It sends no content, its target has the gate, and
  the sessions of the old identity do not count at the new identity.
- The issuer is `https` only (rule 2). The host fetches the issuer itself, and an `http` issuer on the loopback address
  can reach a local service.
- An allowlist entry has visible ASCII characters and the space only, a list has at most 100 entries, and the host
  stores it in byte order (rule 4). An entry goes into a header, the console and the approval page, and the order makes
  two equal lists equal.
- The headers are `X-Flats-Visitor-Provider`, `X-Flats-Visitor-Subject` and `X-Flats-Visitor-Email`, and the host also
  removes client copies of the Portal identity headers on every route (rules 41 and 42). Flat code that reads the names
  of Portal must not trust a client copy on a route without the gate.
- An access link names the canonical host name by default (rule 46). An alias can change hands, and the canonical host
  name belongs to the identity.
- The audit log gets `signin.set` and `signin.link`, and a client secret change writes `secret.set` and `secret.delete`
  with `"use": "signin"`. The webhook gets one type, `signin.changed`, with two operations, and its body names no
  entry and no subject (rules 59 to 63). This follows rule 5 of [event webhook](event-webhook.md): a body names no login
  or address of a person.
- The Portal provider writes the token secret into an identity file that has none (rule 34). For such a file the SDK
  makes a new random token secret at each load, and every session and link would end at each restart.

## The setting

1. The sign-in setting of a flat has a provider and the fields of that provider. The provider is `oidc`, `credential` or
   empty. Sign-in is on when the provider is not empty. A new flat has sign-in off.
2. An `oidc` setting has an issuer, a client ID and an allowlist. The issuer is an `https` URL with a host name and
   without user information, query or fragment, of at most 2048 bytes. The host stores it as the SDK normalizes it,
   without a trailing `/`. The host refuses an `http` issuer, also on a loopback host, which the SDK accepts. The client
   ID has 1 to 256 bytes of visible ASCII. Each entry of the allowlist is a verified email address, an `@domain` entry
   or a `sub:<subject>` entry. The host normalizes each entry as `appauth.Config.Normalize` does: an email and a domain
   in lower case, a subject byte for byte.
3. A `credential` setting has an allowlist of subjects. The host normalizes each subject as the SDK does (spaces at the
   ends are removed). The SDK opens an empty credential allowlist to any credential of the identity. The host refuses an
   empty list.
4. An allowlist has 1 to 100 entries. Each entry has only visible ASCII characters and the space, and at most 256 bytes
   without the prefix `sub:`. The host removes duplicates after the normalization and stores the list in byte order. Two
   settings are the same when their providers, issuers, client IDs and stored lists are equal.
5. An `oidc` setting can have a client secret (rules 23 to 30). Without one, the SDK uses the setting as a public client
   with PKCE.
6. `flats.db` stores the setting with the flat. A redeploy, a rollback, a data restore, a rename and a host restart keep
   it. The deletion of a flat removes it. Only an approval changes the setting (rule 16). Only rules 24 and 26 change
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
     providers is granted. The message names the provider, and its removal under Networks on the Settings tab of the
     flat;
   - a sign-in request, or the application of a sign-in approval, that turns sign-in on while one of those providers is
     granted. The message names the provider and its removal under Networks.
9. The store checks the grants and the setting, and writes the grant or the setting, in one write transaction. So a
   grant and a sign-in change that run at the same time give the result of one of the two orders.
10. While sign-in is on, the host asks no public provider other than Portal to serve the flat, also when a stored grant
    permits one. Rule 40 refuses a request that arrives on such a route.
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
flats signin secret set <slug>
flats signin secret rm <slug>
flats signin link <slug> <SUBJECT> [--expires DURATION|TIME] [--host HOST]
```

A missing flag, a flag that the provider does not take, and a second positional argument are usage errors (exit
status 2). `flats help signin` names each form, and says that sign-in protects the Portal URL only.

13. `flats signin set` and `flats signin off` call `POST /api/flats/{slug}/signin`. The route takes a caller on the
    loopback address, with the CLI header or without it. A remote caller gets 403. MCP has no tool that requests a
    change, and the console has no form for it.
14. The body is `{"provider": "oidc", "issuer": ..., "client_id": ..., "allow": [...], "reason": ...}`, `{"provider":
    "credential", "allow": [...], "reason": ...}`, or `{"provider": "", "reason": ...}` for off. `reason` is optional
    text of at most 1 KiB. A member that the provider does not take, a member that is missing, and a value that breaks
    rules 2 to 4 give 400. The message names the member and the rule that it breaks. The message for an issuer does not
    repeat the text of the caller, because it can hold user information or a query.
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
    change, a client secret change and a change of the host settings of a provider do not.
19. The sign-in sequence of a flat starts at 0 and grows by 1 with each applied sign-in change. A sign-in change of a
    flat and back to the first setting still makes an earlier request stale.
20. The host applies an approval in this order:
    1. It builds the configuration of the gate from the frozen setting and the stored client secret, and checks it with
       `appauth.Validate`. When that fails, the approval fails and nothing changes.
    2. It writes the setting, the new sequence and the audit row of rule 59 in one transaction, with the check of
       rule 9.
    3. When the flat has a Portal route, the Portal provider builds the new gate and puts it in place of the old one
       (rules 31 and 33).
    4. It writes the flat event and sends the webhook event of rule 62.
21. Between steps 2 and 3, rule 40 refuses each request that the old gate lets in. When step 3 fails, the Portal route
    of the flat stops (rule 33). The approval is then `approved`, and its result says that the setting is stored and the
    Portal route is stopped, with the reason.
22. The result of an applied approval says `done` and names the provider, the issuer and the number of entries. It never
    holds an entry of the allowlist, so that the flat event has none either.

## The client secret

23. The OIDC client secret is a flat secret with the use `signin` and the name `oidc-client-secret`. A flat has at most
    one. The name is not a valid name of an environment variable, so it never collides with a variable or a secret of
    the flat code.
24. `flats signin secret set <slug>` reads the value as `flats secret set` does (from a terminal without echo, or from a
    pipe with one trailing newline removed) and calls `PUT /api/flats/{slug}/signin/secret` with `{"value": ...}`.
    `flats signin secret rm <slug>` calls `DELETE` on the same route. Only a loopback caller with the CLI header can
    call the route, as `operatorVia` allows for `cli`. A remote caller gets 403. The console has no route for it.
25. A value has 1 to 4096 bytes of visible ASCII, without a space or a control character. Another value gives 400, and
    the stored value stays. The SDK removes spaces at the ends of a client secret, and the sent bytes must be the stored
    bytes.
26. A set or a removal needs no approval. It applies at once. When sign-in is `oidc` and the flat has a Portal route,
    the Portal provider builds the gate again with the new value (rule 31). When that fails, rule 33 stops the route.
27. A set is allowed while sign-in is off or `credential`. The value then waits for an `oidc` setting. Only an `oidc`
    gate reads it.
28. No text and no interface of the host holds the value: `env`, the injected list, a `fetch`, a log line, a flat event,
    an audit row, a webhook body, the export, an approval, the outputs of the API, MCP, the CLI and the console, and the
    status. An activation reads only the secret rows with an empty use, so the value never reaches a worker. `flats
    secret ls`, `GET /flats/{slug}/secrets`, MCP `list_secrets` and the console list of secrets do not list it. The
    sign-in view says whether a value is stored and when it was set (rule 51).
29. `flats secret set` refuses the name, because it is not a valid name (as today). With 409, `flats secret rm <slug>
    oidc-client-secret`, its route and the delete of a secret in the console refuse to remove the sign-in secret. The
    message names `flats signin secret rm <slug>`.
30. The commands that treat every secret treat this one. `flats secret rotate-key` seals it again and keeps its use.
    `flats secret restore-key` opens it and counts it. `flats secret reset` removes it, and its list names it under the
    flat. `flats backup` copies it. A rename keeps it. The deletion of the flat removes it with a `secret.delete` event.

## The gate

31. The Portal provider builds the gate with `appauth.New(next, id, cfg)`:
    - `id` is the Portal identity of the slug. The entry keeps it from now on;
    - `next` is the handler that the entry serves today, `publicHandler` of its handler box, in a wrapper that marks
      each request with the sign-in sequence that the gate was built from (rule 40);
    - `cfg` has the provider, the allowlist in `EmailList` for `oidc` or in `Allow` for `credential`, the issuer, the
      client ID and the opened client secret for `oidc`, `IdentityHeaders` false, no `IdentityProof`, and the
      `PublicHosts` of rule 32.
32. `PublicHosts` gives, on each call, the canonical URL of each ready relay with a zero `Since`, and each alias URL
    with the `AliasSince` that the exposure reports for that relay (`RelayStatus`, the time when this exposure came to
    hold the alias), as the `portal-tunnel` command of the SDK does. A lost alias leaves the list. When the alias comes
    back, it gets a new `Since`. The gate answers a request for another host name with 421.
33. When the setting cannot be read, the client secret cannot be opened, rule 34 cannot keep the token secret of the
    identity, or `appauth.New` fails, the Portal provider builds no gate and does not serve the flat on Portal. For a
    new entry it opens no route. For an entry that it serves, it stops the route and unregisters it from its relays. It
    never serves the flat without the gate while sign-in is on. The Portal endpoint of the flat has the state `error`
    and the detail `sign-in gate: <reason>`, with the next action. The host log has one line with the slug and the
    reason. The other routes of the flat stay. A visibility approval that needs the Portal route fails with that reason,
    as it fails today when no public route opens. The next build tries again: a deploy, a grant, a restart, a sign-in
    change or a client secret change.
34. Before the first build for an identity, the Portal provider checks that the identity file holds a token secret. When
    the file has none, the provider writes the file again with the identity that it loaded, atomically and with mode
    0600 (on Windows the access list of the SDK). The address, the keys and the canonical host name stay the same. When
    the write fails, rule 33 applies.
35. After each build of an `oidc` gate, the Portal provider runs `appauth.Check` in the background, with a limit of 15
    seconds, outside the lock of the slug and outside the path of a request. A failure writes one warning line in the
    host log and the detail `sign-in provider check failed: <reason>` on the Portal endpoint, until a later build passes
    the check. The gate stays as it was built and never lets a visitor in without a sign-in. A `credential` gate has no
    check.
36. The Portal provider builds the gate when it opens a Portal route for a flat with sign-in on (an approved deploy or
    visibility change, a grant, a host start, a rename), when a sign-in change applies (rule 20), and when the client
    secret changes (rule 26). A redeploy, a rollback, a data restore and a replacement of the worker put a new handler
    into the handler box and keep the gate. When sign-in goes off, the provider removes the gate and serves the handler
    box as today. Each open of the Portal route of a flat carries the stored setting, which the host reads under the
    lock of the flat. An open that carries no setting never removes a gate: only an applied change to off removes it.
    The provider never puts a gate built from a lower sequence in place of a gate built from a higher one, so an open
    that read the setting before a change cannot leave a gate that rule 40 refuses until the next change.
37. A gate keeps the sessions of the gate before it when both have the same identity and the same provider, issuer and
    client ID, and the gate checks the allowlist on every request. So a redeploy, a rollback, a host restart, a change
    of the allowlist and a client secret change keep the session of a visitor who stays on the allowlist. A visitor who
    leaves the allowlist loses access at the next request after rule 20 applies. A change of the provider, the issuer or
    the client ID ends every session. A rename ends every session and every access link, because the new slug has a new
    identity. A session on an alias host name ends at a host restart and when the alias comes back (rule 32).
38. The gate owns the paths under `/_portal/auth/`. While sign-in is on, a request for such a path on the Portal URL
    never reaches the flat.
39. The rename redirect of an old slug has no gate. It answers with a redirect to the new address and serves no content
    of the flat, and the new address has the gate.

## Each request

40. The public site handler reads the setting and the sequence with the flat, for each request, as it reads the
    visibility. While sign-in is on, it passes a request to the flat only when the request carries the principal of a
    gate and the sequence of that gate is the stored sequence:
    - a request without a principal gets 403 with the text `This flat needs sign-in. Open it at its Portal address.`;
    - a request whose gate has another sequence gets 503 with `Retry-After: 5` and the text `The sign-in of this flat is
      changing. Try again.`
    This holds on every public provider. While sign-in is off, the check passes each request, as today.
41. For a request that passes rule 40 with a principal, the host sets:

    | Header | Value |
    | --- | --- |
    | `X-Flats-Visitor-Provider` | `oidc` or `credential` |
    | `X-Flats-Visitor-Subject` | the allowlist principal: an email, `sub:<subject>`, or the credential subject |
    | `X-Flats-Visitor-Email` | the verified email of the visitor, only when the provider vouched for one |

    The values are ASCII, because of rule 4 and because the SDK refuses an email that is not ASCII. Without a sign-in,
    the three headers are absent.
42. On every route that runs flat code (the public site, the private site, a preview, the host operations of the docs
    worker), the host removes each spelling of the three headers of rule 41, and of `X-Portal-User`, `X-Portal-Auth`,
    `X-Portal-Proof` and `X-Portal-Access-Credential`, from the request of the client before flat code runs. So a client
    copy never reaches flat code, also on a route without the gate.
43. A static file, an asset of a document and a WebSocket upgrade pass the same gate and the same check as a page.
44. A preview has no public route (`servePublic` refuses it). So a visitor on Portal cannot open a preview, with sign-in
    or without it. A preview gets no headers of rule 41.

## Access links

45. `flats signin link <slug> <SUBJECT> [--expires DURATION|TIME] [--host HOST]` calls `POST
    /api/flats/{slug}/signin/links` with `{"subject": ..., "expires": ..., "host": ...}`. Only a loopback caller with
    the CLI header can call the route. A remote caller gets 403. MCP has no tool for it, and the console has no form for
    it.
46. The host refuses the link and changes nothing:
    - with 409, when the provider of the flat is not `credential`. The message names `flats signin set`;
    - with 400, when the subject, after the normalization of rule 3, is not on the allowlist. The message names `flats
      signin show`, and a change of the allowlist with `flats signin set`;
    - with 409, when the flat has no ready Portal route. The message says that the flat must be Public on Portal;
    - with 400, when `--host` is not a current Portal host name of the flat. The message lists the current host names;
    - with 400, when the expiry is less than 1 minute or more than 90 days from now.
    Without `--host`, the link names the canonical host name of the first ready relay. Without `--expires`, it expires
    after 7 days.
47. The host makes the credential with `appauth.IssueCredential` from the identity of the entry, the host name, the
    subject and the expiry, and the link with `appauth.CredentialRedeemURL`. The response is `{"url": ..., "subject":
    ..., "host": ..., "expires_at": ...}`. The CLI prints the link on stdout. On stderr it prints the subject, the host
    and the expiry, and says that anyone who has the link can open the flat as that subject until it expires.
48. The link and the credential are in that response only. No log line, flat event, audit row, webhook body, export
    line, MCP output or other API output holds them.
49. A link needs no approval. It works on its host name only and for the identity of the flat. A rename, a new relay or
    a lost alias of its host name, and the removal of its subject from the allowlist, end it. A machine client can send
    the credential in `X-Portal-Access-Credential` instead of a session, as the SDK allows.
50. The issue of a link writes the audit row of rule 61 and sends the webhook event of rule 62.

## Views of the setting

51. `GET /api/flats/{slug}/signin` and the same route of the console API return `{"provider": ..., "issuer": ...,
    "client_id": ..., "allow": [...], "client_secret": {"name": "oidc-client-secret", "updated_at": ...}, "hosts":
    [...], "callback_urls": [...], "seq": N}`. `client_secret` is `null` when no value is stored. `hosts` are the
    current Portal host names of the flat. `callback_urls` are `https://<host>/_portal/auth/callback` for each host name
    when the provider is `oidc`, and empty otherwise. Every caller of the API can read the route.
52. The flat view (`GET /flats`, `GET /flats/{slug}`, MCP `list_flats` and `get_flat`) has the member `sign_in` with
    `provider`, `issuer`, `client_id`, `allow_count` (the number of entries), and `client_secret` with the name
    `oidc-client-secret` when a value is stored, while sign-in is on. It has no member `sign_in` while sign-in is off.
    It holds no entry of the allowlist: an entry names a person, as the webhook keeps out of its bodies, and the
    operator reads the entries with `flats signin show` or the route of rule 51. The descriptions of the MCP tools say
    that `sign_in` protects the Portal URL only.
53. `flats signin show <slug> [--json]` prints the setting, whether a client secret is stored, the current Portal host
    names and the callback URLs to register at the provider. `--json` prints the response of rule 51.
54. No view holds the client secret or an access link.
55. The public notice of a Public flat with sign-in on is: `This flat is public on its Portal address behind sign-in:
    only the visitors that its sign-in allows can open it there. Its private routes, the console and MCP are not behind
    sign-in.` The pending notice of a visibility request for such a flat says the same with `If approved`.
56. The console shows a Public flat with sign-in on with the badge `Public · Sign-in`, in the flat list and on the page
    of the flat, and shows a Public flat without sign-in with `Public`, as today. A Private flat shows `Private`.
57. On the Settings tab, below Sharing, the console has the row Sign-in. It shows the provider (`OpenID Connect` and the
    host of the issuer, or `Access links`), the client ID, the number of entries with the list, whether a client secret
    is stored, the callback URLs, and these texts: `Sign-in protects the Portal address of this flat only. Its private
    routes, previews, the console and MCP are not behind sign-in.` and `Change it with flats signin set on the host.
    Each change waits for your approval here.` A pending `set_signin` approval shows as `Waiting for approval` with View
    request, as for a visibility request.
58. The approval list and the approval page describe a `set_signin` approval: `Turn on sign-in for <slug>`, `Change the
    sign-in of <slug>`, or `Turn off sign-in for <slug>: anyone can open its Portal address`. The page shows the stored
    and the requested setting, the added and the removed entries, and a warning when the change widens access (off, an
    added entry, a change of the issuer or the client ID). While sign-in is on, the Networks section disables the switch
    of each other public provider and shows the reason of rule 8.

## Audit and the webhook

59. `audit_events` gets the kind `signin.set`. The host writes it in the transaction of step 2 of rule 20, with `flat`,
    no `name`, `via` `console`, the actor of the console (empty when the host applies the approval without a console
    request, as `resumeApplying` does), and the data `{"from": {"provider": ..., "issuer": ..., "client_id": ...,
    "count": N}, "to": {"provider": ..., "issuer": ..., "client_id": ..., "count": M, "allow": [...]}, "approval": ID}`.
    When `allow` does not fit into the 4 KiB of the data, the row holds the first entries that fit and `"truncated":
    true`, as `network.set` does.
60. A set of the client secret writes `secret.set` with `name` `oidc-client-secret`, `via` `cli`, and the data
    `{"created": true or false, "use": "signin"}`. A removal writes `secret.delete` with `{"use": "signin"}`. `flats
    secret reset` and the deletion of a flat write `secret.delete` with `"use": "signin"` and the fields that they write
    today.
61. The issue of a link writes `signin.link` with `flat`, no `name`, `via` `cli`, and the data `{"subject": ..., "host":
    ..., "expires_at": ...}`. The row holds no credential and no link.
62. The webhook gets the type `signin.changed`, with `state` `applied` and `console_url` `<console>/flats/<slug>`:

    | `operation` | When | `change` |
    | --- | --- | --- |
    | `set_signin` | an approval applied a sign-in change. The body has `approval_id` and `decided_by`. | `{"from": {"provider": ..., "count": N}, "to": {"provider": ..., "count": M}}` |
    | `issue_signin_link` | an access link was issued | `{"expires_at": ...}` |

    No body holds an entry of the allowlist, a subject, the client secret, a credential or a link. A set or a removal
    of the client secret sends no webhook event, as no secret change does ([event webhook](event-webhook.md), "Scope"):
    the secret decides the access of nobody, and its audit row of rule 60 records it.
63. `approval.created` and `approval.decided` of a `set_signin` approval have the `operation` `set_signin`, as the other
    approvals have their action.

## Compatibility

64. A flat with sign-in off behaves as today, with one difference: rule 42 removes client copies of its seven headers on
    every route that runs flat code. Flat code that read such a header from a client stops seeing it. The docs say so.
65. An older CLI has no `signin` command. An older host answers 404 to the routes of this spec, and the new CLI says
    that the host is older than the command.
66. Schema 29 adds to `flats` the columns `signin_provider`, `signin_issuer` and `signin_client_id` (text, empty by
    default), `signin_allow` (text, `[]` by default) and `signin_seq` (integer, 0 by default). It adds to `secrets` the
    column `use` (text, empty by default, with the check `use IN ('', 'signin')`) and a unique index on `flat` of the
    rows whose use is `signin`. The migration runs in the transaction of the version bump. Every flat gets sign-in off,
    and every secret keeps an empty use. An older release refuses a database of schema 29, as it refuses one of
    schema 28. The tests that name schema 28 as the latest change with it. The docs say to make a backup with `flats
    backup` before the upgrade, and that a return to an older release needs the copy from before the upgrade.
67. A flat that [public instance mode](public-instance.md) made Public on a default network other than Portal holds that
    grant. Rule 8 refuses sign-in on for it until the operator removes that grant.

## Changes to other specs

68. These specs change with the implementation:
    - [audit events](audit-events.md): the table gets the rows `signin.set` and `signin.link`, the row `secret.set` and
      the row `secret.delete` get `"use": "signin"`, rule 3 allows the fields of rules 59 to 61, rule 4 names the
      channels of those rows, and the scope names this spec;
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
    - [public instance mode](public-instance.md): a note on rule 67;
    - `docs/spec/README.md`: the row of this spec.
69. [Windows support](windows-support.md) does not change. The identity file keeps the access list that the SDK gives it
    (its rule 18), and this spec adds no file on disk.

## Docs

70. These docs and texts change:
    - `docs/networking.md`: the Portal row of the provider table, the section Visibility (Public is not access control,
      except on the Portal URL of a flat with sign-in on), and a new section on Portal sign-in. The section says what
      sign-in protects and what it does not (rule 12), how to register the callback URLs, that a relay change or a
      rename changes the host names, that explicit relays keep the host names stable, and how to issue and end access
      links;
    - `docs/design.md`: Exposure (the sentence "Public is never access control"), Secrets (the sign-in secret),
      Approvals (`set_signin`), and Server flats (the headers of rule 41 and rule 42);
    - `docs/surfaces.md`: the routes of rules 13, 24, 45 and 51, the CLI row of `signin`, the flat view, the MCP texts
      and the console screens;
    - `docs/configuration.md`: the backup before the upgrade to schema 29, the audit kinds, and the webhook type;
    - the agent topics of server code and the generated `docs/runtime-api-v1.md`: the headers of rule 41;
    - the texts in code: the MCP instructions (`docs/instructions.go`, the line that says that Public is not access
      control), `PublicAccessNotice` and `PendingPublicAccessNotice`, the MCP descriptions of `get_flat` and
      `list_flats`, the CLI help, and the console texts of rules 56 to 58;
    - the tests that pin these texts change with them.

## Known limits

- Sign-in protects the Portal URL only. A visitor on a private route keeps the rights of a private visitor, also on a
  `proxy` route that the reverse proxy of the operator opens to many people.
- The provider needs one callback URL for each Portal host name of the flat. A relay that discovery replaces, a pin that
  moves, an alias that is lost or comes back, and a rename change the host names. Until the operator registers the new
  URL, nobody can sign in on that host name. Explicit relays keep the host names stable.
- The token secret of the Portal identity, in `portal/<slug>.json`, signs every session and every credential. The host
  key does not seal it, `flats secret rotate-key` does not change it, and `flats backup` copies it. Whoever reads the
  file can make a session or a link for any subject on the allowlist.
- An access link cannot be revoked alone. Its subject is a label: whoever holds the link is that subject until the link
  expires or the subject leaves the allowlist.
- Anyone who can run the CLI on the host can issue a link for a subject on the allowlist without an approval.
- A rename ends every session and every access link of the flat.
- A machine client, such as a service that sends webhooks to the flat, cannot sign in. Only a credential in
  `X-Portal-Access-Credential` lets it in.
- The gate answers some requests itself (sign-in pages, refusals). Analytics does not count them.
- The flat cannot serve a path under `/_portal/auth/` on its Portal URL while sign-in is on.
- `appauth.Check` runs only after a build. A provider that fails later shows only to the visitor who signs in.
- The SDK fetches the issuer from the host with its own HTTP client. The origin grants of the flat do not apply, and an
  issuer on a private network is reachable.
- The headers of rule 41 reach server flats only. A static flat cannot read them. A guest name on a board or a document
  is still the name that the visitor types.
- A request that the gate passed before an allowlist change finishes after the change.
- The console has no form that turns sign-in off. When the identity provider fails, an operator away from the host can
  make the flat Private or revoke Portal for it in the console, but cannot open it without sign-in.

## Completion criteria

Test seams. The Portal provider has two seams that tests in package `portal` set: the function that makes an exposure
and the function that runs HTTP (the fields `expose` and `runHTTP` of `Net`). The check of rule 2 that refuses an `http`
issuer is in package `core`, so a test in package `portal` can use an OIDC provider on a loopback `http` server, which
the SDK accepts. A check failure uses an `https` issuer on a closed loopback port. A build failure uses a setting that a
test wrote into `flats.db`, a client secret sealed by another key, and an identity file without a token secret in a
folder that the test cannot write. A test that needs a folder that it cannot write runs on Unix as a user that is not
the superuser, and skips elsewhere. No test needs the network.

- The setting: the migration from schema 28 gives every flat sign-in off and every secret an empty use; a reader of
  schema 28 refuses schema 29; each limit and each normalization of rules 2 to 4, with the order and the duplicates; an
  `http` issuer on a loopback host is refused; a rename keeps the setting and the deletion removes it.
- The request and the approval: each 400 of rule 14 with its message; a remote caller gets 403 from the routes of
  rules 13, 24 and 45; MCP has no tool that requests a change, sets the client secret or issues a link; a request for
  the stored setting is `done` and makes no approval; a pending approval changes nothing on the Portal route; an equal
  request returns the open approval; the frozen parameters hold no client secret; each stale case of rule 18 fails and
  names what changed, and a redeploy, a rollback, a visibility change and a client secret change leave the approval
  valid; a change and its reverse make an earlier request stale (rule 19); `AutopilotNeeds("set_signin", ...)` is
  unclassified, and an Autopilot decision is refused; a console decision applies the setting; the result and the flat
  event hold no entry.
- Portal only: each refusal of rule 8, with 409 and its message; a grant and a sign-in apply that run at the same time,
  many times, give the result of one order; a `flats.db` that a test wrote with sign-in on and a `tailscale-funnel`
  grant makes `ensureLifecycle` ask no Funnel route, and a request marked with `tailscale-funnel` gets the 403 of
  rule 40.
- The gate build failure: each case of rule 33 opens no Portal route for a new entry (the fake exposure is never called
  and the fake HTTP run never gets the handler of the flat), stops the route of a served entry, shows the endpoint state
  `error` with the detail, writes one log line, and keeps the other routes; no case serves the flat without the gate;
  the next build after the cause is removed opens the route.
- The gate: a Portal route of a flat with sign-in on sends a request without a session to the sign-in page; a second
  `Serve` with a new handler keeps the gate and sends a signed-in request to the new handler; a removed entry loses
  access at the next request after the change; a request that the old gate let in gets the 503 of rule 40 until the new
  gate is in place; sign-in off serves the handler box without a gate; an identity file without a token secret gets one
  and keeps its address (rule 34); a failed check writes the warning and the detail, and the gate still refuses a
  visitor without a session; `PublicHosts` gives the canonical host names with a zero `Since` and the alias host names
  with their `Since`, a session from before the `Since` of an alias is refused, and a request for another host name gets
  421; a reopen of the route without a setting keeps the gate; an open with a setting of a lower sequence keeps the gate
  of the higher one.
- A visitor outside the allowlist: with no session, with a session of a removed entry, and with a credential of a
  subject that is not on the list, the visitor cannot open a page, a static file or an asset of a document on the Portal
  route. A preview of the flat has no Portal route with sign-in on and Portal granted, and its private route serves no
  headers of rule 41.
- Survival: the setting and the gate survive a redeploy, a rollback and a host restart (a new `Net` on the same folder
  accepts a session of the canonical host name from before the restart); a rename keeps the setting, builds a gate with
  the new identity, refuses the sessions and links of the old one, and the old slug answers only with a redirect.
- Each request: the headers of rule 41 for `oidc` with an email entry, with a `sub:` entry, and for `credential`, and
  none without a sign-in; client copies of each header of rule 42, in mixed case, never reach flat code on the public
  route, the private route, a preview and the host operations of the docs worker; the docs worker sees `public` for a
  signed-in visitor; a cross-origin WebSocket upgrade with a session is refused.
- The client secret: a set and a removal through the CLI header on loopback, 403 for a remote caller; each 400 of
  rule 25; a test stores a distinctive value and shows that no `env`, worker spec, log line, flat event, audit row,
  webhook body, export line, approval, API, MCP, CLI or console output and no status holds it, and that `flats secret
  ls`, `list_secrets` and the console list of secrets do not list it; rule 29 refuses `flats secret rm` of it;
  `rotate-key` keeps its use and opens it; `restore-key` counts it; `reset` names and removes it; a set while sign-in is
  `oidc` rebuilds the gate.
- Access links: each refusal of rule 46; the link has the form of `CredentialRedeemURL` for the canonical host name, and
  for `--host`; the default and the limits of the expiry; the link signs a visitor in on its host name and not on
  another host name of the flat; a link of a subject that left the allowlist is refused; the credential in
  `X-Portal-Access-Credential` lets a request in; the link is in no log line, flat event, audit row, webhook body or
  other output.
- The CLI: each form of the synopsis, each usage error with exit status 2, and the help text.
- The views: the route of rule 51 with its members, the name of the client secret and no value; `sign_in` in the flat
  view while sign-in is on and absent while it is off; `flats signin show` with and without `--json`; the notices of
  rule 55.
- Audit and the webhook: each row of rules 59 to 61 with its `via` and its data, in the transaction of its change (a
  failing audit write leaves the change undone); an allowlist that does not fit has `truncated`; each operation of
  rule 62 sends one `signin.changed` with the fields of its row and no entry, subject, secret, credential or link.
- The console: the UI test shows `Public · Sign-in` for a Public flat with sign-in on and `Public` for one without it,
  the Sign-in row with the texts of rule 57, the description and the warning of rule 58, and the disabled switches of
  the other public providers; no console text holds the client secret.
- The docs: `docs/networking.md`, `docs/design.md` and the MCP instructions say that sign-in protects the Portal URL and
  not the private routes, the console or MCP; the doc checks pass, and the generated references match their sources.

## Open questions

None.
