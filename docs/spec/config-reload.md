# Config reload on SIGHUP

Status: implemented

A running host reads `config.json` once at start. An operator who edits the
file directly must restart the host for the change to apply, and until then
the console refuses settings saves because the file's hash no longer matches
the settings ETag. This spec adds an explicit reload: SIGHUP tells a running
host to adopt the file without interrupting serving.

The rule for every key is: a reload applies what a restart would apply,
immediately, for every key with a defined live path. Keys without one wait
for the next start, as they do today. Where configuration.md says a key
applies "immediately from the console; otherwise restart", the reload is the
live path for file edits: it runs the same side effects as the console save.
A change that the running host cannot apply safely at once waits for the
next start, and a file the next start would refuse to run with is refused,
as requirements 6 and 9 describe. Requirements 4 to 9 below partition every key of the configuration.md key
table; a key's bucket follows the table's Applies column.

## Requirements

1. **Trigger.** Windows has no SIGHUP, so this spec does not apply there: see
   [Windows support](windows-support.md). SIGHUP to a `flats serve` process whose standard input is
   not a terminal, as under a service manager or in a container without a
   TTY, requests one reload. It never terminates or interrupts that host.
   There is no file watcher. Reloads happen only on the signal. Reloads are
   serialized by the settings lock (requirement 10); SIGHUPs that arrive
   while one runs coalesce into at most one further reload. A SIGHUP that
   arrives while the host starts reloads once the host has started, so an
   edit saved after the start read the file is not lost.

   A host whose standard input is a terminal was started by hand, and there
   SIGHUP means the terminal closed: the host shuts down cleanly, as on
   Ctrl+C, and does not reload. A SIGHUP that was already ignored when the
   host started (as under `nohup`) stays ignored, with or without a
   terminal; such a host neither reloads nor stops on it.

2. **Validation before adoption.** A reload parses and validates the whole
   file with the same rules a start applies: strict parsing, per-key checks,
   the documented cross-key checks (proxy address uniqueness,
   external-origins versus proxy-template overlap, default providers within
   the permitted set, at most one of `proxy` and `proxy-public`), and the
   host binding (`host.instance_id` and `host.data_dir` must match the
   running host; they never change on a live host) and the values a legacy
   service flag pins (requirement 9). The host refuses an
   unreadable, missing or invalid file, and one whose `schema_version`
   differs from the running host's schema in either direction, because a
   reload cannot migrate a running host. A refusal keeps the current
   settings, logs the reason, and leaves the settings ETag naming the file
   the host last read or wrote.

3. **Adoption.** A successful reload makes the file the host's current
   truth. The settings ETag advances to the file's hash, so the
   changed-on-disk condition clears and console saves work against the new
   ETag. The adoption is all-or-nothing: a reload that fails validation
   changes nothing.

4. **Keys with immediate effect.** These keys take effect on reload with
   the same side effects as a console save:

   - `system.rate_limit_rps` pushes the new rate to every live flat's
     request limiter.
   - `system.upload_max_bytes` governs new uploads.
   - `system.version_quota` governs new versions; existing versions are
     kept.
   - `system.revision_quota` prunes when lowered.
   - `system.pin_quota` keeps existing pins and refuses new pins while
     full.
   - `system.disk_quota_bytes` pushes document budgets and re-measures
     boards.
   - `portal.hide` re-applies host listings.
   - `proxy.url_template` and `console.external_origins` affect new
     requests.
   - `console.preferred_host` enters the adopted document; a preference
     whose name is not listed is not in effect, as the console save path
     already defines.
   - `network.default_private` and `network.default_public` govern flats
     created afterwards.

   If a side effect fails after the file is adopted, the reload keeps the
   adopted document and the new ETag, and logs the failed side effect; the
   effect retries at its next occasion, such as a later settings save or a
   restart.

5. **Keys with periodic effect.** These keys enter the adopted document
   and govern their next occasion: `system.preview_ttl_seconds` at the next
   sweep, `system.events_keep` at the next pruning, `system.redirect_days`
   for new renames. They log as applied; no side effect runs.

6. **Network providers.** A reload applies `network.permitted` by the
   console's rules for turning a provider on or off. A newly permitted
   provider's host grant is recorded and its backend starts, as when the
   console turns it on; a backend contacts nothing until a flat allows the
   provider. A removed provider that no flat allows loses its host grant, as
   when the console turns it off. A grant or backend failure is logged and
   leaves that provider's state as it was; the next reload retries it, and
   the rest of the reload stands.

   A removal that only a start can apply safely waits for it. The console
   refuses to turn off a provider that a flat still allows; a reload instead
   keeps that provider's host grant and routes for this run and logs,
   naming the provider and the flats that allow it, that the removal applies
   at the next start. The next start opens no route for the provider; each
   flat's provider permission and visibility in `flats.db` stay, and nothing
   is published or unpublished. A later reload removes the grant once no
   flat allows the provider, and one that lists the provider again leaves
   its grant and routes as they are. The two reverse proxy kinds share one
   listener and are never on together, by their host grants: a reload
   applies removals before additions, and while a removed kind stays on
   because flats allow it, the other kind is not turned on, which the
   reload logs. It is turned on once the first kind is off, by a later
   reload or at the next start; the console refuses to turn it on until
   then. A removed kind whose route is still registered, such as one a flat
   allowed while the reload ran, also stays on with its listener, as the
   console's turn-off refuses then. Likewise, while private routes run on
   Tailscale, a reload does not remove Tailscale's grant: the
   `network.private_backend` change that moves them off it applies only at
   the next start. The reload logs that Tailscale stays on until then.

   The reconciliation applies `network.permitted` as it is when it runs,
   under the lock the console's provider changes hold. A console provider
   change that lands after the file was adopted is therefore kept, not
   undone by the reload.

7. **Console domains.** A reload reconciles both console domain lists,
   `tailscale.console_hosts` and `netbird.console_hosts`, the way the next
   start would, whether or not the file changed them. A listed name that is
   not served starts being served, as when the console adds it; a name
   already served is left as it is. A served name that is no longer listed
   is retired the way a start retires it: the Tailscale node logs out, the
   NetBird peer stops, and the identity is deleted. A failed retirement
   keeps the name reserved, is logged, and retries at the next reload or
   start. A conflict or a failed retirement leaves that name's state as it
   was; the rest of the reload stands, and the next reload retries the
   name, serving it once its conflict has gone. A conflicting name enters
   the adopted list but is not served, and the conflict is logged. The
   conflict and reservation rules of the console flows apply unchanged: a
   name taken by a flat, preview, redirect or a still-retiring node is not
   served. The check and the start of the name's node hold the name's
   reservation, as a console addition does, so a flat being created under
   that name meanwhile either finishes first, and the name is not served,
   or is refused.

8. **Restart-required keys.** These keys enter the adopted document and
   apply at the next start, exactly as they do today: `host.console_addr`,
   `host.local_addr`, `host.server_runtime`, `metrics.enabled`,
   `metrics.expose_on`, `metrics.per_flat`, `network.private_backend`,
   `portal.relays`, `portal.discovery`, `portal.max_active_relays`,
   `zrok.environment`, `zrok.namespace`, `netbird.management_url`,
   `proxy.addr`, and the deprecated `credentials.tailscale_authkey_file`.
   `host.instance_id` and `host.data_dir` never change on a live host
   (requirement 2), and `credentials.operator_file` stays ignored. The
   running host keeps its current values and logs each changed key that
   awaits a restart.

9. **Run overrides and pins.** Values set for this run by service flags
   survive a reload. The config-mode run overrides `--listen`
   (`host.console_addr`), `--local-addr` (`host.local_addr`),
   `--console-host` (`tailscale.console_hosts`) and `--runtime`
   (`host.server_runtime`) are never stored: the running value stays, a
   differing file value enters the adopted document without changing this
   run, and the reload log names each such key and its flag.

   A legacy service's flags pin values instead: `--relays` pins
   `portal.relays`, `--console-host` pins `tailscale.console_hosts`, and
   `--portal`, `--network tailscale` and `--permit` pin their providers'
   membership of `network.permitted`. Such a service compares its flags
   with the file at every start and refuses to run while they differ, and
   the console refuses to change a pinned value. A reload likewise refuses,
   all or nothing as in requirement 2, a file whose value for a pinned
   setting differs from the running value; the refusal names the key and
   the flag that pins it, as the console's refusal does.

10. **Atomicity.** A reload holds the same settings lock as a console save,
    so the two cannot interleave, and each observes the other's result.
    Both run the same side effects for a changed setting. Shutting the host
    down waits for a reload in flight but never cancels its adoption. It
    cancels the provider and console domain reconciliation: a provider or
    console domain change already running finishes, no further one starts,
    and the next start applies the rest. A console domain retirement that
    has not begun stays recorded as retiring, so the next start retires it.

11. **Observability.** Every reload attempt logs its outcome. A successful
    reload logs the applied keys, the keys awaiting restart, and the keys
    held by run overrides. A refusal logs the reason. Keys are logged by
    name only, never by value.

## Open questions

- Whether the console or CLI should offer a reload trigger, such as a button
  or `flats config reload`, in addition to SIGHUP. Not part of this spec's
  target. SIGHUP covers service managers and operators on the host.
