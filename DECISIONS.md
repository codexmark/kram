# Decisions

Why Kram is built the way it is. This records the decisions where the
reasoning isn't obvious from the code — especially the ones that were
wrong at first, since a decision that got reversed carries more
information than one that never got tested.

Each entry: what was decided, what the alternative was, and why. Where a
decision came from studying another project, that project is named.

---

## Architecture

### One binary, components in-process

`cmd/kram` runs gateway, daemon and CLI as goroutines in a single
process, not as three subprocesses.

**Alternative:** separate processes, coordinated by ports and a
supervisor.

**Why:** the three components already communicate over HTTP, so keeping
them independently runnable costs nothing — `internal/gateway` and
`internal/daemon` each export the same `Run()` the standalone binaries
call. But the *default* experience should not require three terminals and
manual port coordination. In-process gets a single `ctrl+c` that shuts
everything down together, with no orphaned processes or bound ports.

The components stay genuinely separable: you can still run them on
different machines when you want to.

### A gateway in front of the agent

Provider selection, fallback and circuit breaking live in a separate
layer the agent talks to, rather than inside the agent loop.

**Why:** it makes "which provider answered, after how many attempts" a
first-class fact the whole stack can see — the CLI's footer shows the
real fallback trail per request, not a guess. It also means the agent
loop never contains provider-specific code. Borrowed from OmniRoute's
core idea, reimplemented rather than wrapped.

### The daemon owns durability, the CLI owns nothing

Sessions and messages are persisted to SQLite by the daemon before any
caller is told the write succeeded. The CLI is a pure view.

**Why:** a conversation must survive closing the terminal, and a crash in
the UI layer must not be able to lose history. This is Compozy's
local-first daemon model. Notably, Compozy's own v0.3 converged on the
same shape (daemon + SQLite) after starting from Temporal-backed
workflows — which is evidence against adopting a workflow engine at
Kram's scale.

Pure-Go SQLite driver (`modernc.org/sqlite`), no cgo, to keep the static
binary property.

---

## Cross-platform shell

### A central runner, not per-call-site platform checks

`internal/shell` is the only place that knows how to build and kill a
shell command; `bash.go`, `background.go`, and `customtools.go` all go
through it instead of calling `exec.Command("sh", "-c", ...)` themselves.

**Why:** every one of those three call sites had the same two bugs —
assuming `sh` exists (false on a bare Windows install) and assuming
`Process.Kill()` stops what the command started (false the moment a shell
forks a child, on any platform). Fixing it in three places independently
would have meant three chances to fix it inconsistently, or forget one.

### Unix: a real process group; Windows: a Job Object

Unix commands run with `Setpgid: true` so the shell leads its own process
group; killing sends SIGTERM to the whole group (negative PID), then
SIGKILL after a short grace period if it's still alive. Windows has no
process-group equivalent, so commands are assigned to a Job Object
configured with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` — the actual OS
mechanism other tools (Docker Desktop, containerd, Chromium's launcher)
use for the same "kill this and everything it started" problem.

**Why not just track child PIDs ourselves:** a shell can fork
grandchildren Kram never sees a PID for. Only the OS's own grouping
primitive reliably reaches the whole tree.

### `cmd.exe /S /C`, not `sh.exe`, on Windows

Resolved via `COMSPEC` (falling back to the literal `cmd.exe`), never
Git Bash or WSL's `sh`.

**Why:** those only exist if the user happens to have installed them.
Depending on that silently was the exact bug being fixed — a tool
description that promises `sh -c` semantics on a platform where they
don't hold is a wrong answer that looks like a right one.

### Windows Job Object assignment happens after `Start()`, not via `CREATE_SUSPENDED`

A known, accepted race: a child that forks in the sliver of time between
`Start()` and job assignment could escape the job.

**Why accepted rather than closed:** closing it means creating the
process suspended and resuming it via the thread handle from
`CreateProcess`'s `PROCESS_INFORMATION` — `os/exec` doesn't expose that
handle, so doing it properly means reimplementing `CreateProcess`
ourselves instead of building on `os/exec`. Shell commands spawn children
well after `cmd.exe` itself starts running (parsing the command line,
resolving the program), not in that first instant, so the practical risk
is low. Revisit only if this actually bites someone.

---

## LSP

### Lazy per-language servers, never started at daemon startup

`lsp.Manager` reads no config and starts no process until the first
`lsp_*` tool call for a given language; concurrent calls for the same
language before it's ready all wait on the one in-flight start rather
than racing to launch two.

**Why:** the same invariant MCP servers deliberately don't get (those
connect eagerly at daemon startup via `mcp.ConnectAll`) doesn't apply
here — a language server can be much heavier to start (gopls indexing a
whole module cache) and most sessions never touch most languages a
workspace might contain. Starting only what's actually asked for keeps
daemon startup itself free of that cost entirely.

### A broken or missing language server costs only its own tools

Same contract `mcp.Connect` already established: a failed handshake or a
missing binary never brings Kram down, it just makes `lsp_diagnostics`/
`lsp_definition`/`lsp_references` report "LSP capability unavailable for
`<language>`: `<reason>`" as plain text, which the model can read and fall
back to grep from.

### Own transport, not MCP's — despite both being hand-rolled JSON-RPC

LSP uses `Content-Length`-framed messages over stdio; MCP's stdio
transport is newline-delimited JSON. The dispatch-by-request-id pattern
is shared conceptually between `internal/lsp` and `internal/mcp`, but the
two packages don't import each other — the framing difference means the
transport layer genuinely isn't reusable, and forcing a shared
abstraction over two protocols that happen to both be JSON-RPC would have
been the "more generic than the problem needs" mistake this project
otherwise avoids.

---

## Session search

### A distinct concern from `memory_write`, on purpose

`session_search` answers "where did we actually discuss X" over real
conversation history; `memory_entries`/`memory_fts` stays exactly what it
was — facts the model deliberately chose to remember. Neither replaces
the other.

**Why keep them separate rather than one smarter memory system:** a fact
nobody thought to `memory_write` about is common (most of a conversation
never gets curated), and conflating "what was said" with "what was
decided worth remembering" would make memory noisy in exactly the way its
size cap and curation discipline (see Memory, above) exist to prevent.

### Only `user`/`assistant` messages are indexed — structurally, not by filtering results

The FTS5 trigger that keeps `messages_fts` in sync only fires for
`role IN ('user', 'assistant')` with non-empty content. Tool output and
`system` rows (the system prompt, AGENTS.md injection, and compaction
summaries) are simply never in the index.

**Why structural exclusion instead of filtering compaction summaries out
of search results by name:** a blanket guarantee that doesn't depend on
remembering to check `Name == CompactionMarkerName` in every query path.
A compaction summary can still appear as *context* inside a match's
anchored window (clearly tagged by role), which is legitimate — it's
appearing *as a match*, claiming to be something someone said, that's
the actual failure mode being prevented.

### `subagent: `-prefixed sessions are excluded by default

A subagent's transcript repeating the same terms as the real conversation
it was delegated from would otherwise crowd out the conversation a user
actually wants found. `scope="all"` opts back in explicitly.

---

## Agent loop

### Tool execution waits for a complete response

Streaming and tool execution are decoupled: the loop streams text to the
user as it arrives, but only acts on tool calls once the model's response
is complete.

**Why:** every agent loop we looked at that documents this decision
decouples the two. Acting on a partially-streamed tool call means acting
on possibly-malformed arguments.

### Soft landing on the turn budget

At the last allowed turn, tools are withdrawn and the model is asked
directly for a final answer, rather than being cut off mid-loop.

**Why:** a hard cutoff produces a truncated non-answer. Hermes Agent's
pattern. There's also a grace turn: if the model ignores the wrap-up
request and tries to call tools anyway, it gets exactly one more chance
before the loop stops for real.

### Compaction is capped at 3 attempts per run

Exceeding it fails with `ErrContextOverflow` rather than compacting again.

**Why:** this guards a specific, documented failure mode in other agent
loops — the model treats its own compaction summary as a new task,
executes it, overflows again, and loops forever. The summary is also
stored wrapped as explicitly non-actionable reference material for the
same reason.

### bash is foreground-only — background processes are a separate, explicit tool

No background processes, no servers, no watchers *in `bash` itself*.
Timeout-bounded. Hermes Agent makes the same call for the same reason:
a background process outlives the turn that started it, and nothing in
the loop owns cleaning it up if it's just a stray flag on a generic
shell tool.

**Why not fix this by adding a `background: true` flag to `bash`
instead:** ownership. A flag makes "does this outlive the turn" an
implicit property of arbitrary command text the model wrote, invisible
until something goes looking for it. A separate tool
(`run_background`/`process_list`/`process_output`/`process_kill`,
`internal/daemon/tools/background.go`) makes the decision to start
something long-lived explicit and the resulting process trackable and
killable by id — every process a `processManager` owns, not something
`bash` quietly left running.

Two consequences that follow from making it explicit rather than
implicit: processes are daemon-lifetime, not session-lifetime — a dev
server started from one session is still checkable and killable from a
different one, matching `todoStore`'s pattern rather than being scoped to
whichever conversation happened to start it. And the daemon kills every
tracked process on its own shutdown (`Registry.StopBackgroundProcesses`),
so nothing outlives the daemon as an orphan the way a flag-based approach
would have no natural place to clean up from.

### Images are gated by capability, never assumed

If no provider in the active combo declares `supports_images`, attached
images are dropped before the request and the caller gets an explicit
notice.

**Why:** silently sending a text-only request when the user attached an
image is a wrong answer that looks like a right one.

---

## Memory

### Agent-curated, not auto-captured

The model calls `memory_write` itself. Kram never scrapes conversation
into memory automatically.

**Why:** automatic capture accumulates noise that then needs pruning.
Curated entries stay short and intentional. This is the deliberate
narrowing of [ai-memory](https://github.com/akitaonrails/ai-memory)'s
design.

### A hard per-scope size cap — *reversed decision*

**First version:** memory grew without limit.

**Problem:** memory is injected into the prompt on every turn, so
unbounded memory is an unbounded prompt prefix, forever. "Summarize it
when it gets long" never happens unless something forces it.

**Now:** 2,400 chars per scope. Overflowing doesn't truncate or fail
silently — it returns every current entry with its id and tells the model
to consolidate first, which is why `memory_write` grew `replace` and
`remove`. From Hermes Agent, whose memory files are hard-capped for the
same reason. The cap *is* the design.

### Snapshotted per run, not per turn — *reversed decision*

**First version:** memory was re-read from SQLite on every turn.

**Problem:** the preamble is a prompt *prefix*, and providers cache
prefixes server-side and bill cached input at a fraction of the rate. A
prefix that changes between calls discards that cache on every tool
round-trip — which is exactly where an agent turn makes most of its
calls.

**Now:** snapshotted once per run. Kram freezes per *run* (one user
message and all its tool round-trips) rather than per *session* as Hermes
does, so a fact written mid-conversation still appears on the user's very
next message instead of only in a new session.

### Entry ids are shown in the injected memory

**Why:** `replace`/`remove` need something to target. Without visible
ids, consolidating would require a `memory_search` round-trip first just
to find out what's there — and a consolidation step that costs an extra
call is a consolidation step that doesn't happen.

---

## Subagents

### Zero context inheritance

A subagent starts in a brand-new session knowing nothing about the parent
conversation. It sees only the `goal` and `context` strings passed to it.

**Alternative:** opencode's `task` tool, where delegation creates a child
session within the same context lineage.

**Why Hermes's model instead:** it keeps a delegated task's context
budget independent of however long the parent conversation has run. The
cost is real — the parent must be able to write down everything the child
needs — but that constraint is also a useful forcing function: work that
can't be described in a paragraph probably shouldn't be delegated.

### Depth capped at 1, concurrency at 3

A subagent cannot itself delegate. Both are Hermes's defaults.

**Why:** the failure mode of unbounded delegation is an agent tree that
consumes the entire budget before producing anything. Depth travels
through `context.Context` rather than a parameter threaded through every
tool's `Execute` — only `delegate_task` needs it.

### `delegate_task` blocks until every subtask finishes

**Alternative:** return a handle and poll.

**Why:** it fits Kram's existing synchronous tool contract. Async
delegation would mean building a whole task-status subsystem for v0.

---

## Tools

### Disabled tools are invisible, not just refused

A tool turned off in the settings screen is removed from what the model
is offered, *and* refused if called anyway.

**Why:** the removal is the point — a tool the model can see is a tool it
will try. The refusal is defense in depth, for a model that saw the name
in an earlier turn before it was disabled.

### `ask_question` genuinely blocks the turn

It emits an SSE event and parks the daemon's handler until a *separate*
HTTP call (`POST /sessions/{id}/answer`) delivers an answer.

**Why:** the alternative — returning a placeholder and continuing — means
the model proceeds on an assumption it just said it couldn't make. The
`Asker` interface is injected per-turn via context rather than wired into
the registry at startup, because unlike delegation it needs that specific
turn's live event channel.

### Deterministic output filtering, not LLM summarization

Regex rules keyed on the command that produced the output.

**Why:** an LLM call to compress tool output costs a round-trip, adds
latency, and can hallucinate — on the exact content the model is about to
reason from. Regex can only delete, never invent. Adapted from
OmniRoute's RTK engine.

**The invariant:** preserve patterns are checked before drop rules, so a
line that looks like a failure survives whatever else matches. This is
what `outputfilter_test.go` exists to enforce; a filter that quietly eats
a build error is worse than no filter.

### The all-routine case returns a summary, not the original — *reversed decision*

**First version:** if every line matched a drop rule, return the full
original (the obvious "never return empty" fallback).

**Problem:** caught by the tests on their first run. That's the worst of
both worlds — no signal added, no tokens saved.

**Now:** return the last line plus a count of what was hidden. "It ran and
was unremarkable" is a different claim from "it produced no output", and
both are different from dumping 200 lines of passing tests.

---

## Routing

### Strategy depends on what's in the chain

The auto-built combo picks: no rotation when a paid provider is present,
round-robin when everything is free tier.

**Why:** these two cases want opposite things.

With a paid provider, the catalog order is a *priority* order — it leads
the chain — and prompt-cache economics dominate. Rotating away from it
between tool round-trips re-pays full price for the same prefix, over and
over.

With only free tiers, the providers are interchangeable peers and the
binding constraint is rate limits, not cost. You cannot benefit from a
warm cache on a provider that's answering 429, so proactive spreading
wins.

`prefix-affinity` exists as a third strategy for chains of equivalent
peers where stability matters more than spreading. Its key deliberately
excludes the growing tool-call tail, which would otherwise produce a
different key on every round-trip and defeat the purpose.

### Both Anthropic and Gemini adapters concatenate system messages

**Found as a real bug**, while adding AGENTS.md injection: Anthropic
accepts one top-level `system` field and Gemini one `systemInstruction`,
so a second system message (a compaction summary, say) was silently
clobbering the first. Both now concatenate.

---

## MCP

### The stateful protocol era, not the current one

Implements `initialize` → `notifications/initialized` →
`tools/list`/`tools/call`, the lifecycle used through revision 2025-11-25.
Revision 2026-07-28 removes the handshake entirely for stateless
per-request metadata.

**Why the older one:** essentially every MCP server actually deployed
today still speaks it. Implementing only the current spec would produce a
client that's correct against the document and useless against reality.
`client.go`'s `handshake` marks where a modern probe slots in.

### No SDK

MCP is JSON-RPC 2.0 over stdio or HTTP. Hand-rolled.

**Why:** it preserves the pure-Go, static-binary, no-surprise-dependencies
property for maybe 400 lines of protocol code.

### Tools namespace as `mcp__<server>__<tool>`

**Why:** a server publishing `bash` or `read_file` would otherwise
silently shadow Kram's own, changing what those names do. The prefix also
makes origin visible in the tool-activity log.

### Connecting never blocks startup

A server that fails to start or handshake is logged and skipped.

**Why:** MCP servers are third-party processes. One broken entry in a
config must cost you that server's tools and nothing else.

---

## Permissions

### A policy layer is a different thing from enabled/disabled

`toolsettings` (enabled/disabled) removes a tool entirely. The new
`internal/permission` package sits *inside* "enabled": a tool can be on in
general but still ALLOW/ASK/DENY per specific call, based on its actual
arguments (`bash: allow "go test *", ask "git push*", deny "rm -rf *"`).

**Why not just extend disabled to be argument-aware:** enabled/disabled
answers "does the model ever see this tool"; policy answers "given this
specific call, does it run, get confirmed, or get refused". Conflating them
would mean a tool that's mostly fine but has one dangerous invocation shape
has to be all-or-nothing.

### Approval is not `ask_question`

`ask_question` means "I don't know enough to proceed." A policy-gated
approval means "I know exactly what I want to do, but policy requires
sign-off first." These are told apart deliberately —
`tools.Asker`/`tools.Approver` are separate interfaces, `EventQuestion`/
`EventApproval` are separate SSE event kinds, `/sessions/{id}/answer` and
`/sessions/{id}/approve` are separate endpoints, with separate pending-id
maps on `agent.Service`. Reusing one mechanism for both would make a
policy pause look, to the model and the user, like the agent being unsure —
which it isn't.

### Compatibility default is Allow, always

An install with no `permissions.json` anywhere (global or project) behaves
exactly as Kram did before this feature existed — every rule evaluates
against a default of `Allow` unless a policy file explicitly sets
`"default": "..."`. Shipping a permission engine must never make existing
users start getting asked about things they were never asked about before.

### Precedence: most-specific-match, not first-match

Rules are scored (exact tool + exact pattern > exact tool + wildcard >
glob tool name), and the highest-scoring matching rule wins, ties broken by
declaration order (later wins). "First rule in the file wins" was rejected
because it makes behavior silently depend on file layout — reordering two
unrelated-looking lines could flip what a specific command does.

### A grant persists the exact approved subject, never a widened pattern

Choosing "always" on `bash: git push origin feature/foo` persists exactly
that literal command string as a new Allow rule
(`.kram/permission_grants.json`, project-scoped — an approval in one
project has no business applying to another) — never `bash: allow "*"`. A
policy engine that silently widens what it remembers approving is more
dangerous than one that keeps asking. Grants are evaluated at the same
specificity as an equivalent hand-authored rule and appended after the
configured policy, so at equal specificity they win the tie against a
same-shaped `ask` rule — which is the point: approving something "always"
should stop it asking again for that *exact* case, nothing broader.

A grant earned mid-run takes effect on the *next* run, not immediately —
the `Evaluator` a run is using was already built for that run. This is the
same tradeoff memory snapshotting already made (see Memory, "Snapshotted
per run"): rebuilding policy state mid-turn was rejected for the same
reason rebuilding the tool registry mid-turn was never on the table.

### A fully-denied tool is hidden; a partially-denied one stays visible

If every rule mentioning a tool says Deny (or the global default is Deny
and nothing else mentions it), the tool is dropped from `Definitions()` —
same treatment `toolsettings`-disabled tools get, since a capability the
model can never successfully use shouldn't cost tokens in the schema. A
tool denied only for some patterns (`rm -rf *` denied, everything else
allowed) stays visible: it's still sometimes usable, and the policy is
enforced at call time regardless.

### The policy "subject" is a per-tool best-effort extraction, not a generic mechanism

`bash`'s subject is its `command`; file tools' is `path` (`move_file`'s is
`"old -> new"`); everything else (custom manifest tools, MCP tools,
`delegate_task`) falls back to the raw argument JSON. This is a known
simplification, not a general "tools declare their own policy subject"
interface — building that felt like speculative generality for a v1 where
only bash and the file tools have an obvious single field worth pattern-
matching on. Revisit if a real policy needs fine-grained matching on a
custom or MCP tool's arguments.

---

## Local state

### The config directory is `kram-gateway`, not `kram`

**Found the hard way:** `~/.config/kram` on the development machine was
already in use by a completely unrelated tool. The first write went into
someone else's config directory.

**Why it matters:** short names collide. The repo's actual name doesn't.

### Credentials are a `0600` file, not encrypted

**Why:** there is no key-management story for a local single-user CLI
that would make "encrypted JSON readable only by code that ships the
decryption key" meaningfully safer than file permissions. `gh` and the
AWS CLI make the same call. Pretending otherwise would be security
theater.

### Env vars always win over stored keys

`cmd/kram` loads stored credentials into the environment only where the
real env var is unset.

**Why:** an explicitly exported variable is an explicit instruction. The
stored key is a convenience for the common case, not an override.

---

## Interface

### One left-aligned column, not two-sided chat bubbles — *reversed decision*

**First version:** user messages right-aligned, agent left, like a
messaging app.

**Reverted because:** it read worse. A terminal transcript is a log, not a
conversation UI; the eye wants one column and a tag. User text is
colored distinctly instead, so a turn is still identifiable at a glance.

Worth recording that the *bug fix* underneath it was real and subtle:
`lipgloss.Style.Width()` pads short content with trailing spaces, so
measuring the padded string gave the box width rather than the content
width, and short messages rendered left-shifted inside an invisible box.

### Nothing in the UI is simulated

The footer's latency, fallback trail, breaker states and token counts all
come from the gateway's own counters. The context panel is computed by
the same code path that decides when to compact.

**Why:** a panel that can disagree with reality is worse than no panel. It
also means the two can never drift.

---

## Boundaries

Several things were deliberately not built. Recording them so they don't
get quietly re-litigated.

### No account rotation or provider evasion

A request to build free-tier account rotation with proxy diversification
and user-agent spoofing was declined. A disposable local mock provider
was built instead, which solved the actual need (testing without burning
quota).

**Why:** the purpose of that system is circumventing a provider's terms,
and no framing changes that. The `opencode-zen` entry in the catalog is
the opposite case and worth contrasting: one legitimate account's own key
pointed at a documented public endpoint.

### No proprietary content copied

Three separate times, content that was publicly *visible* turned out not
to be permissively *licensed*:

- `anthropics/claude-code` — "© Anthropic PBC, all rights reserved"
  despite the public repo. Its `frontend-design` skill was not copied; an
  MIT-licensed equivalent was found instead.
- `anthropics/skills` — per-skill mixed licensing. Most Apache-2.0, but
  the document skills are proprietary and one has no license at all.
- `Gitlawb/openclaude` — the source tree contains internal feature-flag
  names, internal env vars, and internal cost-analysis comments, which
  indicates extracted proprietary source rather than an independent
  project. Used only as evidence of what patterns exist, not mined for
  implementation.

**The rule applied:** verify the LICENSE file, not GitHub's detected
label. Public is not permissive.

### Skill licenses are verified per repository

Every bundled skill's license was read from the source repo's LICENSE
file before copying, and each installed copy keeps a `SOURCE` file.
`skill_install` reports a repo's license and warns when there isn't one —
but never judges whether copying is permitted. That's the user's call,
not a regex's.

### No YAML dependency for two fields

Skill frontmatter is hand-parsed.

**Why:** pulling in a YAML parser to read `name` and `description` isn't
worth the dependency. It does handle block scalars (`>` and `|`) —
**added after** a real external skill using `description: >` silently
produced an empty description.

### A weak model's empty final answer gets one retry, then a visible fallback

**Found reproducing a real report:** the user described Kram "stopping
silently" — no error, no quota message, just nothing. The daemon's own
logs showed every turn completing normally, with real telemetry (provider,
latency, token counts) and a persisted assistant message. So the backend
was not failing. The persisted message's content was an empty string.

**What actually happened:** `bash` reported a normal, expected non-zero
exit (`grep` finding no matches) as `[exit error: exit status 1]`. A
free-tier model reading that tool result interpreted "error" literally,
and instead of explaining or retrying, returned a genuinely empty
completion as its final answer — no text, no tool calls. Kram persisted
and displayed exactly that: nothing. Confirmed against the real
combination of providers in use (`openrouter-gptoss`), not just the mock.

**Two fixes, not one:**

1. `bash`'s error framing no longer editorializes. A non-zero exit is
   reported as `[exit code N]`, not `[exit error: ...]` — grep's "no
   matches", diff's "files differ", and a failing test's non-zero exit
   are all *normal outcomes* of running those commands, and telling a
   model "error" when nothing actually broke is what triggered the
   give-up behavior in the first place.
2. The loop itself no longer treats an empty final answer as done. One
   retry, with an explicit system nudge telling the model its previous
   response was empty; if it happens twice in a row, the user gets a
   clear message instead of silence. This is the actual fix for "stops
   silently" — even in the case where a model just does this regardless
   of what triggered it, the user now always sees *something*.

**A second, independent bug found along the way:** `grep` was walking
into `.kram/` — the daemon's own live SQLite database — and returning
binary garbage (control bytes) as search matches, exactly the kind of
confusing tool result that can contribute to a model giving up. `.kram`
is now in the same ignore list as `.git`/`node_modules`, and `grep`
additionally sniffs for a NUL byte in the first 8KB of any file outside
the ignored directories, so an explicit path to some other binary file
doesn't hit the same problem.

`devtools/mock-provider` gained an `-empty-replies N` flag to make this
class of failure reproducible on demand rather than only when a specific
free-tier model happens to misbehave.

---

## Known gaps

Honest list of what Kram still doesn't have, as of this writing:

- ~~Background/async tasks.~~ Closed: `run_background`/`process_list`/
  `process_output`/`process_kill`. Scheduling (Hermes's `cronjob`) is
  still open — nothing triggers a run on a timer.
- ~~Plugins.~~ Closed for the tool-authoring half: `.kram/tools/*.json` /
  `kramhome/tools/*.json` manifests (`{name, description, command,
  schema}`, stdin-in/stdout-out) let anyone add a tool without touching
  Go. Pluggable memory providers or terminal backends (Hermes has both)
  are still open — narrower, lower priority.
- ~~MCP beyond tools.~~ Resources and prompts closed
  (`mcp_resource_list`/`read`, `mcp_prompt_list`/`get` — cross-cutting
  tools taking a server name, not one registered tool per item, since a
  server's resource set is unbounded and can change). Elicitation is
  still deferred — it needs the same turn-pausing plumbing
  `ask_question` required, and no server in real use has forced building
  it yet.
- ~~Test coverage.~~ Closed for now: 7 packages covered beyond the
  agent-loop smoke tests (credentials, toolsettings, providercatalog,
  router, memory store + tool, skills parser, mcp). Still nothing for
  `internal/gateway`, `internal/provider`, or the CLI itself.
- ~~Evals.~~ Closed: `evals/` runs scripted scenarios against the real
  configured provider (same in-process gateway+daemon wiring as
  `cmd/kram`), each a regression test for a specific behavior — several
  traced directly to bugs found by hand earlier in this project. Hard
  scenarios (does a turn ever return truly empty, does grep ever leak
  binary garbage, are core tools registered) must pass regardless of
  model; soft ones (does the model proactively use a skill/memory) are
  informational, since that depends on the configured model's capability,
  not just Kram's own code being correct. First real run against an
  actual provider found a genuine soft-fail: an unmistakable skill
  trigger phrase didn't get the model to call `skill_list`.
- ~~Distribution.~~ Closed for prebuilt binaries: `scripts/build-release.sh`
  cross-compiles `cmd/kram` for linux/darwin × amd64/arm64 plus
  windows/amd64, `.github/workflows/release.yml` runs it on a version tag
  and attaches the archives (plus checksums) to a GitHub release. This
  works with no cross-compiler toolchain and no per-platform build image
  — `CGO_ENABLED=0` — for the same reason cross-compiling has been free
  this whole project: the SQLite driver was chosen pure-Go specifically
  so the daemon's storage layer never needs cgo. Every target in the
  matrix was hand-verified to actually build *and run* before being added
  (not just "cross-compiles without error" — modernc.org/sqlite is large
  enough that a target compiling cleanly isn't a given). A docs site is
  still open — narrower, and README + DECISIONS.md cover the same ground
  for now.

Scheduling, memory-provider/terminal-backend plugins, and MCP elicitation
remain deliberate omissions — narrower than what they'd extend.
Distribution is what remains from
matter most for Kram being trustworthy rather than merely capable.
