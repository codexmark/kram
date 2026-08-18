# AGENTS.md

Project-level rules for any agent (Claude Code or otherwise) working in
this repository. Read fresh every turn — see `internal/daemon/agent/agent.go`
for how Kram's own agent loop does the same for its users.

## Git commits

**Never add a `Co-Authored-By` line for Claude/Anthropic, or any AI
co-author trailer, to commit messages in this repository.** The user does
not want AI attribution appearing in the repo's authorship history. This
applies to every commit, not just ones where it was asked about directly.

## Where to look first

- `README.md` — architecture, components, how to run each piece.
- `DECISIONS.md` — why things are built the way they are, including
  reversed decisions and deliberate omissions. Check here before
  re-litigating something that looks like it could be simplified; it may
  have already been tried the other way.
