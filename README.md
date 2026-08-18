# Kram

**A local-first coding agent runtime, multi-provider LLM gateway, and terminal workspace — built from scratch in Go.**

Kram is a terminal-native coding agent designed to do real work inside a project: inspect code, edit files, run commands and tests, use language servers, delegate independent tasks, remember decisions across sessions, connect to MCP servers, recover workspace state, and route model calls across multiple LLM providers without tying the agent loop to any one vendor.

The normal experience is intentionally simple:

```bash
kram -workspace ~/code/my-project
```

One process starts the complete runtime — gateway, durable daemon, agent loop, and TUI — while keeping those components independently runnable for development or distributed setups.

Kram is built around a few priorities:

- **Reliability over cleverness.** Failures should be visible, bounded, recoverable, and isolated.
- **Local-first state.** Conversations, memory, workspace metadata, artifacts, and operational state stay on the machine running Kram.
- **Provider independence.** The agent talks to one normalized gateway instead of embedding provider-specific behavior throughout the runtime.
- **Real observability.** Routing, context usage, tool activity, latency, fallback, and approvals come from real runtime state — the TUI does not invent telemetry.
- **Token economy.** Prompt-prefix stability, deterministic output filtering, artifact spilling, memory limits, context compaction, and progressive disclosure reduce unnecessary context growth.
- **Explicit control.** Tools can be disabled, permission-gated, approved interactively, or denied before execution.
- **Small operational surface.** The core ships as a single Go binary with `CGO_ENABLED=0` builds.

> [!IMPORTANT]
> **Kram's core is an original implementation written specifically for this repository in Go.** It is not a fork, port, wrapper, source transplant, or repackaging of another coding agent or LLM gateway. The agent loop, routing layer, gateway, daemon, MCP client, LSP client, permission system, persistence wiring, process control, and terminal behavior are implemented here from the ground up.
>
> Kram does use normal third-party Go libraries for infrastructure such as terminal rendering, YAML parsing, and SQLite. Those are dependencies, not reused agent/runtime source code.

For the architectural decisions behind the implementation, including trade-offs, reversals, and deliberately deferred features, see [`DECISIONS.md`](DECISIONS.md).

---

## What Kram is

At a high level, Kram is four things working together:

```text
┌─────────────────────────────────────────────────────────────────────┐
│                              KRAM                                   │
│                                                                     │
│  ┌──────────────┐      ┌────────────────────────────────────────┐   │
│  │ Terminal TUI │─────▶│ Durable daemon + agent runtime         │   │
│  │              │ SSE  │                                        │   │
│  │ sessions     │◀─────│ sessions · tools · memory · context    │   │
│  │ route trace  │      │ delegation · approvals · persistence   │   │
│  │ context      │      └────────────────┬───────────────────────┘   │
│  └──────────────┘                       │ model calls               │
│                                         ▼                           │
│                          ┌──────────────────────────────┐           │
│                          │ Multi-provider LLM gateway   │           │
│                          │                              │           │
│                          │ routing · fallback · gates   │           │
│                          │ circuit breakers · telemetry │           │
│                          └──────────────┬───────────────┘           │
│                                         │                           │
│                         ┌───────────────┼────────────────┐          │
│                         ▼               ▼                ▼          │
│                    Anthropic        OpenAI-style       Gemini       │
│                    providers         providers         providers     │
└─────────────────────────────────────────────────────────────────────┘

Agent tools branch out locally to:

filesystem · shell/processes · git · LSP · MCP · snapshots · artifacts
skills · memory · session search · web fetch · subagents · user approval
```

The separation is deliberate. The **agent loop does not contain provider-specific code**, the **TUI owns no durable state**, and the **gateway does not own conversations**. Each layer has one job and can be tested or run independently.

---

## Why Go

Kram was written from zero in Go because the runtime has unusually strong requirements around lifecycle ownership, long-running processes, concurrency, portability, and failure isolation.

Go gives Kram:

- a single native executable instead of a runtime plus a dependency tree;
- cheap goroutines for gateway, daemon, streams, MCP supervision, LSP clients, and delegated work;
- explicit `context.Context` cancellation through long-running operations;
- straightforward HTTP/SSE and JSON-RPC implementations;
- predictable process ownership and graceful shutdown;
- easy cross-compilation;
- a strong standard library for filesystem, networking, synchronization, and testing;
- low operational complexity for a tool that is supposed to live inside development environments.

The SQLite layer uses `modernc.org/sqlite`, a pure-Go driver. Release builds therefore keep `CGO_ENABLED=0`, allowing the same project to cross-compile for Linux, macOS, and Windows without a C cross-toolchain.

The goal is not “zero dependencies.” The goal is **a small, inspectable application core whose behavior Kram owns**.

---

## Quick start

### Requirements

For building from source:

- Go version declared in [`go.mod`](go.mod) — currently Go 1.26.6;
- at least one configured LLM provider;
- Git is recommended and required for workspace snapshot features;
- language-server binaries are optional and only needed when using LSP tools.

### 1. Configure a provider

The simplest path is an environment variable:

```bash
export ANTHROPIC_API_KEY="..."
# or
export OPENAI_API_KEY="..."
# or
export GEMINI_API_KEY="..."
# or another provider supported by the catalog/configuration
```

Environment variables always take precedence over keys stored by Kram.

### 2. Start Kram

```bash
go run ./cmd/kram -workspace ~/code/my-project
```

Or, once using a release binary:

```bash
kram -workspace ~/code/my-project
```

Kram will:

1. resolve the workspace;
2. create `<workspace>/.kram/` if needed;
3. load provider credentials/configuration;
4. start the gateway and daemon on localhost;
5. wait for both health checks;
6. open the terminal UI;
7. restore access to durable sessions already stored for that workspace.

Gateway and daemon logs are written to:

```text
<workspace>/.kram/kram.log
```

Conversation state is stored in:

```text
<workspace>/.kram/kram-daemon.db
```

### Useful flags

```text
-workspace      project root
-config         explicit gateway YAML configuration
-strategy       routing strategy for the auto-detected combo
-model          gateway combo used by the session
-session        resume a specific session ID
-title          create/open directly with a new session title
-max-turns      maximum model-call budget for one agent run (default 50)
-gateway-port   explicit gateway port; 0 chooses a free localhost port
-daemon-port    explicit daemon port; 0 chooses a free localhost port
-version        print Kram version
```

For example:

```bash
go run ./cmd/kram \
  -workspace . \
  -strategy smart \
  -max-turns 50
```

---

# Feature overview

## 1. A real tool-calling agent loop

Kram is not a chat proxy that makes one model request per message. Each user turn can become a complete agent run:

```text
user request
    │
    ▼
model call
    │
    ├─ final answer ──────────────────────────────▶ done
    │
    └─ tool calls
          │
          ▼
      execute tools
          │
          ▼
      persist results
          │
          └──────────────────────────────────────▶ next model call
```

The loop persists the user message before work begins, streams visible text to the client, executes completed tool calls, feeds their results back to the model, and continues until a final answer is produced or a safety budget is reached.

### Important loop behavior

- **Tool calls are executed only after the model response containing them is complete.** Kram never executes half-streamed arguments.
- **Default run budget: 50 model calls.** Tool round-trips count toward the same budget.
- **Soft landing at the limit.** On the final allowed call, tools are withdrawn and the model is explicitly asked to provide its best final answer rather than being cut off mid-task.
- **Empty-answer recovery.** A genuinely empty final model response receives one explicit retry; a second empty response becomes a visible diagnostic instead of silent failure.
- **Usage is aggregated across the whole run**, not only the last model call.
- **Routing trace is accumulated across the whole run**, including model calls made before and after tools.
- **Tool results and assistant tool-call messages are durable**, so the history reflects what actually happened.

---

## 2. Multi-provider gateway and Combos v2

The gateway gives the rest of Kram one normalized OpenAI-style surface while provider adapters handle the actual upstream wire formats.

Current adapter families:

- **Anthropic** — native Messages API translation;
- **Gemini** — native Gemini content/function-call translation;
- **OpenAI-compatible** — OpenAI itself plus compatible gateways and endpoints configured by the user.

Tool calls are normalized through Kram and translated to/from the provider-specific representation. Provider capability declarations are explicit: tool support and image support are routing constraints, not guesses.

### Combos

A **combo** is a named pool/fallback chain of providers. The incoming OpenAI `model` field selects a combo, with `default_combo` as fallback.

Combos v2 separates three concerns:

```text
COMBO
  │
  ▼
ROUTE STRATEGY
  │ ranks eligible candidates
  ▼
ATTEMPT EXECUTOR
  │ calls providers in ranked order
  ▼
RESPONSE / STREAM GATE
  │ accepts or rejects technical result
  ├─ accept ──▶ client
  └─ reject ──▶ next candidate, while fallback is still possible
```

Circuit-open and capability-incompatible providers are removed **before** scoring. A strategy cannot “score around” a hard constraint.

### Routing strategies

| Strategy | Purpose |
|---|---|
| `priority` | Keep the configured provider order. Predictable and cache-friendly. |
| `round-robin` | Rotate the leading candidate to distribute calls across peers. |
| `prefix-affinity` | Deterministically keep a stable prompt prefix on the same healthy provider. |
| `smart` | Balanced weighted routing across health, reliability, latency, quality hint, cache affinity, and priority. |
| `quality` | Weighted preset that emphasizes explicit quality hints and reliability. |
| `fast` | Weighted preset that emphasizes observed latency while still considering health. |
| `reliable` | Weighted preset that strongly favors observed success and health. |
| `cheap` | Cost-conscious preset using configured provider priority as the operator's cost preference. Kram does not fabricate price telemetry. |
| `weighted` | Fully configurable version of the weighted engine. |
| `lkgp` | Prefer the last known good eligible provider. |
| `p2c` | Power-of-two-choices style selection for a small, cheap balancing decision. |

The weighted family uses one scoring engine rather than separate implementations for each preset. Custom weights are normalized automatically.

### Smart routing signals

The current weighted engine can use:

- circuit-breaker/health state;
- observed provider success rate;
- observed average latency;
- explicit operator `quality_hint`;
- prompt-prefix/cache affinity;
- declared combo priority;
- last-known-good boost;
- run stickiness;
- bounded exploration for gathering telemetry from non-leading candidates.

Kram intentionally does not invent quality, price, quota, or latency data it does not actually have.

### ResponseGate

A transport-level HTTP success is not always a useful model response. Combos may enable deterministic response validation such as:

- reject empty output;
- require a terminal completion signal;
- require a minimum text length;
- reject configured substrings used by upstreams to disguise technical failures inside HTTP 200 responses.

This gate is for **technical validity**, not semantic judgment. It is not intended to route around legitimate model refusals or policy behavior.

### Streaming fallback

For streaming requests, Kram performs a small bounded peek before committing downstream SSE output. Empty role-only chunks, keepalives, immediate errors, and streams that fail before meaningful output can still fall through to another provider.

Once meaningful model content has been committed to the downstream client, the same HTTP response cannot safely switch providers. Kram therefore treats **pre-commit fallback** and **post-commit stream handling** as different lifecycle stages.

---

## 3. Circuit breakers and provider isolation

Every provider has independent breaker state.

Current breaker behavior:

- 3 consecutive failures open the circuit;
- open providers are skipped during routing;
- after a 30-second cooldown, recovery is probed through the half-open state;
- a successful attempt closes and resets the circuit;
- a failed half-open attempt reopens it.

The purpose is simple: one degraded upstream should not be hammered repeatedly and should not prevent healthy candidates from receiving work.

The gateway also exposes real per-provider telemetry: request count, failure count, token usage, average latency, success rate, capabilities, and breaker state.

---

## 4. Durable sessions and local-first persistence

The daemon owns conversation durability. The TUI is only a view.

Sessions and messages are written to SQLite before the caller is told the operation succeeded. Closing the terminal does not delete the conversation, and restarting the daemon does not erase history.

The store also backs:

- persistent cross-session memory;
- full-text session search;
- compaction summaries;
- tool-call history;
- provider attribution on assistant messages.

The database lives inside the workspace, so project history follows the project rather than a remote account.

---

## 5. Built-in toolset

A normal daemon with persistence available registers **34 core tools** before custom tools and MCP-provided tools are added.

They are grouped by purpose below.

| Area | Tools | What they are for |
|---|---|---|
| Files | `read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `move_file`, `delete_file` | Inspect and modify the workspace with deterministic file operations. |
| Shell & processes | `bash`, `run_background`, `process_list`, `process_output`, `process_kill` | Run bounded foreground commands or explicitly manage daemon-owned background processes. |
| Git | `git_status`, `git_diff` | Give the model read-only visibility into repository state and changes. |
| Web | `web_fetch` | Fetch HTTP(S) resources with bounded output for reference material. |
| Planning | `todo_write`, `todo_read` | Persist a project-level task list for multi-step work. |
| Human interaction | `ask_question` | Pause the agent run and wait for a real user answer instead of guessing. |
| Delegation | `delegate_task` | Fan independent work out to isolated subagents. |
| Skills | `skill_list`, `skill`, `skill_install` | Discover, load, and install reusable instruction packages with progressive disclosure. |
| Artifacts | `artifact_read` | Read bounded slices of oversized output previously spilled to disk. |
| Code intelligence | `lsp_diagnostics`, `lsp_definition`, `lsp_references` | Use language-server semantics instead of relying only on text search. |
| Workspace recovery | `snapshot_create`, `snapshot_list`, `snapshot_diff`, `snapshot_restore` | Capture and restore explicit workspace snapshots. |
| Memory & history | `memory_write`, `memory_search`, `session_search` | Preserve curated knowledge and search real historical conversations. |

### File safety

Structured file tools resolve user-supplied paths against the workspace root and reject paths that escape it.

The shell is intentionally different: it starts with the workspace as its working directory, but it is a real operating-system shell, **not a filesystem sandbox**. Shell risk is controlled through visibility, timeout/process ownership, and the permission engine. If stronger OS isolation is required, Kram should be run inside an appropriate container/VM/sandbox rather than pretending `cwd` is a security boundary.

---

## 6. Cross-platform shell and process-tree ownership

All command-running features share `internal/shell` instead of each inventing its own `exec.Command` behavior.

### Unix

Commands run through `/bin/sh -c` in their own process group. Cancellation/kill targets the process group so children are not silently orphaned; shutdown can escalate from graceful termination to kill when necessary.

### Windows

Commands run through `cmd.exe /S /C` (resolved through `COMSPEC`) and are attached to a Windows Job Object with kill-on-close semantics. Kram does not assume Git Bash, WSL, or a POSIX shell exists on Windows.

This layer is used by foreground shell calls, background processes, and manifest-defined custom tools.

`bash` itself remains foreground-only with a default 30-second timeout and a 120-second maximum. Long-running servers/watchers belong in the explicit background-process tools so Kram can track and terminate them later.

---

## 7. Permission engine: ALLOW / ASK / DENY

Tool availability and tool permission are separate concepts.

A tool may be enabled but still require approval for a particular operation. Every built-in, manifest-defined, and MCP-backed tool call flows through the same permission evaluator before execution.

Decisions are deterministic:

- `allow` — execute immediately;
- `ask` — pause the run and ask the user;
- `deny` — refuse without executing.

Rules may target exact tool names, MCP prefixes, and operation subjects such as a shell command or file path. More-specific matching wins over broader matching.

Example project policy:

```json
{
  "default": "allow",
  "rules": [
    {"tool": "bash", "pattern": "git push*", "decision": "ask"},
    {"tool": "delete_file", "pattern": "*", "decision": "ask"},
    {"tool": "mcp__github__*", "pattern": "*", "decision": "ask"}
  ]
}
```

Project policy:

```text
<workspace>/.kram/permissions.json
```

Global policy:

```text
~/.config/kram-gateway/permissions.json
```

When the user chooses **always**, Kram persists an allow grant for the exact approved subject rather than silently widening it to a wildcard.

A fully denied tool is removed from model-visible definitions entirely; a partially denied tool remains visible because some calls are still valid.

---

## 8. Interactive questions and approvals

Some decisions should not be guessed by the model.

`ask_question` emits a live SSE event and genuinely parks the current agent run until a separate answer arrives. Permission `ask` decisions use a separate approval channel with `once`, `always`, and `deny` options.

Questions and approvals are intentionally distinct mechanisms:

```text
ask_question  -> the model needs information
approval      -> policy requires authorization
```

Both waits are bounded. Approval timeout fails closed rather than silently becoming permission.

The TUI renders both flows directly inside the active turn.

---

## 9. Context management and compaction

Coding agents can accumulate context quickly because every tool result becomes part of the next model call. Kram manages this deliberately instead of waiting for an upstream context-window error.

The current compaction path is tiered:

1. build effective history;
2. structurally prune old/redundant tool output first;
3. only if still necessary, ask the model for a compact summary;
4. persist that summary as explicitly non-actionable reference context;
5. reload the reduced effective history.

A run is allowed at most three compaction attempts by default. If the context keeps overflowing, Kram returns a real `ErrContextOverflow` instead of entering an unbounded summarize/retry loop.

The TUI's context panel reports the same categories the daemon uses to reason about context, including conversation content, tool definitions, project context, memory/compaction material, and remaining budget.

---

## 10. Deterministic tool-output filtering

Raw command output is often mostly progress noise. Sending every line of a compiler, package manager, or test runner back through every later model call wastes tokens and makes signal harder to find.

Kram applies deterministic, command-aware filtering to inline shell output:

- no extra LLM call;
- no summarization hallucination;
- preserve/error patterns are evaluated before drop patterns;
- an all-routine result still returns a small truthful summary rather than becoming empty.

The filter can only remove known noise. It does not invent replacement output.

---

## 11. Artifact store and bounded command output

Large outputs should not consume unbounded RAM or disappear through truncation.

Foreground shell and custom-tool output use a spill writer directly as process stdout/stderr. Inline output is bounded; once the threshold is crossed, the complete stream is written to the workspace artifact store while memory use stays bounded.

The model receives:

- a short preview;
- an artifact ID;
- metadata sufficient to identify the stored result;
- access through `artifact_read` in bounded slices.

Artifact access accepts Kram-generated IDs, not arbitrary filesystem paths.

Artifacts are local workspace state and old artifacts receive best-effort garbage collection at daemon startup.

The agent loop separately enforces a combined per-turn tool-output budget so many individually reasonable results cannot explode context in one model turn.

---

## 12. Persistent memory

Conversation history and memory are deliberately different.

History answers:

> What was actually said in this session?

Memory answers:

> What fact or decision is worth carrying into future sessions?

Kram memory is **agent-curated**, not automatic conversation scraping. The model uses `memory_write` when a fact, preference, or decision is durable enough to keep.

Memory supports:

- **project scope** — tied to the current workspace;
- **global scope** — available across projects;
- FTS5 search through `memory_search`;
- pinned/recent automatic injection at the beginning of a run;
- replace/remove operations for consolidation;
- a hard per-scope size cap to prevent memory from becoming an unbounded prompt prefix.

Recent memory is frozen once per user run so tool round-trips keep a stable prompt prefix. Newly written memory becomes available on the next user run.

---

## 13. Cross-session conversation search

`session_search` searches what users and assistants actually said across durable sessions, even if nobody promoted that information into memory.

It uses deterministic SQLite FTS5/BM25 retrieval rather than an LLM query.

By design:

- user and assistant text is indexed;
- system messages and tool output do not become primary search matches;
- delegated subagent sessions are excluded by default to avoid drowning real conversation history in repeated worker transcripts;
- callers can opt into the wider scope when needed;
- matches include surrounding context so the result is useful, not just a disconnected line.

This gives Kram both **curated memory** and **historical recall** without conflating the two.

---

## 14. Project context

Kram looks for project-level instructions in the workspace root and injects them into the run preamble.

Supported root files currently include:

```text
AGENTS.md
CLAUDE.md
```

Project context is re-read instead of permanently copied into conversation history, so changing the file affects subsequent work without requiring the session to be recreated.

The model preamble is assembled roughly as:

```text
Kram system rules
    + project context
    + persistent memory snapshot
    + effective conversation history
```

Keeping this prefix deliberate and stable matters for both model behavior and upstream prompt caching.

---

## 15. Subagents and parallel delegation

`delegate_task` lets the main agent split independent work into parallel subtasks.

A delegated task starts in a fresh session with **zero inherited conversation history**. It only sees the explicit goal and context supplied by the parent. This keeps the worker's context budget independent from a potentially long parent conversation and forces delegation to carry a clear brief.

Current safeguards:

- up to 3 concurrent subagents by default;
- delegation depth capped at 1;
- subagents cannot recursively build an unbounded agent tree;
- each delegated task may select a different gateway combo/model;
- the parent tool call waits for workers and receives their consolidated results.

Subagents currently share the same workspace. They are context-isolated, not filesystem-isolated.

---

## 16. Skills with progressive disclosure

Skills are reusable instruction packages stored as a directory containing `SKILL.md`.

Kram discovers project and global skills, but does not inject every skill body into every prompt. Instead:

1. `skill_list` exposes cheap metadata — name and description;
2. `skill` loads full instructions only when needed;
3. `skill_install` can discover/install skills from a public Git repository and reports source/license information.

Project skills:

```text
<workspace>/.kram/skills/<skill>/SKILL.md
```

Global skills:

```text
~/.config/kram-gateway/skills/<skill>/SKILL.md
```

This progressive-disclosure model keeps specialized knowledge available without permanently taxing the context window.

Skills can also be disabled from the same settings system used for tools.

---

## 17. Custom tools without recompiling Kram

Project and global JSON manifests can add tools without writing Go or rebuilding the binary.

Locations:

```text
<workspace>/.kram/tools/*.json
~/.config/kram-gateway/tools/*.json
```

Example:

```json
{
  "name": "uppercase",
  "description": "Uppercase the supplied text.",
  "command": "python3 -c \"import json,sys; d=json.load(sys.stdin); print(d['text'].upper())\"",
  "schema": {
    "type": "object",
    "properties": {
      "text": {"type": "string"}
    },
    "required": ["text"]
  }
}
```

The tool-call JSON is sent to the command on stdin; stdout becomes the result. Manifest tools share Kram's shell runner, output limits/artifact handling, tool settings, and permission path.

A manifest cannot override a built-in tool name. Project manifests take precedence over global manifests with the same custom name.

This keeps extension simple and preserves the static Go binary instead of loading fragile in-process native plugins.

---

## 18. MCP client

Kram includes its own MCP client implementation in Go. It does not require an MCP SDK inside the runtime.

Supported capabilities include:

- stdio servers;
- Streamable HTTP transport;
- initialization/lifecycle handling;
- dynamic `tools/list` and `tools/call`;
- resources list/read;
- prompts list/get;
- project and global MCP configuration;
- server isolation — one broken server does not prevent Kram from starting;
- transport reconnect supervision with bounded exponential backoff;
- `tools/list_changed` refresh;
- on-disk schema snapshots keyed by connection-config fingerprint.

Remote tools are namespaced as:

```text
mcp__<server>__<tool>
```

so a remote server cannot silently replace a built-in name such as `bash` or `read_file`.

When MCP servers are connected, Kram also exposes generic tools for server resources and prompts:

```text
mcp_resource_list
mcp_resource_read
mcp_prompt_list
mcp_prompt_get
```

MCP connections are external trust boundaries. Their tools still pass through Kram's common tool registry and permission evaluator before execution.

---

## 19. LSP code intelligence

Text search is useful, but a coding agent also benefits from semantic code navigation.

Kram contains a small LSP client implementation over the protocol's `Content-Length` framed JSON-RPC transport. Language servers start **lazily** — not at daemon startup — and one server is reused per language.

Current agent-facing capabilities:

- diagnostics;
- go-to-definition;
- references.

The manager has defaults for common Go, TypeScript/JavaScript, and Python language servers and can be extended through project configuration.

If a language server is missing or fails to initialize, Kram keeps running. Only that language's LSP capability is unavailable; the agent can fall back to regular file/search tools.

Language servers started by Kram are closed on daemon shutdown.

---

## 20. Workspace snapshots and restore

Kram can explicitly snapshot the workspace before risky work and later inspect or restore that state.

The snapshot engine uses a **separate, isolated Git repository** under Kram state, with the real workspace as its work tree. It never points snapshot commands at the user's own `.git` directory, index, branch, or HEAD.

Available operations:

```text
snapshot_create
snapshot_list
snapshot_diff
snapshot_restore
```

The snapshot store:

- respects the project's `.gitignore`;
- always excludes `.git` and `.kram` from snapshot history;
- reports files affected by restore;
- leaves files that were never captured by the snapshot system alone;
- degrades cleanly when the system `git` executable is unavailable.

Snapshots are currently **explicit**, not automatically created before every mutation. `snapshot_restore` is a destructive operation and can be permission-gated like any other tool.

---

## 21. Background processes

Long-running work is deliberately separate from foreground shell execution.

`run_background` starts a tracked daemon-owned process. The agent can later inspect or stop it through:

```text
process_list
process_output
process_kill
```

Background processes are daemon-lifetime rather than session-lifetime. That allows a server started in one conversation to be inspected from another session in the same workspace/runtime.

The daemon kills tracked process trees on shutdown so a dev server does not silently outlive the process that owns it.

---

## 22. Terminal UI designed around real runtime state

The TUI is implemented with Bubble Tea/Lip Gloss and talks to the daemon/gateway over their real APIs. It does not persist conversations itself and does not call providers directly.

### Transcript

- Kram responses remain left-aligned and render completed Markdown.
- Submitted user messages are shown as a compact right-aligned **prompt block** with a slim accent, not a messaging-app bubble.
- The active composer is a 3-row word-wrapping textarea, so long prompts remain visible instead of scrolling horizontally off-screen.
- Responses stream incrementally.
- Tool calls appear while they are running and settle into success/failure state when their result arrives.
- Notices, questions, and approval prompts are integrated into the turn.
- Mouse-wheel transcript scrolling is supported.

### Route bar

A one-line route bar sits above the transcript and reports the active strategy and the real routing trail once a model call completes.

On wide terminals it can show provider names, outcome glyphs, and latency. It degrades to more compact forms on narrower terminals without allowing long provider IDs to wrap the layout.

While a model call is in flight, the bar shows a generic routing state rather than fabricating which internal fallback attempt is currently active.

### `Ctrl+R` — full route trace

Shows the routing story for the most recently completed user run:

- every model call;
- every upstream attempt;
- provider;
- latency;
- outcome;
- technical rejection/error reason;
- selected winner;
- aggregate counts and provider time.

This is important because a real agent turn may make several model calls around tools; showing only the last call would hide most of the routing behavior.

### `Ctrl+P` — strategy explainability

For scoring strategies, the panel renders the **router's actual ranking data**, including factor weight, value, contribution, total score, and reasons such as sticky/LKGP/cache affinity/exploration.

The TUI does not recalculate the score independently, so display logic cannot drift into a second implementation of routing.

### `Ctrl+T` — context panel

Shows context usage and budget information sourced from the daemon. The context indicator is also clickable in mouse mode.

### Session picker and settings

Launching without `-session` opens the durable session picker.

From the picker:

- `a` opens provider/account management;
- `f` opens tool/skill enable-disable settings;
- arrow keys navigate;
- `Enter` resumes or creates a session;
- `Ctrl+C` exits.

The accounts screen can store provider keys locally, use supported OAuth flows, and run real lightweight connectivity/auth checks. Status dots reflect actual ping results rather than decorative state.

---

## 23. Provider credentials

Provider keys may come from environment variables or Kram's local credential store.

Environment variables **always win**. Stored credentials only fill variables that are otherwise unset.

The local store is:

```text
~/.config/kram-gateway/credentials.json
```

It is written with `0600` permissions and the containing directory with restrictive permissions.

Credentials are **not encrypted at rest**. This is intentional: Kram has no separate local key-management system that would make application-level encryption with a bundled decryption secret meaningfully stronger. Treat the file like other local CLI credential files and protect the user account/filesystem accordingly.

---

## 24. Provider health checks

The accounts UI can perform lightweight provider connectivity/auth checks without making a full agent request.

Checks run when entering the accounts screen and can be refreshed manually. They are designed to answer questions such as:

- is the endpoint reachable?
- is this credential accepted?
- is provider setup obviously broken before an agent run starts?

This status is separate from gateway runtime telemetry and circuit-breaker history.

---

# Routing configuration

For normal use, Kram can auto-detect configured provider credentials and build a default combo automatically.

When only free-tier peers are available, the auto path favors distribution. When a paid provider is present, it favors stable priority to preserve prompt-prefix/cache economics. `-strategy` can override the auto choice without requiring a full YAML file.

For complete control, pass `-config`.

## Example modern config

```yaml
host: 127.0.0.1
port: 20128

providers:
  - id: anthropic
    kind: anthropic
    api_key_env: ANTHROPIC_API_KEY
    model: claude-sonnet-4-5
    supports_tools: true
    supports_images: true
    quality_hint: 0.95

  - id: openai
    kind: openai-compat
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
    model: gpt-5
    supports_tools: true
    supports_images: true
    quality_hint: 0.95

  - id: gemini
    kind: gemini
    api_key_env: GEMINI_API_KEY
    model: gemini-2.5-pro
    supports_tools: true
    supports_images: true
    quality_hint: 0.90

combos:
  - id: default
    strategy: smart
    providers: [anthropic, openai, gemini]

    strategy_options:
      sticky: true
      lkgp_boost: 0.10
      exploration: 0.03
      weights:
        health: 30
        reliability: 20
        latency: 15
        quality: 15
        cache_affinity: 15
        priority: 5

    response:
      reject_empty: true
      require_terminal: true
      min_content_length: 8

default_combo: default
```

`quality_hint` is an explicit operator signal. Kram does not infer model quality by pretending it has a benchmark result it never measured.

An absent `response` block preserves the permissive compatibility behavior. An absent `strategy_options` block uses strategy defaults.

See [`config.example.yaml`](config.example.yaml) for the repository example configuration.

---

# Gateway HTTP API

The gateway can be used independently of the built-in agent runtime.

### `POST /v1/chat/completions`

OpenAI-compatible chat-completions surface with streaming and non-streaming support plus Kram routing extensions on completed responses/chunks.

Example:

```bash
curl http://127.0.0.1:20128/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "default",
    "messages": [
      {"role": "user", "content": "Explain this API in one paragraph."}
    ],
    "stream": false
  }'
```

### `GET /admin/status`

Returns real gateway state including:

- provider IDs/kinds;
- capabilities;
- circuit-breaker state;
- request/failure counts;
- prompt/completion token totals;
- average latency;
- success rate;
- configured combos and strategies.

### `GET /health`

Liveness endpoint used by the all-in-one launcher and external supervision.

---

# Daemon HTTP API

The daemon is Kram's durable local control surface.

Current endpoints:

```text
GET  /health
POST /sessions
GET  /sessions
GET  /sessions/{id}
GET  /sessions/{id}/context
POST /sessions/{id}/messages
POST /sessions/{id}/answer
POST /sessions/{id}/approve
GET  /tools
```

`POST /sessions/{id}/messages` responds over SSE and emits the live agent lifecycle:

```text
delta
route_start
route_done
tool_start
tool_result
notice
question
approval
done / error
```

The final `done` event carries the persisted assistant message, usage, tool activity, compaction count, routing trace, and image capability notice.

---

# Running components separately

The all-in-one `cmd/kram` path is the recommended user experience, but each layer remains independently runnable.

### Gateway

```bash
go run ./cmd/gateway -config config.yaml
```

### Daemon

```bash
go run ./cmd/daemon \
  -db ./kram-daemon.db \
  -gateway http://127.0.0.1:20128 \
  -workspace .
```

### CLI

```bash
go run ./cmd/cli \
  -daemon http://127.0.0.1:20130 \
  -gateway http://127.0.0.1:20128
```

This separation is useful for development, debugging, or setups where gateway and agent runtime should live on different machines/processes.

---

# Local state layout

Kram intentionally keeps project state visible rather than hiding it in a remote service.

Typical workspace state:

```text
<workspace>/.kram/
├── kram.log
├── kram-daemon.db
├── todos.json
├── permissions.json             # optional project policy
├── permission_grants.json       # exact persistent approvals
├── mcp.json                     # optional project MCP config
├── lsp.json                     # optional project LSP config
├── artifacts/                   # spilled large output
├── snapshots/                   # isolated snapshot repository/state
├── skills/                      # project skills
└── tools/                       # custom tool manifests
```

Global/user configuration lives under:

```text
$XDG_CONFIG_HOME/kram-gateway/
```

falling back to:

```text
~/.config/kram-gateway/
```

This includes credentials, global tool settings, global policy/configuration, skills, tool manifests, MCP configuration, and MCP schema cache where applicable.

---

# Reliability model

Kram treats “never crash” as an engineering direction, not a magic promise. The code tries to reduce the blast radius of failures and make failure states explicit.

Examples:

- gateway and daemon HTTP handlers recover panics instead of taking the whole process down;
- providers have independent circuit breakers;
- routing can fall through candidates before response commitment;
- MCP server failure is isolated to that server;
- missing LSP server disables only that semantic capability;
- background processes and LSP servers are cleaned up during daemon shutdown;
- command timeouts kill process trees rather than only the immediate shell where supported;
- output growth is bounded and oversized results spill to artifacts;
- agent turns have iteration and compaction budgets;
- empty model output cannot silently complete a turn twice;
- disabled/fully-denied tools are hidden from the model;
- approval timeout denies rather than allowing;
- durable messages are persisted by the daemon instead of trusting terminal state;
- snapshots provide an explicit recovery mechanism for workspace changes.

The result is not that failures disappear. It is that Kram attempts to make them **contained, observable, and recoverable**.

---

# Security and trust boundaries

Kram is an agent that can execute developer tools. That means its security model should be explicit.

### Structured file tools

Kram rejects file-tool paths that escape the configured workspace.

### Shell

The shell is a real host shell started in the workspace directory. It is not an OS sandbox and can do whatever the host user and configured permission policy allow. Use containers/VMs/OS sandboxing when untrusted code requires stronger isolation.

### Tool permissions

ALLOW/ASK/DENY policy is evaluated before all registered tool execution paths, including custom tools and MCP tools.

### MCP

MCP servers are external code/services. Namespacing prevents tool-name shadowing, but an enabled and approved remote tool still acts with whatever capabilities its server has.

### Skills and fetched content

Skills, project files, command output, web content, and external resources are data/instructions supplied to an agent. Their provenance matters. Kram does not make untrusted external content magically safe.

### Credentials

Stored API keys rely on local filesystem permissions, not application-managed encryption.

### Snapshots

Snapshots use an isolated Git directory and do not intentionally mutate the user's own repository metadata. Snapshot creation/restoration is explicit; it is not a full filesystem backup system.

---

# Testing

The repository uses regular Go tests plus higher-level model evals.

## Unit/integration suite

```bash
go test ./... -race
go vet ./...
go build ./...
```

CI also checks `gofmt` cleanliness.

The test suite includes coverage for areas such as:

- routing strategies and weighted scoring;
- response and stream gates;
- route traces and TUI rendering;
- circuit-breaker interaction;
- permission policy/grants;
- artifacts/spill behavior;
- workspace snapshots;
- shell process-tree cleanup;
- LSP transport/client/manager behavior;
- MCP JSON-RPC, lifecycle, caching, and reconnect behavior;
- session/memory FTS5 search;
- tool boundaries and output filtering;
- eval-harness correctness.

## Model evals

```bash
go run ./evals
```

Evals run through the real in-process gateway + daemon stack using an actual configured model. They are for behavior that ordinary unit tests cannot prove — for example whether a real model actually uses the capabilities exposed by Kram's prompt/tool contract.

The harness distinguishes:

- **PASS** — behavior was exercised and succeeded;
- **FAIL** — behavior was exercised and violated the scenario;
- **SKIP** — the scenario could not observe the property it was meant to test.

Hard scenarios represent runtime invariants; model-dependent soft scenarios remain diagnostic rather than pretending every model has identical agent behavior.

---

# Building releases

```bash
./scripts/build-release.sh v1.2.3
```

Current release targets:

```text
linux/amd64
linux/arm64
darwin/amd64
darwin/arm64
windows/amd64
```

Release builds use:

```text
CGO_ENABLED=0
```

and embed the version through linker flags.

The build script creates `.tar.gz` archives for Unix-like targets and `.zip` for Windows. GitHub Actions contains separate CI and tagged-release workflows; CI builds, vets, checks formatting, and runs the test suite with the race detector.

---

# Repository map

For readers who want to open the hood:

| Path | Responsibility |
|---|---|
| `cmd/kram` | Recommended all-in-one launcher. |
| `cmd/gateway` | Standalone gateway entry point. |
| `cmd/daemon` | Standalone durable daemon entry point. |
| `cmd/cli` | Standalone terminal client. |
| `internal/daemon/agent` | Tool-calling loop, run lifecycle, questions/approvals, route accumulation. |
| `internal/daemon/store` | SQLite sessions, messages, memory, FTS5 history search. |
| `internal/daemon/tools` | Core tool registry and concrete agent capabilities. |
| `internal/daemon/compaction` | Context pruning and summarization. |
| `internal/router` | Combos v2 strategies, factors, sticky/LKGP state, response/stream gating. |
| `internal/server` | OpenAI-compatible gateway HTTP surface. |
| `internal/provider` | Anthropic, Gemini, and OpenAI-compatible adapters. |
| `internal/breaker` | Per-provider circuit breaker. |
| `internal/telemetry` | Lightweight provider runtime counters. |
| `internal/permission` | ALLOW/ASK/DENY policy and exact grants. |
| `internal/artifact` | Bounded-output spill writer and artifact store. |
| `internal/shell` | Cross-platform shell/process-tree control. |
| `internal/snapshot` | Isolated workspace snapshot engine. |
| `internal/lsp` | Hand-written LSP transport/client/manager. |
| `internal/mcp` | Hand-written MCP JSON-RPC client, transports, reconnect/cache lifecycle. |
| `internal/cli/app` | Bubble Tea terminal UI, route/context panels, accounts, approvals. |
| `internal/credentials` | Local provider-key store. |
| `internal/providercatalog` | Auto-configuration/account catalog. |
| `internal/providerping` | Lightweight account connectivity/auth checks. |
| `internal/toolsettings` | Tool/skill enable-disable persistence. |
| `evals` | End-to-end behavioral evaluation harness. |
| `scripts` | Release/build automation. |
| `DECISIONS.md` | Architectural rationale and known boundaries. |

---

# Current boundaries

Kram deliberately does not pretend to solve every agent-runtime problem today.

A few important boundaries are worth stating clearly:

- shell execution is not a host sandbox;
- subagents share the workspace even though their conversational context is isolated;
- snapshots are explicit rather than automatically taken before every mutation;
- MCP schema caching currently improves persisted knowledge/refresh behavior but does not replace every startup connection with lazy discovery;
- streaming fallback is only possible before downstream response commitment;
- route-bar in-flight state is currently per model call rather than fake per-attempt progress;
- scheduling/cron-style autonomous runs are not part of the current core;
- the context policy layer continues to evolve as output/artifact behavior matures.

These are explicit engineering boundaries, not hidden claims in the UI.

---

# Development philosophy

Kram favors mechanisms that are easy to reason about under failure:

- deterministic filtering before another LLM summarizer;
- explicit process ownership before background shell magic;
- a permission engine before scattered confirmation dialogs;
- one durable owner before multiple clients writing state;
- hard capability constraints before “smart” scoring;
- real trace data before simulated dashboards;
- bounded state before “we'll clean it up later”;
- graceful degradation when optional integrations fail;
- small protocol implementations where owning the wire behavior improves predictability.

When a behavior is important enough to show in the TUI, the preferred architecture is for the runtime to compute it once and the UI to render that truth — not implement another version of the same logic for display.

For the detailed rationale behind those decisions, see [`DECISIONS.md`](DECISIONS.md).

---

# License

Kram is licensed under the [MIT License](LICENSE).

Copyright © 2026 codexmark.
