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
Requirements 4 to 9 below partition every key of the configuration.md key
table; a key's bucket follows the table's Applies column.

## Requirements

1. **Trigger.** SIGHUP to the `flats serve` process requests one reload.
   SIGHUP never terminates or interrupts the host. There is no file watcher.
   Reloads happen only on the signal. Reloads are serialized by the settings
   lock (requirement 10); SIGHUPs that arrive while one runs coalesce into at
   most one further reload. A SIGHUP that arrives before the settings source
   exists is ignored.

2. **Validation before adoption.** A reload parses and validates the whole
   file with the same rules a start applies: strict parsing, per-key checks,
   the documented cross-key checks (proxy address uniqueness,
   external-origins versus proxy-template overlap, default providers within
   the permitted set, at most one of `proxy` and `proxy-public`), and the
   host binding (`host.instance_id` and `host.data_dir` must match the
   running host; they never change on a live host). The host refuses an
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

6. **Network providers.** A reload applies `network.permitted` the way the
   next start would, without waiting for it. A newly permitted provider's
   host grant is recorded and its backend starts, as when the console turns
   it on; a backend contacts nothing until a flat allows the provider. A
   removed provider's grant is removed and its routes come down, as they
   would at the next start; each flat's provider permission and visibility
   in `flats.db` stay, and nothing is published or unpublished. Unlike the
   console, a reload does not refuse a removal while a flat still allows the
   provider: the file is the operator's explicit instruction, and a restart
   would take the same routes down. The resulting state is the one a restart
   with the same file would produce: a flat keeps its permission for the
   removed provider, the provider serves no routes while it is not
   permitted, and the console shows it as not permitted, exactly as after a
   restart. A grant or backend failure is logged and leaves that provider's
   state as it was; the rest of the reload stands.

7. **Console domains.** A reload reconciles both console domain lists,
   `tailscale.console_hosts` and `netbird.console_hosts`, the way the next
   start would. A listed name that is not served starts being served, as
   when the console adds it. A served name that is no longer listed is
   retired the way a start retires it: the Tailscale node logs out, the
   NetBird peer stops, and the identity is deleted. A failed retirement
   keeps the name reserved, is logged, and retries at the next reload or
   start. A conflict or a failed retirement leaves that name's state as it
   was; the rest of the reload stands. A conflicting name enters the adopted
   list but is not served, and the conflict is logged. The conflict and
   reservation rules of the console flows apply unchanged: a name taken by a
   flat, preview, redirect or a still-retiring node is not served.

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

9. **Run overrides and pins.** Values pinned for this run by service flags
   survive a reload: the config-mode run overrides `--listen`
   (`host.console_addr`), `--local-addr` (`host.local_addr`),
   `--console-host` (`tailscale.console_hosts`) and `--runtime`
   (`host.server_runtime`); the legacy `--relays` flag pins
   `portal.relays`; and the legacy `--portal`, `--network` and `--permit`
   flags pin their provider grants in `network.permitted`. The running value
   stays, a differing file value enters the adopted document without
   changing this run, and the reload log names each such key and what pins
   it.

10. **Atomicity.** A reload holds the same settings lock as a console save,
    so the two cannot interleave, and each observes the other's result.

11. **Observability.** Every reload attempt logs its outcome. A successful
    reload logs the applied keys, the keys awaiting restart, and the keys
    held by run overrides. A refusal logs the reason. Keys are logged by
    name only, never by value.

## Open questions

- Whether the console or CLI should offer a reload trigger, such as a button
  or `flats config reload`, in addition to SIGHUP. Not part of this spec's
  target. SIGHUP covers service managers and operators on the host.
