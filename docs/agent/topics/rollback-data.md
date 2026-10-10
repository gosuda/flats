## Rollback and live data

### Live data

Server and docs flats keep live data: `env.DB` and `env.FILES` for server
flats, the collaborative document state for docs flats. Live data belongs to
the flat and survives successful redeploy, ordinary rollback and host restart.
Deleting the flat deletes its data. Previews work on isolated copies. Versions
are code, not data backups.

Before activating a version of a flat that has data, Flats snapshots the live
data: the database and FILES together. It keeps a bounded number of snapshots
per flat (10, plus 3 from failed deploys).

### Rollback

`rollback {slug, version?}` requests approval to activate an earlier
published version (default: the one live before the current one). Code only
by default: live DB and FILES stay as they are.

`restore_data: true` also replaces the live data with the snapshot taken
before the current live version was deployed. Current data is first backed up
as a new snapshot, and the target version is health-checked on a copy of the
snapshot before anything changes. Snapshots from current hosts capture DB and
FILES, and restoring them replaces both; legacy DB-only snapshots from older
releases restore the database and preserve current FILES. Writes made since
the snapshot stop being live. Nothing is restored before operator approval.
Ask the user before requesting it, then poll `get_approval` and report its
`result_data` (`data_impact`, `health_data`, `live_data`).

A rollback to the version that is already live, or with nothing live yet, is
refused (`refusal.conflict`, `refusal.not_deployed`). Restoring a named
snapshot without changing code is an operator console action.

For docs flats, a code rollback merges the target version's Markdown into the
live text: independent human edits are kept and overlapping lines prefer the
target (`topic.docs`).
