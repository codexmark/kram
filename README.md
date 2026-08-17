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
- **[ai-memory](https://github.com/akitaonrails/ai-memory)** — the idea of
  agent-curated, scoped (project vs. global) persistent memory searchable
  by full-text index, surfaced automatically at the start of a turn
  instead of requiring the user to repeat themselves every session. Kram's
  version is a deliberately smaller slice of that design — see "Memory"
  below for what was kept and what wasn't.

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

Assistant replies render through a custom [Glamour](https://github.com/charmbracelet/glamour)
style matching Kram's own palette (`internal/cli/app/markdown.go`) — bold,
headings, lists, code blocks and blockquotes render properly instead of
showing raw `**`/`###`. A malformed-markdown or rendering failure falls
back to the raw text rather than crashing; user messages are never run
through it (echoing back reformatted input would be surprising). While a
turn is running, the transcript shows a "thinking" line — the `kram` tag
under a real color-gradient shimmer (`internal/cli/app/shimmer.go`,
[go-colorful](https://github.com/lucasb-eyer/go-colorful) interpolation
sweeping per character, not a discrete palette step) plus a live elapsed
counter. This came out of a research pass on Crush and OpenClaude's chat
UIs specifically: both independently converged on a moving color gradient
as their "working" indicator instead of a spinner glyph, and OpenClaude
separately tracks stalled-vs-progressing state rather than showing the
same "busy" look either way — so past 8s with no new event (delta, tool
start/result, notice — see `stallThreshold`), the line switches to a
distinct color and says plainly that it's still going, instead of
shimmering as if everything's normal. Same real elapsed time is echoed in
the footer while waiting.

User message bubbles anchor flush right regardless of length: Lip Gloss's
`Style.Width` pads short content out to that width with trailing spaces
by default, which the naive version of this used as the input to its
own right-alignment math — so a short message inherited the padded box's
width, not its own, and rendered hugging the left edge inside an
invisible box instead of flush against the true right edge. Fixed by
trimming that trailing padding before measuring each line.

A small block-letter "KRAM" banner (`internal/cli/app/banner.go`) opens
the session picker — hand-built and verified by direct render before
shipping, not typed freehand into the final file (ASCII art is easy to
get subtly misaligned).

Replies stream token by token instead of appearing as one block after the
whole turn finishes. `POST /sessions/{id}/messages` (`internal/daemon/
server`) always responds over SSE now — a play-by-play of the agent loop
(`internal/daemon/agent`'s `EventFunc`: text deltas, tool start/result,
notices) as they happen, ending in one `done` event — instead of a single
JSON blob returned after everything completes. The daemon itself now runs
its gateway calls through `gatewayclient.ChatCompletionStream` rather than
the buffered `ChatCompletion`, and the gateway's own streaming responses
(`internal/server/chat.go`) carry `provider`/`attempts`/`usage`/
`tool_calls` on the terminal chunk so the daemon never needs a separate
non-streaming request to get them. While a reply is streaming in, its text
renders plain (no Glamour) — parsing a markdown string mid-formation (an
unclosed code fence, a stray `**`) would flicker through broken rendering
every frame — the full markdown render happens once, when the message
completes. Tool calls show a live spinner from the moment they start,
switching to ✓/✗ when their result lands, not just at the very end.

```bash
go run ./cmd/cli -daemon http://127.0.0.1:20130 -gateway http://127.0.0.1:20128
```

- Messages are laid out like an actual chat: yours anchored to the right,
  Kram's replies on the left — not everything stacked in one left-aligned
  column.
- The mouse wheel scrolls the transcript. Mouse mode has to be enabled for
  the footer-icon click below, which takes the wheel away from the
  terminal's own scrollback — so it's reimplemented against the viewport
  directly rather than just losing it. Text selection is unaffected: every
  mainstream terminal (GNOME Terminal, kitty, Alacritty, foot, xterm) lets
  you hold Shift while dragging to bypass an app's mouse capture, same as
  it would for any other TUI.
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
  breakdown (messages, tool definitions, project context, compaction
  summary, free space)
  computed by `GET /sessions/{id}/context` the same way the daemon decides
  when to compact (`internal/daemon/compaction`), so the panel and the
  actual compaction trigger never disagree. Only real categories Kram
  actually has — no invented "skills" or "MCP tools" line items.
- `ctrl+c` quits.
- Launch without `-session`/`-title` and the CLI opens on a **session
  picker** instead of always starting a new one — durable sessions
  (component 2) stay reachable across restarts instead of getting buried.
  `↑`/`↓` to move, `enter` to resume the selected session or create a new
  one (optional title prompt, `enter` to skip it, `esc` to cancel back to
  the list). Passing `-session <id>` or `-title <name>` skips the picker
  entirely, for scripting.

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
- **Tools** (`internal/daemon/tools`), 16 total: `read_file`, `write_file`,
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
  back up after a restart), and `memory_write`/`memory_search` (see
  "Memory" below). Every file/shell tool is confined to the daemon's
  `-workspace` directory — a path that would escape it is rejected before
  touching the filesystem.
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
- **Project context**: an `AGENTS.md` (or `CLAUDE.md`) at the workspace
  root is read fresh and injected as a system message on every turn — not
  persisted into history, so editing it takes effect on the very next
  message rather than requiring a restart. Finding this bug is what
  surfaced a real one: Anthropic accepts only one top-level `system`
  field and Gemini only one `systemInstruction`, so a second system
  message (a compaction summary, say) was silently clobbering the first
  before this was fixed — both adapters now concatenate instead.

Every message, tool call, tool result, and compaction summary is
persisted through the same durable store as component 2 — an agent run
survives a daemon restart exactly like a plain conversation does.

## Memory

Session history (component 2) is durable but session-scoped — a new
session starts with no memory of past ones. `internal/daemon/store`'s
`memory_entries` table plus a `memory_fts` FTS5 virtual index (external-
content, kept in sync with insert/delete/update triggers) add a second,
cross-session layer, inspired by
[akitaonrails/ai-memory](https://github.com/akitaonrails/ai-memory) but
scaled down deliberately:

- **Agent-curated, not auto-captured.** The model calls `memory_write`
  itself to save a compiled fact, decision, or preference — Kram never
  scrapes raw conversation into memory automatically. This keeps entries
  short and intentional instead of accumulating noise that has to be
  pruned later.
- **Two scopes**: `project` (the current workspace path) and `global`
  (`store.GlobalScope`, literally `"_global"`) for things true across
  every project, e.g. a user's name or preferences.
- **Automatic injection**: at the start of every turn, the agent loop
  (`Service.recentMemoryMessage`) pulls up to 8 entries — pinned first,
  then most-recently-updated — across both scopes and prepends them as a
  system message, so a brand-new session already has this context before
  the user says anything. The same method backs the context-usage panel's
  `memory` category, so what the panel reports and what actually gets
  sent to the model can never disagree.
- **`memory_search`**: an FTS5 full-text query tool for anything not
  already covered by the automatic top-8 injection — the model reaches
  for this itself when it needs older or more specific context.
- **No embeddings, no decay, no MCP surface, no git-backed markdown
  source-of-truth.** ai-memory's hybrid RRF retrieval and durability
  model are out of scope for this v0 — SQLite FTS5 is the only retrieval
  path, and entries live only in the daemon's existing SQLite store.

Verified end-to-end with the disposable mock provider (see below): a
memory written in one session was independently confirmed in the SQLite
`memory_entries`/`memory_fts` tables, and a second, brand-new session's
`/sessions/{id}/context` response showed a non-zero `memory` category
before any message was sent in it — proving the injection is automatic,
not dependent on the model deciding to search.
