# Playbook: pull requests, merges and releases

Publication and merging follow the
[development protocol](../../../../docs/development-protocol.md#returns-resume-and-completion):
publish only within the user's authorized scope, and merge only with explicit
authorization for that specific PR. This playbook adds checks; it grants no
authority.

## Before publishing

- [ ] Title the PR in Conventional Commits style, lowercase, as in the
  repository history: `type(scope): description`, for example
  `fix(console): show byte limits exactly`. The scope is optional.
- [ ] Main is linear: each PR lands as one squash commit. Never push to main
  directly.
- [ ] Audit every commit's author and committer
  (`git log --format='%an <%ae> | %cn <%ce>' origin/main..HEAD`). Commit
  identity is public metadata in this public repository; a missing identity
  config can fall back to a local machine name.
- [ ] Inspect every attached image or video byte for byte, including embedded
  metadata, before it goes public. Screenshots follow
  [console-ui](console-ui.md#screenshots-in-a-pr).
- [ ] Write the description with [flats-unslop](../../flats-unslop/SKILL.md)
  and link the task report as the protocol requires.

## Before a maintainer merges

- [ ] Check the live main ref on the remote (`git ls-remote origin
  refs/heads/main`), not only the base the PR recorded. If main moved, the
  tested tree is not the tree that will land.
- [ ] Confirm that required checks ran on the current PR head.

## After a merge or release

- [ ] `go install github.com/gosuda/flats/cmd/flats@latest` can resolve an
  earlier commit for a while through the module proxy. Check the binary with
  `go version -m`, and install the explicit revision to verify a new commit.
  See [Build version](../../../../docs/release.md#build-version).
- [ ] Release steps are in [Cutting a release](../../../../docs/release.md#cutting-a-release).
