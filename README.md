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
