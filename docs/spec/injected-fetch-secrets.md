# Injected fetch secrets

Status: planned.

## Summary

A JavaScript server flat reads each of its secrets as `env.NAME` and builds its own `Authorization` header from it. So
flat code holds the value, and a handler can return it in a response or write it to a log. This spec adds an injected
secret: a flat secret that is bound to one exact HTTPS origin and one header name. The host adds that header to each
outgoing `fetch` of the flat to that origin. Flat code never gets the value. It is not in `env`, and no host call
returns it. A readable secret (every secret of today) keeps working as before.

The request is item 3 of issue #224.

## Before this change

- A flat secret is a name and a sealed value in the `secrets` table of `flats.db` (`internal/store/store.go`). Only the
  console and the `flats` CLI on loopback set or remove one (`Service.SetSecret`, `Service.DeleteSecret`,
  `operatorVia` in `internal/api/api.go`). `flats secret set <slug> <NAME>` reads the value from a terminal without
  echo or from a pipe (`readSecret` in `internal/cli/commands.go`). MCP `list_secrets` and `GET
  /flats/{slug}/secrets` return names and update times only.
- An activation captures the ordinary variables, the secrets and the origin grants of a flat in one statement
  (`Store.RuntimeSettingsSnapshot`, `Service.captureEnvironment` in `internal/core/env.go`). The opened secrets and
  the ordinary variables become one map, `env`, and the secret values also make the redactor of the worker's log lines
  and start errors (`newRedactor` in `internal/core/redact.go`).
- The worker process gets that map in its spec on stdin (`workerSpec.Env`, `internal/runtime/protocol.go`). The host
  call `env` gives the whole map to JavaScript (`jsVM.host` in `internal/runtime/js.go`), and a WASI command gets it in
  its environment. So the code of a flat can read every secret of the flat.
- `fetch` of a JavaScript server flat is a host call. The worker process sends it through `egress.Client`
  (`internal/egress/client.go`), which allows only the exact origins of the captured grants, refuses non-public
  addresses, pins the connection to the checked addresses, never follows a redirect, and refuses the header names of
  `forbiddenHeader` in a request of flat code. The embedded docs app gets no origin grants
  (`Service.buildWithEnvironment` in `internal/core/service.go`). WASI has no network.
- The audit log has the kinds `secret.set` with the data `{"created": true or false}` and `secret.delete`
  ([audit events](audit-events.md), rules 8 and 9). A secret change sends no webhook event
  ([event webhook](event-webhook.md), "Scope").

## Scope

In scope: the binding of a secret, the API route and the CLI that set it, the checks, the capture, the worker spec, the
added header, what flat code sees, the listing in the API, the CLI, MCP and the console, the redactor, the audit data,
the schema, the docs and the agent guide.

Out of scope:

- More than one origin or more than one header for one secret. An operator who needs the same credential at two
  origins sets two secrets.
- A template for the header value. The secret value is the whole header value (decision 2).
- A console form that makes an injected secret (decision 3). The console shows the binding and replaces a value.
- Injection into anything other than `fetch` of a JavaScript server flat: browser code, WebSocket, WASI, the docs,
  board and notebook apps.
- A change of what a readable secret does.
- Protection against an origin that copies request headers into its response (rule 15).

## Decisions

The owner of the repository decided four points on 2026-11-18:

1. The binding is fixed when the secret is set. `flats secret set <slug> <NAME> --origin URL --header NAME` makes an
   injected secret. A new value keeps the binding. A change of the origin or the header needs a removal of the secret
   and a new set with its value, so nobody sends a stored credential to a new origin without knowing it.
2. The secret value is the whole header value, for example `Bearer abc123`.
3. The CLI and the API make an injected secret. The console lists the binding of each secret and replaces a value, and
   it gets no form that makes an injected secret.
4. A model other than the author reviews each stage of this change.

The author of this spec decided these points, and each can change before the implementation:

- Only an `https://` origin takes an injected secret (rule 4). An `http://` request sends the header unencrypted.
- The binding does not grant network access (rule 8). The origin must also be one of the flat's origin grants, which
  `flats network set` changes and which have their own audit kind and webhook event.
- A `fetch` that supplies the bound header to the bound origin fails (rule 13). The host does not overwrite a header of
  flat code without a word, and the error shows the author of the flat where the value comes from.
- A secret change, also of an injected secret, sends no webhook event (rule 26), as today.

## Injected secrets

1. An injected secret is a flat secret with a binding: one origin and one header name. Every other flat secret is a
   readable secret. A secret has its binding from its first set until its removal (decision 1).
2. A set of a secret behaves by the stored state of the name and by whether the set names a binding:

   | Stored | The set names no binding | The set names a binding |
   | --- | --- | --- |
   | nothing | a readable secret | an injected secret with that binding |
   | a readable secret | the value is replaced | refused (rule 7) |
   | an injected secret | the value is replaced, the binding stays | the same binding: the value is replaced; another binding: refused (rule 7) |

3. The origin and the header name are not secret. The API, the CLI, MCP, the console, the audit log and the error
   messages show them. The value is shown nowhere.
4. The origin is one exact origin in the form of a network grant (`egress.NormalizeOrigins`): the scheme `https`, a
   host, the port 443 or no port, and no path other than `/`, no query, no fragment and no user information. It is
   stored in its normalized form, for example `https://api.example.com`. The host refuses an `http://` origin.
5. The header name is a valid HTTP field name of at most 128 bytes, and not a name that flat code cannot supply
   (`forbiddenHeader`: `Host`, `Connection`, `Content-Length`, `Transfer-Encoding`, `Upgrade`, `Trailer`, `TE`,
   `Keep-Alive`, `Expect`, `Accept-Encoding`, and every name that starts with `Proxy-` or `Sec-`). It is stored in its
   canonical form (`http.CanonicalHeaderKey`), for example `X-Api-Key`.
6. The value of an injected secret meets the rules of every secret value (valid UTF-8, no NUL, at most 64 KiB), and
   also: it is not empty, it has at most 8 KiB, and it is a valid HTTP field value (no control character other than a
   tab). The CLI removes one trailing newline from piped input, as today.
7. The host refuses these sets and changes nothing:
   - a binding for a name that holds a readable secret. Flat code could read the old value, so the message names
     `flats secret rm <slug> <NAME>`, a new set with `--origin` and `--header`, and a new credential from the service;
   - a binding that differs from the stored binding of an injected secret. The message names the stored origin and
     header and the removal and new set of the secret;
   - a binding whose origin and header (the header compared without case) another injected secret of the same flat has.
     The message names that secret;
   - a new injected secret of a flat that has 32 injected secrets;
   - a binding with an origin, a header or a value that breaks rules 4 to 6. The message says which of the three
     failed and why, and never holds the value.
8. A binding grants no network access. The host adds the header only to a request that the captured origin grants of
   the flat permit (rule 12). A set of an injected secret does not check the current grants, and its output says that
   `fetch` reaches the origin only when `flats network set` permits it.
9. Only the operator sets or removes a secret, as today: the console, and the CLI on loopback. MCP has no tool that
   sets a secret, and a remote caller of the route gets 403, for an injected secret too.
10. An ordinary variable cannot have the name of a secret, injected or readable, and a secret cannot have the name of
    an ordinary variable, as today.

## What flat code sees

11. An injected secret is not in the environment of the flat. In JavaScript, `env.NAME` is `undefined` and
    `Object.keys(env)` does not hold the name. A WASI command does not get the name or the value in its environment.
    No host call returns the value or the binding.
12. The host adds the header to a `fetch` call of a JavaScript server flat when the normalized origin of the request
    URL is the origin of the binding, and the captured origin grants permit that origin. The comparison is exact after
    the normalization of `egress`: `https://API.example.com/v1` and `https://api.example.com:443/v1` match
    `https://api.example.com`, and `http://api.example.com`, `https://eu.api.example.com` and
    `https://api.example.com.evil.test` do not. A request to any other origin gets no injected header. A request that
    the grants refuse fails as today, before any connection.
13. When the request of flat code supplies a header with the bound name (compared without case) to the bound origin,
    the call fails before any connection. The error says that the host sets that header for this origin and names
    the header. It never holds the value. Flat code may supply that header to other origins.
14. The added header does not count toward the limits of the headers that flat code supplies (16 KiB and 128 fields).
    The host never follows a redirect (as today), so the header goes only to the connection of that one call. With
    `redirect: "manual"` the redirect response is returned and nothing else is sent.
15. The host does not change the response. An origin that copies a request header into its response body or its
    response headers gives the value to flat code. Error messages and the `url` of the response never hold the value.
    The docs tell the operator to bind a secret only to an origin of an API that does not return
    request headers.
16. The embedded docs app gets no injected secret, as it gets no origin grants. Boards and notebooks run no flat code.

## Capture

17. An activation captures the injected secrets with the ordinary variables, the other secrets and the origin grants,
    in the same statement (`Store.RuntimeSettingsSnapshot`). The capture times stay as they are: approved deploy,
    redeploy, rollback, data restore, host start and a new preview capture the current settings. The health check and
    the live worker use the same capture. An automatic worker replacement keeps it. A set or a removal does not change a
    running worker: redeploy to apply it.
18. The capture checks each stored binding and value again against rules 4 to 6. A stored binding that fails refuses
    the activation with a message that names the secret and `flats secret rm` with a new set. It never names the
    value.
19. The worker gets the injected secrets in its spec on stdin, in a field apart from `env`, as a list of origin,
    header and value. They are never in argv or in the environment of the process. A worker of a WASI flat ignores the
    field.
20. The values of the injected secrets join the redactor of the flat, with the encodings that it uses for the other
    secret values. A log line or a start error of the worker never holds one.

## Listing

21. `GET /flats/{slug}/secrets` gives each injected secret the object `"inject": {"origin": ..., "header": ...}`. A
    readable secret has no `inject` member. MCP `list_secrets` returns the same, and its description says that an
    injected secret is not in `env` and that the host adds its header to `fetch` calls to its origin.
22. `flats secret ls <slug>` prints the columns `NAME`, `UPDATED` and `INJECTED`. `INJECTED` is `<header> to <origin>`
    for an injected secret and `-` for a readable one. `--json` prints the response of the route.
23. The console lists the binding of each injected secret under its name, with the text `Sent as <header> to <origin>.
    Not in env.` **Replace** sends only the value, so the binding stays. A save of the add form with the name of an
    injected secret also keeps the binding.

## The API and the CLI

24. `PUT /flats/{slug}/secrets/{name}` takes `{"value": ..., "inject": {"origin": ..., "header": ...}}`. Without
    `inject` it behaves as today, and rule 2 applies. An `inject` object without both members, or with a member that is
    not a string, is 400. A refusal of rule 7 for a stored state (the first three items) is 409; a refusal for a limit
    or for rules 4 to 6 is 400. The response of a set of an injected secret has the stored `inject` object.
25. `flats secret set <slug> <NAME> [--origin URL --header NAME]` reads the value as today. `--origin` and `--header`
    go together, and one without the other is a usage error (exit status 2). The output of a set of an injected secret
    says that the host sends the value as that header of each `fetch` to that origin, that flat code cannot read it,
    that `fetch` reaches the origin only when `flats network set` permits it, and when the change applies. The help text
    names the flags.

## Audit and the webhook

26. `secret.set` gets the members `"origin"` and `"header"` in its data for each set of an injected secret, the first
    and each later one: `{"created": true, "origin": "https://api.example.com", "header": "Authorization"}`. The data
    of a readable secret stays `{"created": ...}`. `secret.delete` stays as it is. A secret change sends no webhook
    event, as today ([event webhook](event-webhook.md), "Scope"). The reach of an origin is a network grant,
    whose change writes `network.set` and sends `network.changed`.

## The other commands

27. `flats secret rotate-key` seals an injected value again like any other value and leaves its binding as it is.
    `flats backup` copies the bindings with `flats.db`. `flats secret restore-key` checks injected values like the other
    values. `flats secret reset` removes injected secrets with the others, and its list names them. A rename of a flat
    keeps the bindings. The deletion of a flat removes its injected secrets with `secret.delete` events, as today.

## Compatibility

28. Schema 28 adds the columns `fetch_origin` and `fetch_header` (text, empty by default) to `secrets`. Every existing
    secret becomes a readable secret, with no change of behavior. An older release refuses a database of schema 28, as
    it refuses one of schema 27. The docs say to make a backup with `flats backup` before the upgrade, and that a return
    to an older release needs the copy from before the upgrade.
29. A readable secret, its API, its CLI and its runtime behavior do not change. A client from before this change sends
    no `inject` and gets readable secrets. Its `secret ls` does not show the binding of an injected secret.

## Docs

30. These docs change:
    - `docs/external-api.md`: "Server setup" shows an injected secret, and "JavaScript fetch subset" states rules 12 to
      15;
    - `docs/agent/topics/env-secrets.md`, `docs/agent/topics/server.fetch.md` and the generated
      `docs/runtime-api-v1.md`: an injected secret is not in `env`, flat code calls `fetch` without that header, and the
      call fails when it supplies it;
    - `docs/surfaces.md`: the route body, the CLI row of `secret`, and `list_secrets`;
    - `docs/design.md`, "Secrets": the two kinds of secret;
    - `README.md`: the sentence "A flat can read and return its own secrets" names the exception;
    - `docs/configuration.md`: the backup before the upgrade to schema 28;
    - `docs/spec/README.md`: the row of this spec.

## Known limits

- An origin that copies request headers into its response gives the value to flat code (rule 15). The host cannot
  tell such an origin from another.
- Flat code chooses the path, the method and the body of a request to the bound origin. A credential that the service
  allows to do more than the flat needs can do all of it from flat code.
- A preview runs code that no approval checked, with the injected secrets of the flat, as it runs today with the
  readable ones.
- A secret that was readable before it was removed and set again as an injected secret may still be in data that the
  flat stored. Rule 7 says to get a new credential.
- The binding is not part of the associated data of the seal. A writer of `flats.db` can change it, as such a writer
  can change the code of a version.

## Changes to other specs

- [Audit events](audit-events.md): rule 8 and the row of `secret.set` in the table of kinds get the members `origin`
  and `header` of rule 26.
- [Secret key rotation](secret-key-rotation.md): none. The re-seal changes the nonce, the ciphertext and the key ID of a
  row and no other column.

## Completion criteria

- The egress client: a request to the bound origin carries the header with the value; a request to another permitted
  origin, to `http://` on the same host, to a subdomain and to a longer host name does not; a request of flat code
  that supplies the bound header (in each case form) to the bound origin fails before the dial, and its error names
  the header and does not hold the value; the same header to another origin is sent; a redirect response with
  `redirect: "manual"` is returned and no second request is made; the added header passes when the headers of flat
  code are at their limit; no error text holds the value.
- The runtime: a JavaScript flat with an injected secret and a readable secret reads `env.READABLE`, gets `undefined`
  for `env.INJECTED`, and `JSON.stringify(env)` does not hold the injected value; the same flat calls `fetch` to the
  bound origin and to another origin through an `egress.Client` whose resolver and dialer lead to local test servers,
  and only the bound server gets the header; a WASI command does not get the name or the value; the worker spec holds
  the injected secret outside `env`.
- The service: each refusal of rules 4 to 7 and 9, with its message and its status class; a new value keeps the binding
  through the API with and without `inject`; the capture puts an injected secret into the injected list and not into
  `env`; a stored binding that breaks rules 4 to 6 refuses the activation and names the secret; the redactor removes an
  injected value from a log line; the docs app gets no injected secret.
- The store: the migration from schema 27 keeps every secret readable and its value openable; a set and a list keep the
  binding; `secret.set` has the data of rule 26; the re-seal of `rotate-key` keeps the binding.
- The API: `PUT` with `inject` returns the stored binding; `GET` returns `inject` for an injected secret only; 400 and
  409 as rule 24 says; 403 for a remote caller.
- The CLI: `--origin` without `--header` and the reverse are usage errors; `secret ls` prints `INJECTED`; the output of
  a set names the origin and the header.
- MCP: `list_secrets` returns `inject` for an injected secret.
- The console: the UI test shows the text of rule 23 for an injected secret and none for a readable one, and
  **Replace** sends only the value.
- The doc checks pass, and the generated references match their sources.

## Open questions

None.
