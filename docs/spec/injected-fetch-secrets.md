# Injected fetch secrets

Status: implemented.

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
- `fetch` of a JavaScript server flat is a host call, in a request callback and in a WebSocket callback. The worker
  process sends it through `egress.Client` (`internal/egress/client.go`), which allows only the exact origins of the
  captured grants, refuses non-public addresses, pins the connection to the checked addresses, trusts the system root
  certificates, never follows a redirect, and refuses the header names of `forbiddenHeader` in a request of flat code.
  The embedded docs app gets no origin grants (`Service.buildWithEnvironment` in `internal/core/service.go`). WASI has
  no network.
- The audit log has the kinds `secret.set` with the data `{"created": true or false}` and `secret.delete`
  ([audit events](audit-events.md), rules 8 and 9). A secret change sends no webhook event
  ([event webhook](event-webhook.md), "Scope").

## Scope

In scope: the binding of a secret, the API route and the CLI that set it, the checks, the capture, the worker spec, the
added header, what flat code sees, the listing in the API, the CLI, MCP and the console, the redactor, the audit data,
the schema, the docs, the example and the agent guide.

Out of scope:

- More than one origin or more than one header for one secret. An operator who needs the same credential at two
  origins sets two secrets.
- A template for the header value. The secret value is the whole header value (decision 2).
- A console form that makes an injected secret (decision 3). The console shows the binding and replaces a value.
- Injection into anything other than `fetch` of a JavaScript server flat: browser code, WASI, the docs, board and
  notebook apps. Flats has no client WebSocket API. A `fetch` call in a WebSocket callback is a `fetch` call, and rule
  14 applies to it.
- A change of what a readable secret does.
- Protection against an origin that copies request headers into its response (rule 17).

## Decisions

The owner of the repository decided four points on 2026-11-18:

1. The binding is fixed when the secret is set. `flats secret set <slug> <NAME> --origin URL --header NAME` makes an
   injected secret. A new value keeps the binding. A change of the origin or the header needs a removal of the secret
   and a new set with its value, so nobody sends a stored credential to a new origin without knowing it.
2. The secret value is the whole header value, for example `Bearer abc123`.
3. The CLI and the API make an injected secret. The console lists the binding of each secret and replaces a value, and
   it gets no form that makes an injected secret.
4. A model other than the author reviews each stage of this change.

The issue calls the binding a grant. This spec reads it as a grant of one credential to one origin, and not as a network
grant. So it gives no network access (rule 10), and its change sends no webhook event (rule 30).

The author of this spec decided these points, and each can change before the implementation:

- Only an `https://` origin takes an injected secret (rule 5). An `http://` request sends the header unencrypted.
- The binding does not grant network access (rule 10). The origin must also be one of the flat's origin grants, which
  `flats network set` changes and which have their own audit kind and webhook event.
- A `fetch` that supplies the bound header to the bound origin fails (rule 15). The host does not replace a header that
  flat code supplies, and the error tells the author of the flat that the host sets that header.
- A value that starts or ends with a space or a tab is refused (rule 7), because the Go HTTP client removes those
  characters when it writes a header, and the sent bytes must be the stored bytes.
- `Content-Type` cannot be bound (rule 6), because the `fetch` of the runtime sets it for a string body.
- A secret change, also of an injected secret, sends no webhook event (rule 30), as today.
- The binding is not part of the associated data of the seal (see "Known limits").
- The example `examples/external-api` changes to an injected secret (rule 37).

## Injected secrets

1. An injected secret is a flat secret with a binding: one origin and one header name. Every other flat secret is a
   readable secret. A secret has its binding from its first set until its removal (decision 1). In `flats.db` a row of
   `secrets` is an injected secret when its column `fetch_origin` or its column `fetch_header` is not empty.
2. The result of a set depends on what is stored under the name, and on whether the set names a binding:

   | Stored under the name | The set names no binding | The set names a binding |
   | --- | --- | --- |
   | Nothing | The host stores a readable secret. | The host stores an injected secret with that binding. |
   | A readable secret | The host replaces the value. | The host refuses the set (rule 8). |
   | An injected secret | The host replaces the value and keeps the binding. | With the same binding, the host replaces the value. With another binding, it refuses the set (rule 8). |

3. Two bindings are the same when their normalized origins (rule 5) are equal and their canonical header names (rule 6)
   are equal.
4. The origin and the header name are not secret. The API, the CLI, MCP, the console, the audit log and the error
   messages show them. No text of the host shows the value (rule 23).
5. The origin is one exact origin in the form of a network grant (`egress.NormalizeOrigins`): the scheme `https`, a
   host, the port 443 or no port, and no path other than `/`, no query, no fragment and no user information. It is
   stored in its normalized form, for example `https://api.example.com`. The host refuses an `http://` origin.
6. The header name is a valid HTTP field name of at most 128 bytes. It is not `Content-Type`, and not a name that flat
   code cannot supply (`forbiddenHeader`: `Host`, `Connection`, `Content-Length`, `Transfer-Encoding`, `Upgrade`,
   `Trailer`, `TE`, `Keep-Alive`, `Expect`, `Accept-Encoding`, and every name that starts with `Proxy-` or `Sec-`). It
   is stored in its canonical form (`http.CanonicalHeaderKey`), for example `X-Api-Key`.
7. Each set of an injected secret checks the value, also a set that names no binding (the console **Replace**, a pipe
   to `flats secret set` without flags). The value meets the rules of every secret value (valid UTF-8, no NUL, at most
   64 KiB). It is also not empty, it has at most 8 KiB, it is a valid HTTP field value (no control character other than
   a tab), and it does not start or end with a space or a tab. The CLI removes one trailing newline from piped input,
   as today, so a second newline makes the value fail. A set that fails is 400, and the stored value and binding stay.
8. The host refuses these sets and changes nothing:
   - a binding for a name that holds a readable secret (409). Flat code could read the old value, so the message names
     `flats secret rm <slug> <NAME>`, a new set with `--origin` and `--header`, and a new credential from the service;
   - a binding that is not the same (rule 3) as the stored binding of an injected secret (409). The message names the
     stored origin and header, and the removal and a new set of the secret;
   - a binding that is the same as the binding of another injected secret of the same flat (409). The message names
     that secret, its removal with `flats secret rm`, and `flats secret set` without flags for a new value of it;
   - a new injected secret of a flat that has 32 injected secrets (400). The message names `flats secret ls` and
     `flats secret rm`;
   - an origin, a header name or a value that breaks rules 5 to 7 (400). The message says which of the three failed
     and why. The message for an origin does not repeat the text that the caller gave, because it can hold user
     information or a query.
9. The store checks what is stored under the name, the binding of the other injected secrets of the flat and the count
   of 32, and it writes the row, in one write transaction. Before the seal, the service checks the value against rule 7
   and gives the result to the store. The transaction refuses the set when the value fails rule 7 and the row that it
   reads, or the set, is injected. So two sets that run at the same time give the result of one of the two orders. A unique index on the flat, the origin and the header of the injected rows also refuses a second
   row with the same binding.
10. A binding grants no network access. The host adds the header only to a request that the captured origin grants of
    the flat permit (rule 14). A set of an injected secret does not check the current grants, and its output says that
    `fetch` reaches the origin only when `flats network set` permits it.
11. Only the operator sets or removes a secret, as today: the console, and the CLI on loopback. MCP has no tool that
    sets a secret, and a remote caller of the route gets 403, for an injected secret too.
12. An ordinary variable cannot have the name of a secret, injected or readable, and a secret cannot have the name of
    an ordinary variable, as today.

## What flat code sees

13. An injected secret is not in the environment of the flat. In JavaScript, `env.NAME` is `undefined` and
    `Object.keys(env)` does not hold the name. A WASI command does not get the name or the value. No host call returns
    the value. Flat code can learn the binding (the error of rule 15 names the header), and agents read it through MCP
    (rule 24), because it is not secret (rule 4).
14. The host adds the header to a `fetch` call of a JavaScript server flat, in a request callback or a WebSocket
    callback, when the normalized origin of the request URL is the origin of the binding and the captured origin
    grants permit that origin. The comparison is exact after the normalization of `egress`. `https://API.example.com/v1`
    and `https://api.example.com:443/v1` match `https://api.example.com`. `http://api.example.com`,
    `https://eu.api.example.com` and `https://api.example.com.evil.test` do not match. A request to any other origin
    gets no injected header. A request that the grants refuse fails as today, before any connection.
15. When the request of flat code supplies a header with the bound name (compared without case) to the bound origin,
    the call fails. It fails before the rate limit, the concurrency slots, the address lookup and the connection. The
    error says that the host sets that header for this origin and names the header. It never holds the value. Flat
    code may supply that header to other origins.
16. The added header does not count toward the limits of the headers that flat code supplies (16 KiB and 128 fields).
    The host never follows a redirect (as today), so the header goes only to the connection of that one call. With
    `redirect: "manual"` the redirect response is returned and nothing else is sent.
17. The host does not change the response. An origin that copies a request header into its response body or its
    response headers gives the value to flat code. Error messages and the `url` of the response never hold the value.
    The docs tell the operator to bind a secret only to an origin of an API that does not return request headers.
18. The embedded docs app gets no injected secret, as it gets no origin grants. Boards and notebooks run no flat code.

## Capture

19. An activation captures the injected secrets with the ordinary variables, the other secrets and the origin grants,
    in the same statement (`Store.RuntimeSettingsSnapshot`). The capture times stay as they are. An approved deploy, a
    redeploy, a rollback, a data restore, a host start and a new preview capture the current settings. The health check
    and the live worker use the same capture. An automatic worker replacement and a rename of the flat keep it. A set or
    a removal does not change a running worker. Redeploy to apply it.
20. The capture checks each injected row again, and it refuses the activation when a row has only one of the two
    binding columns, breaks rules 5 to 7, has the same binding as another row, or when the flat has more than 32
    injected rows. In each of these cases no value of the row goes into `env`. The message names the secret, and
    `flats secret rm` with a new set. For more than 32 rows it names the flat, `flats secret ls` and `flats secret rm`.
21. The worker of a JavaScript flat gets the injected secrets in its spec on stdin, in a field apart from `env`, as a
    list of origin, header and value. They are never in argv or in the environment of the process. The host sends no
    list to the worker of a WASI flat. The worker refuses to start when the list breaks rules 5 to 7.
22. The values of the injected secrets join the redactor of the flat, with the encodings that it uses for the other
    secret values. A log line or a start error of the worker never holds one.
23. No text of the host holds an injected value. The messages of the refusals of rules 7, 8 and 20, the flat events,
    the result of an approval, the audit rows, the export and the outputs of the API, MCP and the CLI are made from the
    name, the origin and the header only.

## Listing

24. `GET /flats/{slug}/secrets` gives each injected secret the object `"inject": {"origin": ..., "header": ...}`. A
    readable secret has no `inject` member. MCP `list_secrets` returns the same. Its description and its note say that
    an injected secret is not in `env`, and that the host adds its header to `fetch` calls to its origin.
25. `flats secret ls <slug>` prints the columns `NAME`, `UPDATED` and `INJECTED`. `INJECTED` is `<header> to <origin>`
    for an injected secret and `-` for a readable one. `--json` prints the response of the route.
26. The console lists the binding of each injected secret under its name, with the text `Sent as <header> to <origin>.
    Not in env.` **Replace** sends only the value, so the binding stays, and a value that breaks rule 7 shows the
    refusal and changes nothing. A save of the add form with the name of an injected secret also keeps the binding. The
    delete dialog of an injected secret says that a secret added again in the console is readable, and that
    `flats secret set` with `--origin` and `--header` makes it injected again.

## The API and the CLI

27. `PUT /flats/{slug}/secrets/{name}` takes `{"value": ..., "inject": {"origin": ..., "header": ...}}`, on the API
    route and on the console route. Without `inject`, or with `"inject": null`, the set names no binding (rule 2). An
    `inject` object without both members, or with a member that is not a string, is 400. The statuses of the refusals
    are in rules 7 and 8. The response of a set of an injected secret has the stored `inject` object.
28. `flats secret set <slug> <NAME> [--origin URL --header NAME]` reads the value as today. `--origin` and `--header`
    go together, and one without the other is a usage error (exit status 2). The output of a set of an injected secret
    says that the host sends the value as that header of each `fetch` to that origin, that flat code cannot read it,
    that `fetch` reaches the origin only when `flats network set` permits it, and when the change applies. The help text
    names the flags.

## Audit and the webhook

29. `secret.set` gets the members `"origin"` and `"header"` in its data for each set of an injected secret, the first
    and each later one: `{"created": true, "origin": "https://api.example.com", "header": "Authorization"}`. The data
    of a readable secret stays `{"created": ...}`. `secret.delete` stays as it is.
30. A secret change sends no webhook event, as today ([event webhook](event-webhook.md), "Scope"). The reach of an
    origin is a network grant, whose change writes `network.set` and sends `network.changed`.

## The other commands

31. `flats secret rotate-key` seals an injected value again like any other value and leaves its binding as it is.
    `flats backup` copies the bindings with `flats.db`. `flats secret restore-key` checks injected values like the other
    values. `flats secret reset` removes injected secrets with the others, and its list names them. A rename of a flat
    keeps the bindings. The deletion of a flat removes its injected secrets with `secret.delete` events, as today.

## Compatibility

32. Schema 28 adds the columns `fetch_origin` and `fetch_header` (text, empty by default) to `secrets`, and the unique
    index of rule 9. Every existing secret becomes a readable secret, with no change of behavior. An older release
    refuses a database of schema 28, as it refuses one of schema 27. The tests that name schema 27 as the latest
    version change with it. The docs say to make a backup with `flats backup` before the upgrade, and that a return to
    an older release needs the copy from before the upgrade.
33. A readable secret, its API, its CLI and its runtime behavior do not change. A client from before this change sends
    no `inject`. Its set of the name of an injected secret replaces the value and keeps the binding (rule 2), and its
    `secret ls` shows the name only. An older host refuses a set with `inject` with 400 (an unknown field), and stores
    nothing.

## Docs

34. These docs change:
    - `docs/external-api.md`: "Server setup" shows an injected secret, the order to move a readable secret to an
      injected one (deploy code that does not send the header, then remove the secret and set it again with a new
      credential), and that a Basic credential is entered in its encoded form. "JavaScript fetch subset" states rules 14
      to 17;
    - `docs/agent/topics/env-secrets.md` and `docs/agent/topics/server.fetch.md`, and through them the generated
      `docs/runtime-api-v1.md`: an injected secret is not in `env`, flat code calls `fetch` without that header, and the
      call fails when it supplies it. The topic stops using "injected" for "put into `env`";
    - `docs/agent/types/flats-runtime-v1.d.ts`: the comment of `env`;
    - `docs/surfaces.md`: the route body, the CLI row of `secret`, and `list_secrets`;
    - `docs/design.md`: "Secrets", and in "Server flats (handler ABI)" the text of `env`, the WASI environment and the
      row of the capability table for secrets;
    - `README.md`: the sentence "A flat can read and return its own secrets", and the two paragraphs on what reaches
      server code as `env.NAME` and as the WASI environment;
    - `docs/configuration.md`: the backup before the upgrade to schema 28;
    - `docs/spec/README.md`: the row of this spec;
    - the texts in code: the description and the note of MCP `list_secrets` (`internal/mcpx/tools.go`), the CLI help,
      and the console texts of the secrets section, the network section and the section of the ordinary variables
      (`internal/console/static/assets/js/flat.js`), which say that browser code gets no injected values;
    - the tests that pin these texts change with them, among them `internal/mcpx/reference_test.go` and the example
      checks of `internal/runtime/fetch_test.go`.
35. Each changed sentence keeps "injected" for a secret of this spec only.

## The example

36. The example stays one server flat with `API_BASE` and `API_TOKEN`.
37. `examples/external-api/server.js` calls `fetch` to `env.API_BASE` without an `Authorization` header, and its README
    and "Server setup" set `API_TOKEN` as an injected secret with `--origin` and `--header Authorization`, with the value
    `Bearer <token>`.

## Known limits

- An origin that copies request headers into its response gives the value to flat code (rule 17). The host cannot
  tell such an origin from another.
- Flat code chooses the path, the method and the body of a request to the bound origin. On an origin that serves the
  endpoints of other parties under one host name, flat code can send the header to such an endpoint. A credential that
  lets flat code ask the service for a new credential gives flat code that new credential.
- A credential that the service allows to do more than the flat needs can do all of it from flat code.
- A value of 8 KiB with its header name is longer than the header line limit of many servers, and 32 bindings for one
  origin can add 256 KiB to a request. Such a request fails at the service, and the set gives no warning.
- A preview runs code that no approval checked, with the injected secrets of the flat, as it runs today with the
  readable ones.
- A secret that was readable before it was removed and set again as an injected secret may still be in data that the
  flat stored. Rule 8 says to get a new credential.
- A console page that was loaded before a secret was removed and set again as an injected secret does not show the new
  binding, and its **Replace** sends a value to that binding.
- The binding is not part of the associated data of the seal, which is the name only. A writer of `flats.db` can empty
  the two columns and make an injected secret readable at the next activation, or bind it to another origin. A later
  change that puts the binding into the associated data must seal each injected value again.

## Completion criteria

Test seams. The egress client has three seams that only tests in package `egress` set: the trusted root certificates
(empty means the system roots), the resolver and the dialer. They are unexported fields, and `egress.New` is the only
constructor, so product code cannot set them. The runtime has one seam that only tests in package `runtime` replace,
the function that builds the outbound client of a worker. It is a field of the worker, so parallel tests do not share
it, and an empty field means the product function. The positive HTTPS checks run in package `egress` with
local TLS servers. The runtime checks prove what the worker gives to that function and to flat code. Every check that
a request gets no header runs in the same setup as a check that the bound request gets it. No test needs the network.

- The egress client: a request to the bound origin carries the header with the stored bytes, also for the forms
  `https://API.example.com/v1` and `https://api.example.com:443/v1`; a request to another permitted origin, to
  `http://` on the same host, to a subdomain and to a longer host name does not; a request to the bound origin without
  its network grant fails before the dial; a request of flat code that supplies the bound header (in each case form) to
  the bound origin fails before the rate limit and the dial, and its error names the header and does not hold the
  value; the same header to another origin is sent; a redirect response with `redirect: "manual"` is returned and no
  second request is made; the added header passes when the headers of flat code are at their limit; a list that breaks
  rules 5 to 7 makes `egress.New` fail; no error text holds the value.
- The runtime: a JavaScript flat with an injected secret and a readable secret reads `env.READABLE`, gets `undefined`
  for `env.INJECTED`, and `JSON.stringify(env)` does not hold the injected value; the worker gives the injected list of
  its spec, and nothing else, to the function that builds its outbound client; the request that flat code passes to
  `fetch` does not hold the value; a WASI command does not get the name or the value; the worker spec holds the
  injected secret outside `env`; a worker that the manager replaces gets the same list; a worker that builds its
  outbound client with the product function from a spec with an injected list runs flat code that supplies the bound
  header to the bound origin, and the call fails with the error of rule 15 before any address lookup.
- The service: each refusal of rules 6 to 8 and 11, with its message and its status class; a set without a binding of
  an empty value, of a value with a control character and of a value that ends with a space, through the console route
  and through the route with the CLI header, is 400, and the stored value and binding stay and the next activation
  passes; `flats secret set` without flags gets 400 for a piped value that ends with two newlines or with a space, and
  it refuses an empty piped value itself and sends no request, as today; a new
  value keeps the binding through the API with and without `inject`; an ordinary variable with the name of an injected
  secret is refused; the capture puts an injected secret into the injected list and not into `env`; each stored state
  of rule 20 (one binding column only, a bad origin, a bad header name, a bad value, two rows with one binding, more
  than 32 injected rows) refuses the activation and
  names the secret (for more than 32 rows, the flat and `flats secret ls`), and the value is not in `env`; the redactor removes an injected value from a log line; the docs app
  and a WASI flat get no list; a rename keeps the list, and a new preview captures the current one.
- No host text holds the value: a test stores a distinctive injected value and runs each set refusal of rules 7 and 8,
  a capture refusal of rule 20 through a deploy, the listings of the API, MCP and the CLI, `flats audit ls` and the
  export. No output, flat event, audit row or approval result holds the value or an encoding that the redactor knows.
- The store: the migration from schema 27 keeps every secret readable and its value openable; a reader of schema 27
  refuses schema 28; a set and a list keep the binding; `secret.set` has the data of rule 29; the re-seal of
  `rotate-key` keeps the binding; two sets of one new name with two bindings, run at the same time many times, leave
  each time one binding with the value of the same set; a set without a binding of the value `a\nb` (allowed for a
  readable secret) and a set of the same new name with a binding, run at the same time many times, never leave an
  injected row with that value; the unique index refuses a second row with the same binding.
- The API: `PUT` with `inject` returns the stored binding; `"inject": null` is the same as no `inject`; `GET` returns
  `inject` for an injected secret only; 400 and 409 as rules 7, 8 and 27 say; 403 for a remote caller.
- The CLI: `--origin` without `--header` and the reverse are usage errors; `secret ls` prints `INJECTED`; the output of
  a set names the origin and the header; the help text names the flags.
- MCP: `list_secrets` returns `inject` for an injected secret, and its description names injection.
- The webhook: a set and a removal of an injected secret send no event.
- The other commands: `flats secret reset` names and removes an injected secret; `flats secret restore-key` accepts a
  key that opens an injected value.
- The console: the UI test shows the text of rule 26 for an injected secret and none for a readable one, **Replace**
  sends only the value, and the delete dialog of an injected secret has its text.
- The example: the example check runs `server.js` with `API_BASE` only, and its request holds no `Authorization`
  header.
- The doc checks pass, and the generated references match their sources.

## Open questions

None.
