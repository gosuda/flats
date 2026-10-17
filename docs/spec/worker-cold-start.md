# Fast worker cold start

Status: partly implemented

## Summary

A server flat's worker, including a sleeping document that wakes on demand,
answers its first request quickly. Flats' own startup work (process start,
JavaScript engine setup, the runtime prelude and the built-in document app)
must not make the first response slow; only a flat's own top-level code may
add to it.

## Requirements

1. On a host whose compilation cache is already warm, Flats' own startup
   overhead for a JavaScript server flat is under 50 ms: from the moment a
   worker is needed until it serves its first request, excluding the time the
   flat's own module code takes to load. This holds when the host's
   pre-started worker (requirement 5) is ready; a start that finds none ready
   takes the ordinary, slower path.
2. A sleeping built-in document answers its first page request in under
   50 ms, measured at the host from request arrival to the complete response.
   A collaboration connection opened right after it joins in under 50 ms.
   The same pre-started worker condition applies.
3. Observable behavior does not change. In particular:
   - A flat's module top-level code still runs in every JavaScript runtime
     that serves it, with the same environment, clocks, randomness and logs
     as before. Flats never reuses the result of a flat's own code across
     runtimes, except for the built-in document app (requirement 4).
   - Every runtime has its own independent `Math.random` sequence, and its
     `performance.now()` measures time from that runtime's own start and
     never decreases. Exception: runtimes of the built-in document app that
     start from a saved image share that image's `performance.timeOrigin`
     and continue its clock (requirement 4); the app does not use either.
   - Isolation, resource limits, timeouts, startup error reporting, crash
     restart and backoff, idle sleep and wake preparation, and the WASI
     engine behave as before.
4. The built-in document app may start from a saved image of its own
   loaded state. That image is created only from the document app's code and
   that document's content, never contains environment values, secrets or
   data read from the document's database or files, is private to the host,
   and is deleted together with the document's runtime files.
5. To start workers without paying for process start-up, a host keeps at
   most one pre-started, unassigned worker process while it has any server
   flat or document. That process holds no flat's code or data until it is
   assigned to one, and it ends with the host. A sleeping document still
   holds no process of its own.
6. A slower path never fails where the old one succeeded: when a saved image
   or the pre-started worker is missing, stale, broken or unsuitable, the
   worker starts the old way.

## Current state

The design is in [worker-cold-start-TECH.md](worker-cold-start-TECH.md).
Implementation is in progress; nothing has shipped yet.

## Open questions

None.
