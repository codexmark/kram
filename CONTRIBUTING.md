# Contributing to Kram

Kram is in public beta. Contributions are welcome, especially reproducible bug reports, platform compatibility findings, documentation fixes, and focused changes that improve reliability or developer experience.

## Reporting problems

Use the GitHub issue templates so reports contain enough context to reproduce the behavior.

Choose:

- **Bug report** for reproducible incorrect behavior in Kram itself.
- **Platform compatibility** when the problem depends on OS, architecture, terminal, shell, installation, or host behavior.
- **Feature request** for a concrete workflow problem that needs new behavior.

Never post API keys, OAuth tokens, credentials, private repository contents, or other secrets. Sanitize logs and screenshots before attaching them.

When reporting agent behavior, distinguish runtime correctness from model behavior where possible. A permission bypass, incorrect tool result, corrupted session, or wrong runtime state is a Kram bug. A model choosing an inefficient sequence of otherwise-correct tools may instead be a model/prompt/policy behavior issue. If you are unsure, open the issue and describe what happened.

## Priority vocabulary

Issues may be triaged with the following priority levels:

- **P0** — data loss, corruption, serious security problem, or dangerous incorrect execution.
- **P1** — major functional breakage, blocked installation/startup/onboarding, or another problem that prevents normal use.
- **P2** — real defect with limited impact or a usable workaround.
- **P3** — polish, minor inconsistency, or low-impact improvement.

Priority is assigned during triage; reporters do not need to determine it themselves.

## Development workflow

Create a branch for the change, keep the scope focused, and open a pull request against `master`.

Before opening the PR, run:

```sh
./scripts/verify.sh
```

The repository verification gate performs formatting checks, `go vet`, a fresh race-enabled Go test suite, the global coverage floor, host build validation, Windows and Android cross-builds, and installer behavior tests.

For a small documentation-only change, the complete runtime gate may not exercise the changed content directly, but the branch should still remain mechanically clean and the relevant links/examples should be verified.

## Pull request expectations

A useful pull request explains:

1. what changed;
2. which problem it solves;
3. whether runtime behavior changed;
4. how the change was verified;
5. any known limitations or platform-specific effects.

Prefer narrow PRs that can be reasoned about independently. Avoid bundling unrelated refactors with a bug fix unless the refactor is required to make the fix correct.

Changes that alter permissions, process execution, routing truth, persistence, installer behavior, or release integrity should include deterministic regression coverage whenever practical.

## Compatibility changes

When changing platform-specific behavior, state which environments were actually exercised. A successful cross-build proves compilation, not necessarily terminal, process, filesystem, or provider behavior on the target host.

Kram currently publishes builds for Linux amd64/arm64, macOS amd64/arm64, Windows amd64, and Android/Termux arm64. See the main README for the current support boundaries.

## Commit messages

Use concise commit messages that describe the change itself. Do not add automated co-author trailers or generated attribution trailers to commits.
