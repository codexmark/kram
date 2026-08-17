# kram-gateway

An OpenAI-compatible LLM gateway: load balancing, circuit-breaker fallback
and per-provider telemetry across multiple upstream providers, in a single
static Go binary. v0 scope only — no compression pipeline, no persistence,
one routing strategy (round-robin).

This is the first component of **Kram**, a coding-agent platform being
built from scratch in Go, aimed at performance, reliability ("never
crash") and token economy.

**To actually use Kram, run `cmd/kram`** (see "All-in-one" below) — the
rest of this doc mostly describes the individual components for
development. The gateway, daemon and CLI all still work standalone too.

## Inspirations

Kram is a clean-room build, not a fork — but its design borrows ideas from
three existing projects:

- **[opencode](https://github.com/sst/opencode)** — the agent/UX layer
  model: session, tools, LSP integration, plugins, MCP client. Kram's
  coding-agent layer (not yet started) will follow a similar shape,
  rewritten in Go.
- **[OmniRoute](https://github.com/diegosouzapw/OmniRoute)** — the idea of
  a dedicated LLM gateway in front of the agent: load balancing across
  many providers, circuit-breaker fallback chains ("combos"), context
  compression, per-provider telemetry. `kram-gateway` reimplements this
  concept from scratch in Go rather than embedding or wrapping OmniRoute
  itself.
- **[Compozy](https://github.com/compozy/compozy)** — the daemon-owned,
  local-first orchestration model: durable sessions/tasks that survive
  closing a terminal, multiple control surfaces (CLI/HTTP/MCP), a single
  source of truth. `cmd/daemon` follows this model.
- **[Hermes Agent](https://github.com/NousResearch/hermes-agent)** — the
  agent loop's iteration-budget "soft landing" (a warning near the limit,
  then one forced final answer instead of a hard cutoff), and its
  foreground-only, timeout-bounded shell tool.
- **[OpenClaude](https://github.com/Gitlawb/openclaude)** and
  **[Antigravity CLI](https://github.com/google-antigravity/antigravity-cli)**
  — per-session model routing and terminal-first agent UX patterns.

No code from any of these is reused; only the architectural ideas are.

## All-in-one (`cmd/kram`)

This is the actual entry point for using Kram — one binary, one command,
no separate terminals and no manual port coordination.

```bash
export ANTHROPIC_API_KEY=sk-...   # or OPENAI_API_KEY / GEMINI_API_KEY
go run ./cmd/kram -workspace ~/projects/whatever
```

`cmd/kram` starts the gateway and daemon **in-process** (goroutines, not
subprocesses — `internal/gateway` and `internal/daemon` export the same
`Run()` each standalone binary calls) on free localhost ports, waits for
both `/health` checks, creates a session, and drops straight into the CLI.
Gateway/daemon logs go to `<workspace>/.kram/kram.log` instead of stdout,
since stdout belongs to the CLI's alt-screen once it starts. `ctrl+c`
shuts everything down together — no orphaned processes, no ports left
bound.

With no `-config`, it auto-detects a gateway config from whichever of
`ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GEMINI_API_KEY` are set in your
environment, pinned to a sensible default model each, in one round-robin
combo. Pass `-config path/to/config.yaml` (see `config.example.yaml`) for
anything more specific — multiple providers per kind, OpenRouter/opencode
zen, custom strategies. If neither is available it fails immediately with
a clear message rather than starting partially.

Session state persists to `<workspace>/.kram/kram-daemon.db` — same
durability guarantees as running the daemon standalone (component 2).

Everything below describes the individual components — useful for
development, or if you want the gateway/daemon on separate machines.

## Run

```bash
cp config.example.yaml config.yaml
# edit config.yaml, set the env vars it references (ANTHROPIC_API_KEY, etc.)
go run ./cmd/gateway -config config.yaml
```

## Use

```bash
curl http://127.0.0.1:20128/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"default","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

`model` selects a combo from `config.yaml` (falls back to `default_combo`
if it doesn't match a combo ID). Each combo is an ordered list of
providers; round-robin picks which one leads on a given request, and if it
fails (or its breaker is open), the next one in the combo is tried —
before any bytes are sent to the client, for non-streaming requests.

## Endpoints

- `POST /v1/chat/completions` — OpenAI-compatible, streaming and non-streaming
- `GET /admin/status` — per-provider request/failure counts, token usage, breaker state
- `GET /health` — liveness check

## Providers

Three adapter kinds, one file each in `internal/provider/`:

- `anthropic` — native Messages API
- `gemini` — native streamGenerateContent
- `openai-compat` — anything that already speaks OpenAI's chat-completions
  format: OpenAI, OpenRouter, opencode zen, etc. Only `base_url` and the
  API key env var differ.

## Reliability

- Panic in any request handler is recovered — it never takes the process down.
- Per-provider circuit breaker: 3 consecutive failures trips it open for
  30s, then a single half-open trial decides whether to close or re-open.
- Non-streaming requests only commit to a provider (write any bytes) after
  it has fully succeeded — a failure anywhere in the combo before that
  falls through to the next provider.

## Daemon (component 2)

`cmd/daemon` is the single, local, durable owner of sessions: it persists
sessions and messages to SQLite (pure-Go driver, no cgo) before reporting
success to any caller, so a session survives both a client disconnecting
and the daemon itself restarting. It never talks to LLM providers
directly — every completion goes through `kram-gateway`.

```bash
# terminal 1
go run ./cmd/gateway -config config.yaml

# terminal 2
go run ./cmd/daemon -db kram-daemon.db -gateway http://127.0.0.1:20128
```

```bash
# create a session
curl -s -X POST http://127.0.0.1:20130/sessions -d '{"title":"demo"}'
# → {"id":"ses_...","title":"demo",...}

# send a message (daemon calls the gateway, persists both turns)
curl -s -X POST http://127.0.0.1:20130/sessions/ses_.../messages \
  -d '{"content":"hi"}'

# read it back — works even after restarting the daemon
curl -s http://127.0.0.1:20130/sessions/ses_...
```

Endpoints: `POST /sessions`, `GET /sessions`, `GET /sessions/{id}`,
`POST /sessions/{id}/messages`, `GET /health`.

## CLI (component 3)

`cmd/cli` is Kram's user-facing interface: a Bubble Tea ([charmbracelet](https://github.com/charmbracelet/bubbletea))
terminal app over a daemon session. Its own visual language — the "pulse
bar" — is designed from scratch for this project, not copied from
opencode/Crush's bordered-panel look. It never talks to an LLM provider
or persists anything itself; it's a live view over the daemon and the
gateway's real telemetry (nothing shown is simulated).

```bash
go run ./cmd/cli -daemon http://127.0.0.1:20130 -gateway http://127.0.0.1:20128
```

- The footer is always exactly two lines: the active provider with a
  breathing dot and an animated latency indicator while a request is in
  flight, settling — once the reply lands — into the real per-request
  fallback trail (one dot per provider actually attempted) and token
  usage. It never grows past two lines, no matter how many providers a
  combo has.
- `ctrl+p` opens the strategy panel (25% of the terminal height): every
  provider in the active combo, staggered to show fallback order, each
  tagged with its live circuit-breaker state, average latency and success
  rate straight from `/admin/status`. Arrow keys move focus between
  providers; the explanation line updates to describe whichever one is
  focused. `esc` or `enter` closes it.
- A discreet badge on the footer's bottom-right (`◔ NN%`) is a real click
  target, not just decoration — click it, or press `ctrl+t`, to open the
  context-window panel: a segmented bar plus a per-category token
  breakdown (messages, tool definitions, compaction summary, free space)
  computed by `GET /sessions/{id}/context` the same way the daemon decides
  when to compact (`internal/daemon/compaction`), so the panel and the
  actual compaction trigger never disagree. Only real categories Kram
  actually has — no invented "skills" or "MCP tools" line items.
- `ctrl+c` quits.

## Agent loop (component 4)

`internal/daemon/agent` is what makes Kram an agent rather than a chat
relay: each user message runs a full tool-calling loop, not a single
model call.

- **Tool-calling protocol** (`internal/openai`, `internal/provider`): the
  gateway translates Kram's normalized tool-call format to and from each
  provider's native wire format — OpenAI-style `tool_calls` deltas
  (accumulated across fragmented SSE chunks), Anthropic `tool_use`/
  `tool_result` content blocks, and Gemini `functionCall`/
  `functionResponse` parts.
- **Tools** (`internal/daemon/tools`), 14 total: `read_file`, `write_file`,
  `edit_file` (exact find-and-replace — cheaper in tokens than rewriting a
  whole file, and refuses an ambiguous match instead of guessing which
  occurrence was meant), `list_dir`, `glob` (`**` supported, no external
  glob-library dependency), `grep` (pure Go, no `ripgrep` dependency),
  `move_file`, `delete_file` (refuses directories — recursive deletes go
  through `bash`, where the command is at least visible in the tool-call
  log), `bash` (foreground-only, timeout-bounded), `git_status`,
  `git_diff`, `web_fetch` (GET + HTML-tag stripping, size-capped), and
  `todo_write`/`todo_read` (a project-wide task list persisted to
  `.kram/todos.json`, so the agent can plan multi-step work and pick it
  back up after a restart). Every file/shell tool is confined to the
  daemon's `-workspace` directory — a path that would escape it is
  rejected before touching the filesystem.
- **The loop**: waits for a complete (non-streaming) model response before
  acting on tool calls — every agent-loop implementation we looked at
  decouples token streaming from tool execution rather than interleaving
  them. Tool calls run sequentially and their results are persisted and
  fed back until the model answers in plain text. `-max-turns` bounds the
  round-trips (default 50); near the limit, tools are withdrawn from the
  next call and the model is asked directly for a final answer (Hermes's
  "soft landing" pattern) rather than being cut off mid-loop.
- **Memory and compaction** (`internal/daemon/compaction`): a tiered
  strategy — cheap structural pruning of old tool output first, full LLM
  summarization only if that isn't enough. The summary is stored as a
  system message wrapped as explicitly non-actionable reference material,
  and consecutive compactions are capped at 3 per run: both are direct
  guards against a real, repeatedly-hit bug in other agent loops where the
  model re-executes its own summary as a new task, overflows again, and
  loops forever (see package doc for the specific issues this targets).
- **Images**: gated by capability, never assumed. If images are attached
  but no provider in the active combo declares `supports_images: true` in
  `config.yaml`, they're dropped before the request is sent and the
  caller gets an explicit notice — the CLI shows it inline rather than
  silently sending a text-only request.

Every message, tool call, tool result, and compaction summary is
persisted through the same durable store as component 2 — an agent run
survives a daemon restart exactly like a plain conversation does.
