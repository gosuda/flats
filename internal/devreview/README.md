# devreview: review evidence helper

`devreview` records and checks the evidence that the
[development protocol](../../docs/development-protocol.md#snapshot-and-evidence)
asks for, so each executor does not reimplement it. Its rules are rules 40-50
of the [workflow spec](../../docs/spec/dev-workflow.md#review-evidence-helper).
It checks identities and records invocations. It never approves a stage, and
an exit status of 0 never means a reviewer accepted anything.

It is development tooling. The `flats` binary does not link it. Run it from
the repository:

```sh
go run ./internal/devreview/cmd/devreview <command> [flags]
```

Every manifest, copy, run record and report goes outside the repository
working tree. The helper resolves symbolic links before checking this and
refuses paths that land inside the tree. Keep one evidence directory per task.

## Commands

| Command | Does | Exit status |
| --- | --- | --- |
| `snapshot -base REF -o FILE` | Records the candidate in a manifest | 0, or 2 on error |
| `verify -manifest FILE [-copy DIR]` | Reports drift of the working tree, or of a copy | 0 no drift, 1 drift, 2 error |
| `materialize -manifest FILE -o DIR` | Writes a read-only copy of the candidate | 0, or 2 on error |
| `run -manifest FILE -reviewer NAME -prompt FILE -runs DIR -author MODEL ...` | Runs a configured reviewer and records the invocation | see [run](#run) |
| `report -manifest FILE -runs DIR -o FILE` | Writes a task report skeleton | 0, or 2 on error |

`-C DIR` selects the repository when the current directory is outside it.

## snapshot and verify

The candidate is the working tree. `snapshot` compares it with the merge base
of `-base` (default `HEAD`) and `HEAD`, and lists every path that differs from
the merge base, `HEAD` or the index. The list includes untracked files that Git
does not ignore. Ignored inputs that matter go in with `-include PATH`; a
directory includes every file beneath it. `-exclude PATTERN` keeps reports,
logs and probe output out; exclusion wins over everything else. A pattern
ending in `/` is a directory prefix. Any other pattern uses Go's `path.Match`
against the whole path, and a pattern without `/` also against the base name.

Each entry records its status against the merge base and `HEAD`, whether the
index differs, and the mode, size and SHA-256 of its working-tree content. A
symbolic link is identified by its target string. The manifest digest covers
the base, `HEAD`, a digest of the index, the options and every entry. Editing
the manifest breaks it. Submodules, nested repositories, conflicts and special
files are refused.

`verify` recomputes the manifest. A moved `HEAD`, a staging change, or any
added, removed or changed path is drift.

## materialize

`materialize` creates a detached worktree of `HEAD` at `-o`. It applies the
manifest's entries and makes every file read-only. It refuses symbolic links
whose target leaves the copy. `verify -copy DIR` checks the copy later: every
entry must still match, and nothing else may differ from `HEAD`. Remove a copy
with:

```sh
chmod -R u+w DIR && git worktree remove --force DIR
```

Reviewing a copy binds the review to the snapshot. A review of the live
working tree is bound only when the executor also prevented concurrent
writes: a change can be made and undone between two `verify` runs.

## run

`run` reads reviewer commands from a configuration file outside the
repository: `-config`, then `$FLATS_REVIEW_CONFIG`, then
`<user config directory>/flats-dev/reviewers.json`. The file must not be
writable by other users. The repository holds no reviewer command or model
name.

```json
{
  "reviewers": {
    "second-model": {
      "command": ["/path/to/your/review-command", "--any", "arguments"],
      "requested_model": "<canonical model ID the command requests>",
      "timeout": "45m",
      "fixed_binding": "<optional: where the provider documents that this ID is not an alias>"
    }
  }
}
```

The command runs without a shell, in its own process group, in the review
surface: the copy from `-copy`, or the live tree with
`-writes-prevented "how"`. The prompt comes on standard input. The command
also gets these environment variables:

| Variable | Value |
| --- | --- |
| `FLATS_REVIEW_ROOT` | The review surface; also the working directory |
| `FLATS_REVIEW_MANIFEST` | The manifest |
| `FLATS_REVIEW_PROMPT_FILE` | A copy of the prompt |
| `FLATS_REVIEW_IDENTITY_FILE` | Where to write identity metadata |
| `FLATS_REVIEW_INVOCATION` | The invocation ID |

Effective identity comes only from execution metadata. The command, or a
small adapter script of yours that wraps it, writes this file:

```json
{"effective_model": "<model ID the execution reported>", "session": "<session ID>", "source": "<which metadata field or response reported them>"}
```

Do not use the model's own answer about itself. A `fixed_binding` stands in
for the effective model, never the session.

`run` refuses to start when the surface does not match the snapshot, when no
`-author` is given, when a live-tree run has no `-writes-prevented`
statement, when an earlier invocation may still be running, or when the
invocation budget is spent. The first run in an evidence directory sets the
budget with `-budget N`. It counts every invocation in the directory. Changing
it later needs `-budget-reason`, which is kept in `budget.json`.

Each invocation gets a directory `<runs>/<id>/` with `run.json`,
`prompt.txt`, `stdout.txt`, `stderr.txt` and the identity file. The record
holds the surface, the checks before and after, times, status and exit code,
identity, eligibility against the authors, and any earlier invocation that
used the same session. A held lock inherited by the reviewer's processes
marks a running invocation. A later start reconciles a record left
`running`: it becomes `interrupted` only when the lock is free and the
process group is gone. Otherwise the start is refused. `run` works on Unix
systems only.

| Exit | Meaning |
| --- | --- |
| 0 | Complete evidence: exit 0, known model and session, eligible reviewer, surface unchanged |
| 1 | The command failed, timed out, was cancelled or could not start |
| 2 | Refused or a usage error; no command ran |
| 3 | The command exited 0 but the evidence is incomplete |

## report

`report` writes a Markdown skeleton with the sections the protocol requires.
It fills in the snapshot identity, authors, invocation table, receipt paths,
budget and the limitations the records show. Everything that needs judgment
is marked _(fill in)_: the reviewer's exact verdict, dispositions,
verification evidence, the correction budget and the next action. It never
overwrites a file.
