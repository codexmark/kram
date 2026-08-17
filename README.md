# kram-gateway

An OpenAI-compatible LLM gateway: load balancing, circuit-breaker fallback
and per-provider telemetry across multiple upstream providers, in a single
static Go binary. v0 scope only — no compression pipeline, no persistence,
one routing strategy (round-robin).

This is the first component of **Kram**, a coding-agent platform being
built from scratch in Go, aimed at performance, reliability ("never
crash") and token economy.

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
  source of truth. The next component of Kram (a daemon) will follow this
  model.

No code from any of the three is reused; only the architectural ideas are.

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
- `ctrl+c` quits.
