<img width="1280" height="640" alt="kram-social-preview" src="https://github.com/user-attachments/assets/bf0bd4aa-4a17-4f73-a145-4b36b1fc146e" />

# Kram

**A local-first coding agent runtime, multi-provider LLM gateway, and terminal workspace — built from scratch in Go.**

> [!NOTE]
> **Status: Public Beta.** Kram is ready for real-world testing while its
> cross-platform behavior and provider compatibility continue to stabilize.
> Read the [current beta scope](PUBLIC_BETA.md), see how to
> [contribute](CONTRIBUTING.md), or [report a problem](https://github.com/codexmark/kram/issues/new/choose).

## Install now / Instale agora mesmo

**Linux / macOS**

```sh
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
```

**Windows** — regular, non-Administrator PowerShell

```powershell
irm https://raw.githubusercontent.com/codexmark/kram-releases/master/install.ps1 | iex
```

**Android / Termux arm64**

```sh
pkg install curl tar coreutils git
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
```

The installers select the correct prebuilt binary and verify its SHA-256
checksum. See the [complete installation guide](#install-now--instale-agora-mesmo-1)
for PATH details, first-run setup, supported targets, and version pinning.

---

Kram is a terminal-native coding agent designed to do real work inside a project: inspect code, edit files, run commands and tests, use language servers, delegate independent tasks, remember decisions across sessions, connect to MCP servers, recover workspace state, and route model calls across multiple LLM providers without tying the agent loop to any one vendor.

The normal experience is intentionally simple:

```bash
kram -workspace ~/code/my-project
```

One process starts the complete runtime — gateway, durable daemon, agent loop, and TUI — while keeping those components independently runnable for development, debugging, or distributed setups.

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

For the detailed architectural record, including trade-offs, reversals, and deliberately deferred work, see [`DECISIONS.md`](DECISIONS.md).

---

# Install now / Instale agora mesmo

Kram is distributed as a single prebuilt binary. The installers download the
correct release for the current platform, verify its SHA-256 checksum, install
it in a user-writable directory, and run `kram -version` before reporting
success. Go and Administrator/root access are not required.

## Linux and macOS

Run in a regular terminal:

```sh
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
kram
```

The default destination is `$HOME/.local/bin/kram`. If the installer reports
that this directory is not on `PATH`, add it to your shell configuration and
open a new terminal:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

## Windows

Open a regular, non-Administrator PowerShell window and run:

```powershell
irm https://raw.githubusercontent.com/codexmark/kram-releases/master/install.ps1 | iex
kram
```

The installer places `kram.exe` under
`%LOCALAPPDATA%\Programs\Kram`, adds that directory to the current user's
`PATH`, and updates the open PowerShell session. It does not require an elevated
shell. If an older terminal was already open before installation, close and
reopen it so it reads the updated user `PATH`.

## Termux on Android arm64

Install the small prerequisites, then use the same verified Unix installer:

```sh
pkg update
pkg install curl tar coreutils git
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
kram
```

Termux is detected explicitly and receives the native
`kram-android-arm64.tar.gz` binary at `$PREFIX/bin/kram`; no `proot-distro`,
Ubuntu container, Node, Python, or Go toolchain is required. Keep projects under
`$HOME` for the supported baseline. Android shared storage has separate
permissions and filesystem semantics.

The first launch opens Kram's setup wizard. Choose a workspace, configure at
least one provider or local OpenAI-compatible server, select a routing strategy
and permission preset, then start a session.

For version pinning, alternate installation directories, supported targets,
and release internals, see [Installing](#installing) below.

---

# The engineering ideas that shaped Kram

Kram did not arrive at its current shape by collecting features until the checklist looked large enough. A lot of the current architecture came from discovering that an obvious implementation worked in the happy path but failed under a real agent workload.

Those failures produced a set of rules that now shape the project.

## 1. One gateway is cheaper than provider logic everywhere

A coding agent performs many model calls inside a single user turn. If every layer knows how Anthropic, Gemini, OpenAI-style APIs, fallback, tool calls, images, streaming, and telemetry work, the entire system becomes provider-specific.

The better boundary was:

```text
agent runtime
     │
     │ normalized requests
     ▼
Kram gateway
     │
     ├── Anthropic adapter
     ├── Gemini adapter
     └── OpenAI-compatible adapter
```

That decision made provider selection, fallback, circuit breaking, capability checks, telemetry, response validation, and routing strategy a gateway concern instead of contaminating the agent loop.

**Result today:** the agent loop is provider-agnostic and can reason about one normalized request/response contract.

---

## 2. Prompt caching is part of routing, not just billing trivia

A tool-calling agent repeatedly resends a large, almost identical prompt prefix:

```text
system prompt
+ project context
+ memory
+ conversation
+ tool definitions
+ growing tool-result tail
```

A naive round-robin policy can rotate providers between those calls and destroy upstream prompt-cache locality. That is especially wasteful when a paid provider is otherwise healthy.

So Kram started treating **prompt-prefix stability as routing state**.

That led to several behaviors:

- paid-provider auto-routing prefers stable priority;
- free-tier peers can still use round-robin because rate limits matter more than cache economics there;
- `prefix-affinity` exists for deterministic cache locality;
- weighted routing can score cache affinity;
- persistent memory is frozen once per user run so the prompt prefix does not mutate between tool round-trips.

**The insight:** the cheapest request is often not the provider with the lowest nominal price; it is the provider that can reuse the context you already paid to send.

---

## 3. Smart routing must begin with hard constraints

Early routing logic is easy to over-generalize into “give every provider a score and choose the highest.” That is wrong if a provider cannot actually perform the request.

Kram now separates **eligibility** from **preference**.

Before scoring, routing removes candidates that are not valid for the request:

```text
candidate pool
    │
    ├─ circuit open?      -> remove
    ├─ tools required?    -> require tools capability
    ├─ images required?   -> require image capability
    ▼
eligible candidates
    │
    ▼
strategy ranking/scoring
```

A high quality score can never override a missing required capability.

**The insight:** intelligence belongs after correctness constraints, not instead of them.

---

## 4. Streaming fallback has a real point of no return

For a buffered response, Kram can try provider A, reject it, and then try provider B before returning anything to the caller.

Streaming is different. Once meaningful bytes from provider A have been sent downstream, switching to provider B would splice two different model responses into one stream.

That produced the bounded-peek design:

```text
provider stream
    │
    ├─ role-only / keepalive / empty chunk
    ├─ immediate error
    ├─ malformed early termination
    │       └─ fallback is still possible
    │
    └─ meaningful output
            └─ downstream commit point
                 fallback is no longer safe
```

**The insight:** “HTTP 200” is not the commit point. Meaningful downstream output is.

Kram therefore treats pre-commit fallback and post-commit stream handling as two different lifecycle stages.

---

## 5. A route is the whole turn, not only the last provider call

A real coding turn may look like this:

```text
model call
  -> read_file
model call
  -> grep
model call
  -> edit_file
model call
  -> go test
model call
  -> final answer
```

Originally, keeping only the latest gateway attempt trail meant every earlier routing decision was silently overwritten. The UI could show something technically true while still hiding most of what happened.

That led to `RouteTrace`: every model call in the user run gets its own ranking and attempt trail, and the complete turn is accumulated before the result is exposed.

**Result today:** `Ctrl+R` can explain the entire routing story of the turn instead of only the final model request.

---

## 6. The UI should render truth, not implement a second router

Routing explainability created another trap: the TUI could independently recalculate scores from provider stats.

That would create two routing implementations:

```text
router score
     versus
TUI reconstruction of router score
```

Eventually they would disagree.

Kram instead makes the router produce the ranking, factor values, contributions, and reasons. The TUI only renders them.

The same rule applies to route progress: if the gateway cannot currently expose which internal attempt is live, the UI shows a generic routing state rather than pretending to know.

**The insight:** observability is only useful when it describes the system that actually made the decision.

---

## 7. Truncating output is not enough if RAM already exploded

A classic command-tool implementation does this:

```text
command stdout
    -> bytes.Buffer
    -> command exits
    -> truncate to 50 KB
```

That bounds what is *reported*, but not what was held in memory while the command ran. A command that produces hundreds of megabytes can still consume hundreds of megabytes before truncation happens.

Kram replaced that pattern with a spill writer attached directly to stdout/stderr.

```text
command output
     │
     ├─ small -> inline result
     │
     └─ large -> artifact file
                  + bounded preview
                  + artifact ID
```

The complete oversized output remains retrievable through `artifact_read`, while producer memory stays bounded.

**The insight:** limits must exist at the producer boundary, not only at presentation time.

---

## 8. Context needs a budget, not optimism

Tool output is the fastest way to destroy an agent context window. One verbose test run, package install, or recursive search can be larger than the useful conversation that preceded it.

Kram ended up using several layers because no single technique solves the whole problem:

- deterministic command-output filtering removes known noise;
- large individual outputs spill to artifacts;
- one context-policy plan allocates prompt, history, response reserve, and aggregate tool output from the same window;
- old tool material is structurally pruned before expensive compaction;
- compaction is capped instead of allowed to recurse forever;
- the final model-call budget has a soft landing rather than an abrupt cutoff;
- truly empty model answers get one retry and then a visible diagnostic.

**The insight:** context management is runtime resource management. Treating it as “the model has a big context window” eventually fails.

---

## 9. Memory and conversation history solve different problems

Automatically treating every old conversation as “memory” produces an ever-growing pile of stale context. Treating memory only as manual notes makes it too easy to lose useful historical information.

Kram split the concepts:

```text
conversation history
    -> what was actually said
    -> searchable with session_search

persistent memory
    -> curated durable facts/decisions
    -> written intentionally with memory_write
```

Memory has project/global scope, a hard size cap, consolidation operations, and a bounded automatic injection slice.

Session history remains independently searchable through SQLite FTS5.

**The insight:** recall and memory are related, but they are not the same datastore or the same prompt policy.

---

## 10. Foreground and background commands should be different capabilities

Allowing `bash` to quietly launch background processes makes lifecycle ownership ambiguous. The agent can start a dev server and later have no reliable way to know whether it is still running, where its output went, or how to stop its process tree.

So Kram keeps `bash` foreground-only and bounded, while long-running work has explicit tools:

```text
run_background
process_list
process_output
process_kill
```

Background processes belong to the daemon lifecycle. The daemon owns their output and kills tracked process trees when it exits.

**The insight:** if the runtime starts a process, the runtime should know that it owns the process.

---

## 11. Cross-platform process cleanup is part of reliability

Killing only the shell process is not enough. Child processes can remain alive after cancellation and create exactly the kind of “ghost dev server” behavior a local agent should avoid.

Kram centralized process execution in `internal/shell`:

- Unix uses process groups so cancellation can target the tree;
- Windows uses `cmd.exe /S /C` plus a Job Object with kill-on-close behavior;
- foreground shell, background jobs, and custom manifest tools use the same execution layer.

**The insight:** portability is not “the code compiles on Windows.” Process ownership has to mean the same thing on every supported platform.

---

## 12. Permission checks need one choke point

A growing agent can accumulate built-in tools, custom tools, MCP tools, background processes, and future extension surfaces. Adding one confirmation dialog inside `bash` does not create a security model.

Kram instead routes every registered tool call through one execution boundary:

```text
model asks for tool
      │
      ▼
Registry.Execute
      │
      ▼
ALLOW / ASK / DENY
      │
      ├─ allow -> execute
      ├─ ask   -> pause for user
      └─ deny  -> refuse
```

An `always` approval is persisted for the exact subject that was approved rather than silently broadening into a wildcard.

**The insight:** policy belongs in the dispatch path, not scattered across tool implementations.

---

## 13. Recovery should never borrow the user's Git state

Using the project's real `.git` index/HEAD for agent snapshots would couple Kram's recovery mechanism to the developer's active branch, staging area, and repository state.

Kram instead maintains a separate snapshot repository under `.kram/snapshots` and uses the project directory only as its work tree.

That means snapshot operations do not intentionally move the user's branch, HEAD, index, or staged changes.

**The insight:** a recovery system should not mutate the state it exists to protect.

---

## 14. Optional intelligence should fail locally

A missing language server should not stop the coding agent. A broken MCP server should not stop unrelated tools. An unavailable provider should not prevent healthy providers from receiving traffic.

This led to a repeated architecture pattern:

```text
optional subsystem fails
        │
        └─ lose that capability
           not the whole runtime
```

Examples:

- LSP servers start lazily and fail per language;
- MCP server failures are isolated per server;
- circuit breakers isolate upstream providers;
- artifact GC is best-effort;
- missing local configuration usually contributes no configuration instead of blocking startup.

**The insight:** graceful degradation is easier to achieve when dependencies are narrow and ownership boundaries are explicit.

---

## 15. Progressive disclosure saves context and improves control

Putting every possible instruction and tool body into every prompt is easy, but expensive.

Kram progressively exposes optional capability:

- skills begin as name + description;
- full `SKILL.md` content is loaded only when needed;
- disabled tools disappear from model-visible definitions;
- fully denied tools are also omitted;
- MCP resources/prompts are exposed through fixed discovery/read tools rather than creating one model tool for every remote item.

**The insight:** capability should be discoverable without permanently becoming prompt baggage.

---

## 16. “Not observed” is not the same as “passed”

The evaluation harness also had to learn a basic testing lesson: a scenario that did not actually exercise the property being tested cannot truthfully be called a pass.

Kram's evals distinguish:

```text
PASS  property exercised and succeeded
FAIL  property exercised and violated
SKIP  scenario could not observe the property
```

That sounds small, but it prevents green-looking results from hiding missing coverage.

**The insight:** reliability starts with being honest about what was actually verified.

---

## 17. One binary does not require one monolith

The user-facing goal was always low operational friction: one command, no manual daemon startup, no coordinating ports in three terminals.

The implementation still keeps gateway, daemon, and CLI as separate architectural components. `cmd/kram` starts gateway and daemon **in-process** and then launches the TUI.

```text
one executable
    │
    ├─ gateway goroutine
    ├─ daemon goroutine
    └─ terminal UI
```

The same components can still run independently through `cmd/gateway`, `cmd/daemon`, and `cmd/cli`.

**The insight:** deployment simplicity and architectural separation are not opposites.

---

## 18. A committed stream is not yet a successful one

The bounded-peek commit point (insight 4) answers *when fallback stops being possible*. It does not answer *whether the request actually succeeded* — and those turned out to be two different moments that the first implementation of Combos v2 conflated.

The original code recorded success — and told the router's Sticky/LKGP state about it — the instant `BoundedPeek` saw a meaningful first delta, before the stream had gone anywhere near its terminal event:

```text
first meaningful delta
     │
     ▼
recorded as success, reported to the router
     │
     ▼
stream continues
     │
     └─ later evt.Err -> too late, success was already reported
```

A provider whose first byte looked fine but then errored mid-stream could still become the sticky/LKGP winner. Separately, if the upstream channel just closed on its own without ever sending a terminal `Done`, the old loop still wrote a bare `data: [DONE]`, indistinguishable on the wire from a normal finish — a truncated answer looked like a clean success.

The fix moves outcome decisions to the one place that actually knows the outcome:

```text
commit
  │
  ▼
forward the stream
  │
  ├─ terminal Done         -> success, breaker/Sticky/LKGP updated
  ├─ explicit evt.Err      -> failure, explicit error chunk
  └─ channel closes,
     no Done ever seen     -> failure, explicit error chunk
```

**The insight:** the moment fallback stops being possible and the moment a request actually succeeded are not the same moment. Reporting success at the first one instead of the second corrupts every piece of state that assumes "success" means "actually finished."

---

## 19. A hard preference needs a harder guard — and its own identity

Smart Sticky (insight 3's "hard constraints" idea, applied to run-level preference) is documented as absolute: once a provider wins a run, nothing should displace it except a real failure. Two real bugs showed that "documented as absolute" and "enforced as absolute" are not the same thing.

First, exploration ran unconditionally *after* Sticky applied its pin — the code comment even claimed exploration "never overrides sticky," but nothing in the ordering actually guaranteed that:

```text
score -> sort -> sticky pins the winner -> exploration still runs
                                              │
                                              └─ can still promote
                                                 a different candidate
```

Second, Sticky's "run" identity was the same stable system+first-user-message hash `prefix-affinity`/cache-affinity routing use. That hash is stable across an *entire conversation*, not just one run — so a later, unrelated user turn that happened to start with the same opening message inherited the previous run's pin instead of getting a fresh initial ranking.

```text
turn 1: "inspect this repository"   -> provider A wins, pinned
turn 2: "now redesign the router"   -> same opening message in history
                                        -> same key -> inherits A's pin
```

Both fixes narrow the guard rather than removing the mechanism: exploration now skips entirely whenever a valid Sticky pin exists, and Sticky gained its own `RunKey` — an opaque ID the daemon generates once per agent run and sends as a header — distinct from the `AffinityKey` that cache-affinity/prefix-affinity still correctly share across a run's tool round-trips.

**The insight:** a hard preference is only as strong as the code path that could still bypass it, and a cache-locality key is not automatically the same thing as a run-identity key just because both happen to be derived from the same prompt prefix today.

---

# What Kram is today

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

The separation is deliberate. The **agent loop does not contain provider-specific code**, the **TUI owns no durable conversation state**, and the **gateway does not own sessions**.

---

# Why Go

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

# Quick start

## Requirements

For building from source:

- Go version declared in [`go.mod`](go.mod) — currently Go 1.26.6;
- at least one configured LLM provider;
- Git is recommended and required for workspace snapshot features;
- language-server binaries are optional and only needed when using LSP tools.

## Configure a provider

The fastest path is letting Kram ask: running `kram` with nothing configured yet opens a first-run setup wizard instead of failing — see "First-run setup wizard" below.

To skip straight past it, export a key before the first run:

```bash
export ANTHROPIC_API_KEY="..."
# or
export OPENAI_API_KEY="..."
# or
export GEMINI_API_KEY="..."
```

Other provider credentials can be configured through the catalog, an explicit gateway config, the accounts screen, or the wizard.

Environment variables always take precedence over keys stored by Kram.

## Start Kram

```bash
go run ./cmd/kram -workspace ~/code/my-project
```

Or with a release binary:

```bash
kram -workspace ~/code/my-project
```

On the very first run (no completed setup yet), Kram opens the setup wizard before anything else — see "First-run setup wizard" below. Every run after that:

1. resolves the workspace;
2. creates `<workspace>/.kram/` if needed;
3. loads provider credentials/configuration;
4. starts gateway and daemon on localhost;
5. waits for both health checks;
6. opens the terminal UI;
7. exposes durable sessions already stored for the workspace.

Logs:

```text
<workspace>/.kram/kram.log
```

Conversation state:

```text
<workspace>/.kram/kram-daemon.db
```

## Useful flags

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
-setup          re-run the first-run setup wizard even if it already completed
-version        print Kram version
```

Example:

```bash
go run ./cmd/kram \
  -workspace . \
  -strategy smart \
  -max-turns 50
```

---

# Feature tour

## First-run setup wizard

Kram opens an 8-step wizard automatically the first time it runs with no completed setup — not just when nothing is configured (an already-exported `ANTHROPIC_API_KEY` still gets the full walkthrough once), and again any time `-setup` is passed. Reopening is driven by a small versioned marker (`onboarding.json`), not by re-checking whether a provider happens to exist.

1. **Environment** — OS, current directory, Git detection, home directory.
2. **Projects** — a suggested *Projects Root* (`~/Projects` on Linux/macOS, `Documents\Projects` on Windows, fully editable, persisted for a future project picker) and the *Workspace* for this session, defaulting to the current directory when it's already a Git repo.
3. **Providers** — the same accounts screen described below, in-flow: paste a key or, for OpenRouter, authorize in the browser (no card, real per-user key, the wizard's recommended path — see "Provider credentials"). Each addition is pinged immediately, and a live "Gateway mode: BASIC/RESILIENT" line reports genuine independent-upstream count, never inflated by OpenRouter's several free-model routes sharing one account.
4. **Routing** — Auto (Kram's existing priority/round-robin heuristic, shown resolved live), Smart, or Round Robin; a note that weights/gates/custom strategies stay tunable in the generated config afterward.
5. **Permissions** — Recommended, Strict, or Autonomous, each a real starter `permissions.json` evaluated by the same engine described in "Permission engine" below, not a separate simplified rule set.
6. **Tools & Skills** — Recommended (nothing disabled), Minimal (read/search/navigation/code-intelligence only), or Custom (the same tools/skills screen described below, with bulk enable-all/disable-all added alongside individual toggles).
7. **System Check** — real, non-fabricated status for Git/Go/gopls, workspace writability, configured providers, and MCP servers — informational only, nothing here blocks continuing.
8. **Ready** — a recap of every choice, then Kram creates a real session and drops straight into it with a one-time, client-side-only welcome note (never persisted, never mistaken for a model reply).

Steps 1-5 run in a small standalone program before the gateway/daemon exist (that's what they're for: producing the config those two need to start). Steps 6-8 run in the normal post-daemon program, entered directly instead of the session picker, since listing real tools/skills needs a live daemon connection. The wizard writes a global `config.yaml` (`~/.config/kram-gateway/config.yaml` — provider credentials themselves stay in the separate, more tightly permissioned `credentials.json`) and a global `permissions.json`; an explicit `-config`, or a workspace-local `<workspace>/.kram/config.yaml`, both still override it — see "Routing configuration".

## Agent loop

Kram is not a chat proxy that makes one model request per user message. One user turn can become a complete agent run:

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

Important properties:

- tool calls execute only after their model response is complete;
- the default run budget is four automatic segments of 50 model calls (200-call emergency ceiling);
- identical tool calls/results trigger a strategy-change nudge and then a visible stagnation stop instead of looping to that ceiling;
- final-budget behavior uses a soft landing rather than a hard mid-task cutoff;
- empty final responses receive one recovery retry and then a visible diagnostic;
- token usage is aggregated across the whole user run, including completed
  candidates rejected before fallback and retry rounds;
- cache reads/writes and reasoning tokens are preserved when a provider reports
  them; the footer may also show an API-list-price equivalent (not a ChatGPT
  subscription charge);
- ChatGPT Codex sessions use stable prompt-cache affinity, deferred tool schemas,
  hosted tool search, and encrypted reasoning replay with `store:false`;
- route trace covers every model call in the run;
- tool calls/results are persisted into durable history.

---

## Multi-provider gateway and Combos v2

The gateway exposes one normalized OpenAI-style surface while adapters translate to/from provider-native protocols.

Current adapter families:

- **Anthropic** — native Messages API translation;
- **Gemini** — native Gemini content/function-call translation;
- **OpenAI-compatible** — OpenAI plus compatible endpoints configured by the user.

A **combo** is a named provider pool/fallback chain. The incoming OpenAI `model` field selects a combo, with `default_combo` as fallback.

Combos v2 separates routing, execution, and acceptance:

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

Circuit-open and capability-incompatible providers are removed before strategy scoring.

### Routing strategies

| Strategy | Purpose |
|---|---|
| `priority` | Preserve configured order. Predictable and cache-friendly. |
| `round-robin` | Rotate peers to distribute calls. |
| `prefix-affinity` | Keep a stable prompt prefix on the same healthy provider. |
| `smart` | Balance health, reliability, latency, quality hint, cache affinity, and priority. |
| `quality` | Emphasize explicit quality hints and reliability. |
| `fast` | Emphasize observed latency while retaining health constraints. |
| `reliable` | Strongly favor observed success and health. |
| `cheap` | Use configured provider priority as the operator's cost preference; Kram does not fabricate price telemetry. |
| `weighted` | Fully configurable weighted engine. |
| `lkgp` | Prefer the last known good eligible provider. |
| `p2c` | Power-of-two-choices style selection. |

The weighted family shares one scoring engine rather than duplicating strategy logic.

### Smart-routing signals

The weighted engine can use:

- breaker/health state;
- observed success rate;
- observed average latency;
- explicit `quality_hint`;
- prompt-prefix/cache affinity;
- combo priority;
- last-known-good boost;
- stickiness;
- bounded exploration.

Kram intentionally does not invent quality, price, quota, or latency data it has not measured or been explicitly given.

### ResponseGate

A transport-level success is not always a usable model response. Combos can deterministically reject responses based on conditions such as:

- empty output;
- missing terminal completion;
- minimum text length;
- configured substrings used by upstreams to disguise technical errors inside HTTP 200 responses.

The gate judges **technical usability**, not whether Kram agrees with the model's answer. It is not designed for refusal-shopping.

### Streaming fallback

For streaming requests, Kram performs a bounded peek before committing downstream output. Empty/role-only chunks, keepalives, immediate errors, and failures before meaningful output can still fall through to another provider.

After meaningful content has been committed, provider switching is no longer safe inside the same response — but commit is not the same thing as success. The attempt's real outcome (and whatever the router does with it — breaker state, Sticky, LKGP) is decided only once the stream reaches an actual terminal state: a valid completion, an explicit upstream error, or the upstream channel closing without ever completing, which is treated as a failure with an explicit error signal rather than a silent `[DONE]`.

---

## Circuit breakers and provider isolation

Each provider has independent breaker state.

Current behavior:

- 3 consecutive failures open the circuit;
- open providers are skipped;
- after a 30-second cooldown, a half-open recovery attempt is allowed;
- success closes/resets the circuit;
- failure during half-open reopens it.

The gateway also exposes real provider telemetry such as request count, failures, token usage, average latency, success rate, capabilities, and breaker state.

---

## Durable sessions

The daemon owns conversation durability. The TUI is only a view.

Sessions and messages are persisted to SQLite before success is reported to the caller. Closing the terminal does not delete conversation history, and daemon restart does not erase it.

The same store backs:

- messages and tool-call history;
- persistent memory;
- FTS5 session search;
- compaction summaries;
- provider attribution.

---

## Built-in tools

A normal daemon with persistence available registers **34 core tools** before custom tools and MCP-provided tools are added.

| Area | Tools | Purpose |
|---|---|---|
| Files | `read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `move_file`, `delete_file` | Inspect and modify workspace files deterministically. |
| Shell/processes | `bash`, `run_background`, `process_list`, `process_output`, `process_kill` | Run bounded commands or manage daemon-owned long-running processes. |
| Git | `git_status`, `git_diff` | Read repository status and diffs. |
| Web | `web_fetch` | Fetch bounded HTTP(S) reference content. |
| Planning | `todo_write`, `todo_read` | Keep a persistent project task list. |
| Interaction | `ask_question` | Pause the run and ask the user instead of guessing. |
| Delegation | `delegate_task` | Fan independent work out to isolated subagents. |
| Skills | `skill_list`, `skill`, `skill_install` | Discover/load/install reusable instruction packages. |
| Artifacts | `artifact_read` | Read slices of oversized output stored by Kram. |
| Code intelligence | `lsp_diagnostics`, `lsp_definition`, `lsp_references` | Use language-server semantics. |
| Recovery | `snapshot_create`, `snapshot_list`, `snapshot_diff`, `snapshot_restore` | Explicit workspace snapshots and restore. |
| Memory/history | `memory_write`, `memory_search`, `session_search` | Durable knowledge and historical retrieval. |

### File boundary versus shell boundary

Structured file tools resolve paths against the workspace root and reject escapes.

The shell is intentionally different: it starts in the workspace but is a real operating-system shell, **not a filesystem sandbox**. Stronger host isolation should come from a container, VM, or OS sandbox rather than pretending `cwd` provides security.

---

## Permission engine: ALLOW / ASK / DENY

Tool availability and tool permission are distinct.

Every built-in, manifest-defined, and MCP-backed tool call passes through the same permission evaluator.

```text
allow -> execute
ask   -> pause and ask the user
 deny  -> refuse
```

Rules can target exact tool names, MCP prefixes, and operation subjects such as commands or file paths. More-specific matches beat broader ones.

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

Choosing **always** persists an exact allow grant for the approved subject instead of silently widening permission.

A fully denied tool is removed from model-visible definitions entirely.

---

## Interactive questions and approvals

`ask_question` lets the model pause a live run for information it genuinely needs.

Permission `ask` decisions use a separate approval flow with:

```text
once
always
deny
```

These are distinct concepts:

```text
ask_question -> model needs information
approval     -> policy needs authorization
```

Both are delivered through the live daemon SSE stream and both are bounded. Approval timeout fails closed.

---

## Context management

Kram manages context before the upstream provider has to reject it.

Current path:

1. build effective history;
2. structurally prune old/redundant tool material;
3. if still necessary, generate a compact summary;
4. store the summary as explicitly non-actionable reference context;
5. reload the reduced effective history.

Compaction attempts are capped per run. Persistent overflow becomes a real `ErrContextOverflow` rather than an infinite summarize/retry loop.

The TUI context panel uses the same runtime accounting path instead of maintaining a separate estimate.

---

## Deterministic output filtering

Command output can be thousands of lines of progress noise around a few useful diagnostics.

Kram applies command-aware deterministic filtering to inline shell output:

- no extra model call;
- no generated summary;
- preserve/error patterns are evaluated before drop patterns;
- routine output can collapse to a small truthful result instead of occupying later context.

The filter can remove known noise. It does not invent replacement output.

---

## Artifact store and bounded producer memory

Large command/custom-tool output is streamed through a spill writer.

Small output remains inline. Large output is persisted under the workspace artifact store and replaced in the model context by a bounded preview plus an artifact ID.

`artifact_read` can then retrieve the stored data in slices.

This protects both context size and the process's real memory footprint.

The agent loop additionally enforces a combined per-turn tool-output budget so several medium outputs cannot collectively explode the next model request.

---

## Persistent memory

Memory is **agent-curated**, not automatic conversation scraping.

It supports:

- project scope;
- global scope;
- FTS5 search through `memory_search`;
- bounded automatic injection of pinned/recent entries;
- replace/remove operations for consolidation;
- a hard per-scope size cap.

Recent memory is frozen once per user run so model/tool round-trips keep a stable prompt prefix.

Newly written memory is available on the next user run.

---

## Cross-session search

`session_search` retrieves what users and assistants actually said across durable sessions, even if nobody promoted the information into persistent memory.

It uses SQLite FTS5/BM25 retrieval rather than another model call.

By design:

- user and assistant text is indexed;
- tool/system noise is not the primary search surface;
- delegated subagent sessions are excluded by default;
- wider scope can be requested explicitly;
- results carry surrounding context.

Kram therefore has both **curated memory** and **historical recall** without conflating them.

---

## Project context

Kram can load root-level project instructions from:

```text
AGENTS.md
CLAUDE.md
```

Project context is re-read rather than permanently copied into conversation history, so edits affect subsequent work without creating a new session.

The model preamble is assembled roughly as:

```text
Kram system rules
+ project context
+ persistent memory snapshot
+ effective conversation history
```

---

## Subagents

`delegate_task` splits independent work into parallel subtasks.

A delegated worker starts in a fresh session with **zero inherited conversation history**. It receives only the explicit goal/context supplied by the parent.

Current safeguards:

- up to 3 concurrent workers by default;
- nesting depth capped at 1;
- each task may use a different gateway combo/model;
- parent waits for the batch and receives consolidated results.

Subagents currently share the same workspace. They are conversationally isolated, not filesystem-isolated.

---

## Skills

Skills are reusable instruction packages containing `SKILL.md`.

Kram uses progressive disclosure:

1. `skill_list` exposes names/descriptions;
2. `skill` loads full instructions only when needed;
3. `skill_install` can discover/install skills from a public Git repository while reporting source/license information.

Project skills:

```text
<workspace>/.kram/skills/<skill>/SKILL.md
```

Global skills:

```text
~/.config/kram-gateway/skills/<skill>/SKILL.md
```

Skills can be disabled through the same settings system used for tools.

---

## Custom tools without rebuilding Kram

Project and global JSON manifests can expose process-backed tools without adding Go code.

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

Arguments are sent as JSON on stdin and stdout becomes the result.

Custom tools share Kram's shell runner, output limits/artifact handling, tool settings, and permission path.

A manifest cannot override a built-in tool name. Project custom tools take precedence over global custom tools with the same custom name.

---

## MCP client

Kram includes its own MCP JSON-RPC client implementation in Go rather than requiring an MCP SDK inside the runtime.

Supported capabilities include:

- stdio transport;
- Streamable HTTP transport;
- initialization/lifecycle handling;
- `tools/list` / `tools/call`;
- resources list/read;
- prompts list/get;
- project and global configuration;
- server isolation;
- reconnect supervision with bounded exponential backoff;
- `tools/list_changed` refresh;
- on-disk schema snapshots keyed by connection-config fingerprint.

Remote tools are namespaced:

```text
mcp__<server>__<tool>
```

so an external server cannot silently shadow `bash`, `read_file`, or another built-in.

When MCP servers are available, Kram also exposes:

```text
mcp_resource_list
mcp_resource_read
mcp_prompt_list
mcp_prompt_get
```

MCP is an external trust boundary; remote tools still pass through Kram's common permission path.

---

## LSP code intelligence

Kram contains a small LSP client over `Content-Length` framed JSON-RPC.

Language servers start lazily and one process is reused per language.

Agent-facing capabilities:

- diagnostics;
- definition;
- references.

Built-in mappings cover Go, TypeScript/JavaScript, and Python, while project/global `lsp.json` can override commands or add new extensions/languages.

If an LSP server is missing, only that semantic capability is lost. Kram continues running with normal file/search tools.

---

## Workspace snapshots

Snapshot operations:

```text
snapshot_create
snapshot_list
snapshot_diff
snapshot_restore
```

Snapshots use a **separate Git repository** under Kram state instead of the workspace's real `.git` metadata.

The snapshot layer:

- respects `.gitignore`;
- excludes `.git` and `.kram` from captured history;
- reports affected paths on restore;
- leaves files that were never captured alone;
- degrades cleanly if Git is unavailable.

Snapshots are explicit, not automatically created before every mutation.

---

## Cross-platform shell and background processes

All process-backed capabilities share `internal/shell`.

### Unix

Commands resolve `sh` from `PATH` (with a Termux-prefix fallback and `/bin/sh` only as a last resort) and use their own process group so cancellation can target the process tree.

### Windows

Commands use `cmd.exe /S /C` and Windows Job Objects with kill-on-close behavior.

`bash` remains foreground-only, with a default 30-second timeout and 120-second maximum.

Long-running work uses:

```text
run_background
process_list
process_output
process_kill
```

Tracked background process trees are terminated on daemon shutdown.

---

# Terminal UI

The TUI is implemented with Bubble Tea/Lip Gloss and communicates with the real daemon/gateway APIs.

It does not persist conversations itself and does not call providers directly.

## Transcript and composer

- Kram responses remain left-aligned and completed responses render Markdown;
- user messages render as a compact right-aligned prompt block;
- the composer is a 3-row word-wrapping textarea;
- assistant text streams incrementally;
- tool calls appear while running and settle to result state;
- notices, questions, and approval prompts appear inside the active turn;
- mouse-wheel transcript scrolling is supported.
- dragging text copies it through OSC 52 and leaves a short visual confirmation;
- live activity labels (`MODELO ATIVO`, `EXECUTANDO`, `ESCREVENDO`) come from daemon events and consume no model tokens.

## Route bar

A one-line bar above the transcript reports the active routing strategy and, once available, the real attempt trail.

Wide terminals can show provider names, outcome glyphs, and latency. Narrower layouts progressively reduce detail without letting long provider IDs wrap the UI.

While a model call is in flight, Kram shows a generic routing state because the daemon does not yet receive true per-attempt live progress from inside the gateway fallback loop.

Click the strategy block (marked with `▾`) or press `Ctrl+S` to open the
runtime strategy picker. Arrow keys move, `Enter` applies, `Esc` cancels, and
clicking an option applies it directly. A change is atomic and affects the
next model call; a call whose ranking already began finishes with its original
strategy. Runtime choices deliberately reset when the gateway restarts — edit
`config.yaml` when the change should be permanent.

The mutation endpoint is loopback-only even when the gateway's inference API
is exposed to a LAN.

## `Ctrl+R` — full RouteTrace

Shows the most recently completed user run:

- every model call;
- every upstream attempt;
- provider;
- latency;
- outcome;
- rejection/error reason;
- winner;
- aggregate call/attempt/fallback/provider-time counts.

## `Ctrl+P` — strategy explainability

For scoring strategies, the panel renders the router's own factor data:

```text
weight × value = contribution
```

plus total score and reasons such as sticky, LKGP, cache affinity, or exploration.

The TUI never recomputes the routing score.

## `Ctrl+T` — context panel

Shows context usage and remaining budget sourced from the daemon's own accounting path.

## `Ctrl+B` — background-process observer

Shows every process started by `run_background` and its captured stdout/stderr without asking the model to call `process_output`.

- wide terminals open a side tile while keeping the conversation visible;
- narrow terminals use the same area as a full-width process tab;
- click a structured `bgN` tool-activity link or press `Ctrl+B`;
- `Tab`/`Shift+Tab` switches processes, arrows/Page Up/Page Down scroll, `End` resumes live follow, and `Esc` closes;
- scrolling away from the tail pauses auto-follow and reports newly arrived bytes;
- polling happens only while the observer is open and transfers output incrementally;
- the panel is read-only; process termination remains permission-gated through `process_kill`.

Only captured stdout/stderr can be shown. A process that is alive but produces no output is reported honestly as such; Kram does not invent internal progress. Background-process state remains daemon-lifetime, so restarting the daemon stops tracked process trees and invalidates their `bgN` IDs.

## Session picker and settings

Launching without `-session` opens durable session selection.

From the picker:

- `a` opens provider/account management;
- `f` opens tool/skill settings;
- arrow keys navigate;
- `Enter` resumes or creates a session;
- `Ctrl+C` exits.

The accounts screen can store credentials, use supported OAuth flows, and run real lightweight connectivity/auth checks. Status dots come from actual pings rather than decorative state.

---

# Provider credentials

Keys may come from environment variables or Kram's local credential store.

Environment variables **always win**. Stored credentials only fill values that are otherwise unset.

Store location:

```text
~/.config/kram-gateway/credentials.json
```

The file is written with `0600` permissions.

Credentials are not application-encrypted at rest. Kram relies on local filesystem/user-account protection rather than pretending bundled reversible encryption is a separate security boundary.

---

# Routing configuration

Resolved in this order, first match wins:

1. an explicit `-config` file;
2. `<workspace>/.kram/config.yaml` — a per-project override, hand-written or (currently) never auto-generated;
3. `~/.config/kram-gateway/config.yaml` — the global config the first-run wizard writes;
4. plain env-var autodetection, building a default combo automatically from whichever provider credentials are set.

For that last, fully automatic tier: when only free-tier peers are present, the auto path favors distribution. When a paid provider is present, it favors stable priority for prompt-cache economics. `-strategy` can override the auto choice without requiring a full YAML file.

For complete control, pass `-config` — or run `kram -setup` and let the wizard generate a starting point.

## Example

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

`quality_hint` is an explicit operator signal. Kram does not pretend it has benchmark data it never measured.

An absent `response` block preserves permissive compatibility behavior. An absent `strategy_options` block uses strategy defaults.

See [`config.example.yaml`](config.example.yaml) for the repository example.

---

# HTTP surfaces

## Gateway

### `POST /v1/chat/completions`

OpenAI-compatible chat-completions surface with streaming/non-streaming support and Kram routing metadata on completed responses/chunks.

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

Returns provider IDs/kinds, capabilities, breaker state, request/failure counts, token totals, average latency, success rate, and configured combos/strategies.

### `GET /health`

Gateway liveness.

## Daemon

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

`POST /sessions/{id}/messages` responds over SSE with events such as:

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

The final `done` event carries the persisted assistant message, usage, tool activity, compaction count, RouteTrace, and image-capability notice.

---

# Running components separately

The all-in-one path is recommended for normal use, but every major layer remains independently runnable.

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

---

# Local state layout

Typical workspace state:

```text
<workspace>/.kram/
├── kram.log
├── kram-daemon.db
├── todos.json
├── permissions.json
├── permission_grants.json
├── mcp.json
├── lsp.json
├── artifacts/
├── snapshots/
├── skills/
└── tools/
```

Global/user configuration lives under:

```text
$XDG_CONFIG_HOME/kram-gateway/
```

falling back to:

```text
~/.config/kram-gateway/
```

This area contains credentials, global settings/policy, skills, custom tools, MCP configuration, LSP configuration, and related caches where applicable.

---

# Reliability model

Kram treats “never crash” as an engineering direction, not a magic promise.

The goal is to reduce blast radius and make failure explicit.

Examples:

- gateway and daemon handlers recover panics rather than killing the whole process;
- providers have independent circuit breakers;
- fallback happens before response commitment when possible;
- MCP failures stay isolated to their server;
- LSP failure stays isolated to its language capability;
- background processes and LSP servers are cleaned up on daemon shutdown;
- process-tree cancellation is centralized;
- output memory/context growth is bounded;
- agent turns have iteration and compaction budgets;
- empty model output cannot silently complete twice;
- disabled/fully denied tools are hidden;
- approval timeout denies rather than allows;
- durable state belongs to the daemon, not the terminal;
- explicit snapshots provide a recovery path for workspace mutations.

The objective is not that failures disappear. It is that they become **contained, observable, and recoverable**.

---

# Security and trust boundaries

Kram is an agent that can execute developer tools. Its trust boundaries are therefore explicit.

### Structured file tools

Path resolution is workspace-bound and rejects escapes.

### Shell

The shell is a real host shell. It is not an OS sandbox. Use containers/VMs/OS sandboxing when running untrusted code that requires a stronger boundary.

### Tool permissions

ALLOW/ASK/DENY policy runs before all registered tool execution paths, including custom and MCP tools.

### MCP

MCP servers are external code/services. Namespacing prevents tool-name shadowing, but an approved remote tool still has whatever capabilities its server exposes.

### Skills, project files, web content, and tool output

These can all contain untrusted instructions or data. Provenance still matters; Kram does not make external content inherently safe.

### Credentials

Stored keys rely on local filesystem permissions.

### Snapshots

Snapshots use isolated Git metadata, but they are not a complete host filesystem backup.

---

# Testing

## Go suite

```bash
./scripts/verify.sh

# individual commands used by the gate
go test ./... -race
go vet ./...
go build ./...
```

No automated CI runs these yet — GitHub Actions on this account currently
requires a paid spending limit. `scripts/verify.sh` is therefore the
reproducible local gate: diff/format checks, vet, a fresh race-enabled suite,
**at least 90% global statement coverage across tracked packages**, host build,
Windows and Android cross-builds, and installer tests. The release script
cannot publish when that coverage floor is missed. See "Continuous integration" in
[`DECISIONS.md`](DECISIONS.md).

Coverage includes areas such as:

- routing and weighted scoring;
- response/stream gates;
- route traces and TUI rendering;
- circuit breakers;
- permission policy/grants;
- artifact spill behavior;
- snapshots;
- cross-platform process control;
- LSP transport/client/manager;
- MCP lifecycle/cache/reconnect;
- FTS5 memory/session retrieval;
- tool boundaries and output filtering;
- eval harness behavior.

## Model evals

```bash
go run ./evals
```

Evals run through the real gateway + daemon stack with an actual configured model.

The harness distinguishes:

- **PASS** — the behavior was exercised and succeeded;
- **FAIL** — the behavior was exercised and violated the scenario;
- **SKIP** — the scenario could not observe the property it was supposed to test.

Hard scenarios represent runtime invariants. Model-dependent soft scenarios remain diagnostic rather than pretending every model behaves identically.

---

# Installing

The quickest platform-specific walkthrough is in
[Install now / Instale agora mesmo](#install-now--instale-agora-mesmo).

Latest Linux or macOS release:

```bash
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
```

Downloads the right binary for your OS/architecture from GitHub Releases, verifies its SHA-256 checksum, and installs it to `$HOME/.local/bin` — or `$PREFIX/bin` in Termux. No Go toolchain or `sudo` is needed.

Windows amd64 (PowerShell, no Administrator shell required):

```powershell
irm https://raw.githubusercontent.com/codexmark/kram-releases/master/install.ps1 | iex
```

Termux/Android arm64 uses the same shell command and automatically selects
`kram-android-arm64.tar.gz`. Install its lightweight prerequisites first with
`pkg install curl tar coreutils git`.

Install a specific Unix version by passing the variable to `sh`, which is the
process that evaluates the installer:

```sh
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | KRAM_VERSION=v0.2.7 sh
```

PowerShell version pinning uses a script block so the requested version is
visible to the installer:

```powershell
$env:KRAM_VERSION = "v0.2.7"
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/codexmark/kram-releases/master/install.ps1)))
```

# Building releases

```bash
./scripts/build-release.sh v1.2.3
```

Current targets:

```text
linux/amd64
linux/arm64
darwin/amd64
darwin/arm64
windows/amd64
android/arm64
```

Release builds use:

```text
CGO_ENABLED=0
```

The build script produces `.tar.gz` archives on Unix-like targets and `.zip` on Windows (each containing just a `kram`/`kram.exe` binary, and named without a version — `kram-linux-amd64.tar.gz`, not `kram-v1.2.3-linux-amd64.tar.gz` — so the installer can construct a download URL from OS/arch alone), plus a `SHA256SUMS` file, with version information embedded through linker flags.

Releases are built and published entirely from the maintainer's own machine rather than through GitHub Actions — see "Continuous integration" and "curl-based install distribution" in [`DECISIONS.md`](DECISIONS.md) for why. To cut one:

```bash
./scripts/release.sh v1.2.3
```

This runs `scripts/verify.sh`, cross-compiles every target, generates `SHA256SUMS`, shows a summary, asks for confirmation, and publishes the GitHub Release to the separate [codexmark/kram-releases](https://github.com/codexmark/kram-releases) distribution repository. See `scripts/release.sh --help` for flags (`--notes FILE`, `--yes`).

---

# Repository map

| Path | Responsibility |
|---|---|
| `cmd/kram` | Recommended all-in-one launcher. |
| `cmd/gateway` | Standalone gateway. |
| `cmd/daemon` | Standalone durable daemon. |
| `cmd/cli` | Standalone terminal client. |
| `internal/daemon/agent` | Tool-calling loop and run lifecycle. |
| `internal/daemon/store` | SQLite sessions/messages/memory/FTS5 search. |
| `internal/daemon/tools` | Tool registry and concrete capabilities. |
| `internal/daemon/compaction` | Context pruning/summarization. |
| `internal/daemon/contextpolicy` | Shared prompt/history/response/tool-output budget planning. |
| `internal/router` | Combos v2 strategies, factors, affinity, gates, trace data. |
| `internal/server` | Gateway HTTP surface. |
| `internal/provider` | Provider adapters. |
| `internal/breaker` | Per-provider circuit breaker. |
| `internal/telemetry` | Provider runtime counters. |
| `internal/permission` | ALLOW/ASK/DENY policy and grants. |
| `internal/artifact` | Spill writer and artifact store. |
| `internal/shell` | Cross-platform process execution/cleanup. |
| `internal/snapshot` | Isolated workspace snapshots. |
| `internal/lsp` | LSP protocol/client/manager. |
| `internal/mcp` | MCP JSON-RPC client/transports/lifecycle/cache. |
| `internal/cli/app` | Terminal UI and live panels/settings. |
| `internal/credentials` | Local provider-key store. |
| `internal/providercatalog` | Provider auto-configuration catalog. |
| `internal/providerping` | Lightweight provider connectivity/auth checks. |
| `internal/toolsettings` | Tool/skill enable-disable persistence. |
| `internal/onboarding` | First-run wizard's versioned completion state. |
| `evals` | End-to-end behavioral eval harness. |
| `scripts` | Build/release automation. |
| `DECISIONS.md` | Architectural rationale, reversals, and known gaps. |

---

# Current boundaries

Kram deliberately does not pretend every agent-runtime problem is already solved.

Important current boundaries include:

- shell execution is not a host sandbox;
- subagents share the workspace;
- snapshots are explicit rather than automatic before every mutation;
- MCP schema caching does not yet replace every startup connection with fully lazy discovery;
- streaming fallback is only possible before downstream commitment;
- live route progress is currently per model call rather than true per-provider-attempt streaming;
- scheduling/cron-style autonomous runs are not part of the current core;
- context accounting is provider-agnostic and uses a documented chars/4 estimate rather than each provider's tokenizer;
- aggregate per-turn output budgeting can still truncate with an explicit notice even though individual oversized producers are artifact-backed.

These are documented engineering boundaries, not hidden limitations behind optimistic UI.

---

# Development philosophy

Many of Kram's current principles are consequences of the failure modes above:

- hard capability constraints before smart scoring;
- stable prompt prefixes before unnecessary provider rotation;
- real trace data before simulated observability;
- one score calculation in the router, not one in the router and another in the UI;
- bounded producer memory before post-hoc truncation;
- deterministic filtering before model-generated compression;
- explicit process ownership before background shell magic;
- one permission choke point before scattered confirmation dialogs;
- curated memory plus searchable history instead of one unbounded memory bucket;
- isolated recovery state instead of borrowing the user's `.git` metadata;
- graceful degradation when optional integrations fail;
- PASS/FAIL/SKIP instead of pretending unobserved behavior passed;
- a single distributable binary without collapsing all responsibilities into one package.

When a behavior is important enough to show in the TUI, the preferred design is for the runtime to compute it once and the UI to render that truth.

When a limit matters for reliability, the preferred design is to enforce it where the resource is produced, not after damage has already happened.

When a capability can mutate the developer's machine, the preferred design is to make ownership and permission explicit rather than depend on convention.

That is the direction Kram continues to follow.

For the detailed decision log, see [`DECISIONS.md`](DECISIONS.md).

---

# License

Kram is licensed under the [MIT License](LICENSE).

Copyright © 2026 codexmark.
