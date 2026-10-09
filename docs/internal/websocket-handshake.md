# WebSocket handshake hash and CodeQL

Maintainer note for the server-flat runtime; not part of the agent-facing runtime API.


`internal/runtime/ws.go:wsAccept` implements the exact Sec-WebSocket-Accept
calculation required by [RFC 6455 section 4.2.2](https://datatracker.ietf.org/doc/html/rfc6455#section-4.2.2):
Base64 of SHA-1 over the public Sec-WebSocket-Key nonce followed by the fixed
WebSocket GUID. The server reads only that header; the client generates a fresh
random nonce. Neither call hashes a password, bearer token, document secret,
or session credential. This value confirms protocol negotiation and grants no
access. [Section 10.8](https://datatracker.ietf.org/doc/html/rfc6455#section-10.8)
explicitly explains that the handshake does not depend on SHA-1 collision or
second-preimage resistance. Substituting SHA-256 breaks compliant peers.
`TestWSAcceptRFC6455` checks the RFC's independent handshake vector.

[CodeQL alert #35](https://github.com/gosuda/flats/security/code-scanning/35)
(`go/weak-sensitive-data-hashing`, reported on PR #25 at `26c4f798`) classifies
this operation as sensitive hashing. Its reported sources are document/route
secret handling elsewhere in core, but the handshake hash input is the nonce
header (or locally generated nonce), not those credentials. The scoped treatment
is to dismiss this single alert as a false positive with this rationale in its
GitHub audit record. The repository uses CodeQL default setup; retain its queries,
languages and scanning schedule, and do not suppress the rule or exclude runtime
files. Reassess the exception if the function's inputs or purpose change.
