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

### bash is foreground-only

No background processes, no servers, no watchers. Timeout-bounded.

**Why:** a background process outlives the turn that started it, and
nothing in the loop owns cleaning it up. Hermes Agent makes the same
call. This is a real capability gap versus opencode/Hermes (which have
`process`/`cronjob` tools) and it is deliberate rather than unfinished.

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

- **Background/async tasks and scheduling.** opencode lacks this too;
  Hermes has both (`process`, `cronjob`).
- **Plugins.** opencode has TS/JS custom tools; Hermes has pluggable
  memory providers and terminal backends. Kram is fixed to what's in the
  binary, plus MCP.
- **MCP beyond tools.** Resources, prompts, and elicitation are all
  deferred. Only `tools/*` is implemented.
- **Test coverage.** `outputfilter_test.go` is the only test suite. Every
  other component is verified by manual smoke tests against a mock
  provider, which works but catches nothing automatically.
- **Evals.** No harness that can answer "did this prompt change make the
  agent better or worse".
- **Distribution.** No prebuilt binaries, no docs site.

The first two are deliberate for now. The last three are the ones that
matter most for Kram being trustworthy rather than merely capable.
