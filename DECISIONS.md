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

### Combos v2: strategy, attempt execution, and response gate are three different concerns

The router used to answer one question — "which providers, in what order"
— and `internal/server/chat.go` did everything else inline: call each in
order, treat any non-error response as good, done. That conflated three
genuinely different decisions into one loop:

1. **Routing strategy** (`internal/router`'s `Strategy` interface):
   decides who to try and in what order.
2. **Attempt execution** (`chat.go`'s `handleChatCompletions`): actually
   calls each ranked candidate until one is accepted.
3. **Response gate** (`gate.go`, `stream.go`): decides whether a
   technically-successful response is actually good enough to end the
   fallback chain.

**Why split them:** the old loop couldn't tell "HTTP 500" apart from
"HTTP 200 with an empty body" — both just meant "try the next one." Once
gating became its own concern (see "Response gate" below), it had to stop
living inside the same loop that decides ordering, or every new strategy
would need to know about gating too.

### `Strategy.Rank` returns a full ranking, never just a winner

```go
type Strategy interface {
    Name() string
    Rank(ctx RouteContext, candidates []Candidate) []RankedCandidate
}
```

**Why a ranking and not a single pick:** the fallback chain needs an
ordered list to fall through when the leader fails or gets rejected — a
strategy that only picked a winner would force the executor to re-ask it
"okay, now who's next" after every failure, which is worse for stateful
strategies (round-robin's cursor, weighted's sticky pin) than computing
the whole order once. It also directly enables explainability (see "UI
route bar" below): the CLI can show `gemini .842, openai .816` for
candidates that were never even attempted, because the full ranking — not
just the one that won — is already sitting in the response.

### Package layout: one package, many files — not `router/strategy`, `router/scoring`, `router/acceptance` subpackages

`internal/router` has ~15 files (`router.go`, `candidate.go`,
`context.go`, `trace.go`, `strategy.go`, `priority.go`, `roundrobin.go`,
`affinity.go`, `weighted.go`, `factors.go`, `normalize.go`, `presets.go`,
`sticky.go`, `lkgp.go`, `p2c.go`, `gate.go`, `stream.go`) but is still one
package.

**Alternative considered:** subpackages (`router/strategy`,
`router/scoring`, `router/acceptance`) mirroring the shape a design
sketch for this suggested.

**Why one package:** `Strategy`, `Candidate`, `RouteContext`, and
`RankedCandidate` all need to be visible both to the parent `Router` (to
construct and call strategies) and to every strategy implementation (to
be ranked). Splitting into subpackages that both need those types creates
a straightforward import cycle — the standard fix is a driver-registration
pattern (`database/sql` + blank-imported drivers), which trades one file
count problem for a different one: now the strategy names have to be
wired up via `init()` side effects and a blank import somewhere, which is
more machinery for a package this size to carry. Go's own standard
library organizes large single packages into many small files
constantly (`net/http` is ~40 files, one package) — that's the same move
here, and it keeps "small router.go" true without inventing an
abstraction boundary the code doesn't actually need yet.

### Strategies: priority, round-robin, prefix-affinity, smart/quality/fast/cheap/reliable (one weighted engine), lkgp, p2c, weighted

`""` and `"priority"` are aliases for the same declared-order strategy —
preserving the exact v0 default. `round-robin` and `prefix-affinity` are
the same logic as before, ported to the `Strategy` interface unchanged.
`smart`, `quality`, `fast`, `cheap`, and `reliable` are five *presets* of
weights over one `weightedStrategy` engine, not five separate
implementations — a custom `weights:` block starts from whichever
preset's own weights and only overrides the factors it mentions, so
`strategy: smart` with a single custom weight still has sensible values
for the other five. `lkgp` and `p2c` are standalone strategies for cases
that want that specific behavior without the rest of the weighted
machinery. An unrecognized strategy name is rejected at `Router.New` time
(a config error, not a silent fallback) — except the empty string, which
has always meant "declared order" and stays that way.

### Six real factors, never fabricated ones

`health`, `reliability`, `latency`, `quality`, `cache_affinity`,
`priority` — each backed by something Kram actually measures or an
explicit operator setting, never invented:

- **health** — circuit breaker state (closed vs. half-open; open is
  already excluded before scoring runs, see "Capabilities and breaker
  state are hard constraints" below).
- **reliability** — `telemetry.ProviderStats.SuccessRate`, real observed
  outcomes.
- **latency** — `telemetry.ProviderStats.AvgLatencyMS`, normalized
  *relative to the current candidate set* (fastest among those being
  ranked scores 1.0, slowest scores 0.0) rather than against a fixed
  millisecond threshold, since there's no universal "good" latency across
  every provider/model a user might configure.
- **quality** — `config.ProviderConfig.QualityHint`, an explicit,
  optional, operator-set 0..1 value. Kram has no real per-provider
  quality measurement of its own; this factor does not exist unless an
  operator sets it.
- **cache_affinity** — whether a candidate is the same provider
  `prefix-affinity`'s own hash would pick for this request, computed with
  the exact same `hashString` function — a real, reproducible signal, not
  an estimated cache-hit rate no one actually has.
- **priority** — declared position in the combo, linearly decaying from
  first to last.

**No-data policy:** a factor with no evidence for a candidate (zero
requests yet, no quality hint set) scores exactly 0.5 — neutral, neither
favored nor punished for being unproven. This is one fixed rule applied
everywhere, not a per-factor special case, so an untested provider
competes fairly on whatever factors *do* have data instead of losing by
default.

Weights are normalized (`normalizeWeights`) to sum to 1 regardless of
what scale they're written in — `{health: 30, reliability: 20, ...}`
summing to 100 works exactly like fractions summing to 1. All-zero or
negative-weight input falls back to an equal split across all six
factors rather than dividing by zero — this is what keeps a malformed
`weights:` block from ever producing NaN or a score that silently favors
nothing.

### Smart Sticky: a hard preference within one run, not a nudge

The weighted strategy family (not priority/round-robin/prefix-affinity/
lkgp/p2c) can pin a run to its winning provider across tool round-trips.
"Run" is identified by `RouteContext.RunKey`, propagated end to end from
the daemon's `Service.Run`/`RunTask` (one opaque ID per run, generated
once, sent on every model call it makes as the `X-Kram-Run-Id` header —
see `gatewayclient.WithRunID`) through to the gateway, which falls back
to the stable system+first-user-message prefix hash (`AffinityKey`) only
when a caller sends no header at all — a generic OpenAI-compatible client
that doesn't know about Kram run IDs. While the pinned provider is still
present in the eligible candidate set (not circuit-open, still
capability-compatible), it leads the ranking regardless of score; only
losing eligibility, or the ResponseGate/StreamGate rejecting its
response, moves the pin — and moving it means whoever wins the retried
request becomes the new pin, via `Router.RecordOutcome`, called only once
a request has *actually* finished (see "Streaming success is decided by
the terminal event, not the first byte" below).

**Found as a real bug, reported by the user via GitHub issue, after
shipping Combos v2:** `RunKey` originally *was* `AffinityKey` directly —
the same key `prefix-affinity`/cache-affinity scoring use. That's stable
across an entire persisted conversation, not just one run: a later,
unrelated user turn in the same session still starts with the same
system+first-user-message prefix, so it inherited the previous run's
sticky pin instead of getting a fresh initial ranking. `AffinityKey`
itself is still exactly what prefix-affinity/cache-affinity need — a
stable prompt-prefix identity across an agent run's tool round-trips —
and keeps working unchanged; `RunKey` is what had to become a genuinely
different, narrower-lifetime concept.

**Why this matters for Kram specifically:** a single user turn can be
many model calls (read a file, grep, edit, run tests, answer) — rotating
providers on every one of those tool round-trips throws away prompt cache
warmth, changes latency/behavior unpredictably mid-task, and re-pays full
price for a near-identical prefix repeatedly. Sticky trades a small amount
of provider diversity for exactly the economics/predictability an agent
loop's tool round-trips want. Default `true` for the weighted family
(configurable per combo via `strategy_options.sticky`); the free-tier
example config explicitly sets it `false`, since spreading load across
rate-limited peers matters more there than cache warmth.

Sticky's own state (`stickyStore`) is a bounded, in-memory map
(`stickyMaxEntries = 256`, oldest-evicted-on-overflow) — no TTL sweeper
goroutine. A pin for an abandoned session costs a little memory, never
correctness, and isn't worth a background cleanup process at this scale.

### LKGP is a modifier first, a strategy second

"Last known good provider" is implemented once (`lkgpStore`, in-memory,
per combo, gateway-process lifetime — deliberately not persisted to disk,
since the gateway normally starts and stops together with the rest of
Kram) and used two ways: as a standalone `lkgp` strategy (priority order,
whoever last won moves to the front), and as an additive score boost
inside the weighted engine (`strategy_options.lkgp_boost`, default 0.10)
on top of whatever the six factors already computed. A circuit-open LKGP
can never win from either form — it's excluded from the candidate set
entirely before any strategy, including LKGP's own ranking, ever runs
(see "hard constraints" below).

### P2C: two random samples and a cheap comparison, not a load balancer

`p2cStrategy` samples two eligible candidates uniformly at random,
compares them with a small health/reliability score (not the full six-
factor engine — the entire point of Power of Two Choices is being cheap),
and promotes the better one to lead; everything else stays in declared-
priority order behind it. This is a well-known technique for avoiding the
herd effect of "always pick the single best" at a fraction of full
scoring's cost — not a distributed load balancer, and not meant to become
one.

### Exploration: a small, capped chance to not pick the leader

`strategy_options.exploration` (default 0.03) gives a uniformly random
eligible candidate a chance to lead instead of the top-ranked one, so
providers that would otherwise never win don't go forever without fresh
telemetry. It is skipped entirely whenever Sticky has already pinned a
leader for this run, never fires with fewer than two candidates, and
never selects a circuit-open or capability-incompatible candidate (both
already excluded before scoring). Deliberately small by default — this is
a safety valve against telemetry staleness, not a bandit algorithm.

**Found as a real bug, reported by the user via GitHub issue:**
exploration originally ran unconditionally *after* Sticky applied — the
code comment even claimed "never overrides sticky," but the ordering
alone didn't guarantee that, since exploration's `moveToFront` ran
regardless of whether Sticky had already moved a different candidate
there. With the default 3% rate, a healthy sticky-pinned provider could
still be randomly displaced on any later model call in the same run,
churning providers and losing prompt-cache warmth for no reason. The fix
tracks whether Sticky actually applied and skips exploration entirely
when it did — Option B from the issue (disable exploration whenever a
valid pin exists) over reordering the two checks, since it also avoids
computing an exploration event that could never have affected the
outcome anyway.

### Capabilities and breaker state are hard constraints, never scores

`eligibleCandidates` filters the candidate list down to circuit-allowed,
capability-compatible providers *before* any `Strategy.Rank` call — a
provider without tool support for a tool-using request, or with an open
circuit breaker, is simply absent from what a strategy ever sees, never
merely scored low. This was a real, confirmed gap in v0: nothing
cross-referenced `req.Tools`/image content against
`SupportsTools()`/`SupportsImages()` at the routing layer at all before
this. Health's *scoring* factor (see above) only ever distinguishes among
candidates that already passed this filter — a half-open breaker scores
lower than a closed one, but "open" never reaches scoring to begin with.

### Response gate: technical validation, never a second-guess of a legitimate refusal

`ResponseGate.Evaluate` runs deterministic checks — `reject_empty`,
`require_terminal`, `min_content_length`, `forbidden_substrings` — against
a fully-received response, entirely opt-in (an unconfigured combo accepts
everything, exactly v0's behavior). It exists to catch empty/truncated/
masked-error responses, never to route around a model's genuine safety or
policy refusal: there is no "did the model say no" check anywhere in it,
and there never should be — a refusal is normal, substantial text like
any other answer as far as the gate is concerned.

A rejection still counts as an operational failure for breaker/telemetry
purposes (a provider that chronically returns short or empty content is
not healthy, even though every individual HTTP call "succeeded"), but
`AttemptInfo.Outcome` keeps `OutcomeRejected` visibly distinct from
`OutcomeError` — an HTTP 500 and an HTTP 200-with-rejected-content are
different findings, and the wire format says so explicitly now instead of
collapsing both into `OK: false`.

### Bounded streaming peek: fallback is still possible after `stream: true`

Once a streamed response's headers are sent, no further fallback is
possible for that request — the client is already receiving bytes.
`router.BoundedPeek` buffers a small, bounded prefix of a candidate's
stream (`streamPeekMaxEvents = 8` events, `streamPeekTimeout = 5s`) before
the executor ever commits to relaying it onward, looking for a
*meaningful* signal: a real non-empty text delta, or a terminal event
carrying tool calls. "Received some bytes" is deliberately not sufficient
— a role-only opening chunk or an empty delta would otherwise look like
progress. If the peek doesn't find a meaningful signal (error, empty
close, timeout, buffer exhausted), the attempt is treated as rejected and
the executor moves to the next ranked candidate, exactly like a buffered
response's gate rejection. If it does commit, every buffered event is
replayed to the client in order before continuing to read fresh events
from the source — the client never knows a peek happened.

### Streaming success is decided by the terminal event, not the first byte

`streamResponse` (`internal/server/chat.go`) is the single place a
committed streaming attempt's real outcome is decided and reported —
`router.RecordOutcome`, breaker success/failure, and the trail entry's
`Outcome`/`OK`/`Reason` all come from one `finalize` closure, called
exactly once per attempt, only once the stream has actually reached one
of three real terminal states: a valid `Done` (success), an explicit
`evt.Err` (failure), or the upstream channel closing on its own without
ever sending either (also failure — see below). Nothing about success is
decided at commit time anymore; `BoundedPeek` committing only proves a
meaningful *first* signal, never that the request will finish.

**Found as two related real bugs, both reported by the user via GitHub
issues, after shipping Combos v2:**

1. The executor used to record `OutcomeSuccess` and call
   `RecordOutcome(..., true)` immediately after `BoundedPeek` committed —
   before the stream had actually finished. A provider whose first delta
   looked fine but that then errored mid-stream could still become the
   Sticky/LKGP winner, and the terminal SSE chunk's `Attempts` trail kept
   showing `success` even though the same request's `finish_reason` was
   `"error"` — genuinely contradictory state visible on the wire.
2. If the upstream channel simply closed on its own after commit —
   without ever sending a terminal `Done` or an `Err` — the old loop just
   ended and Kram still wrote a bare `data: [DONE]\n\n`, indistinguishable
   from a normal finish to any client reading the stream. A truncated
   answer looked like a clean success. This directly undermined
   `ResponseGate.RequireTerminal`'s whole purpose (already enforced on the
   buffered path via `drainToBuffer`'s `sawTerminal`), which simply wasn't
   checked at all once a stream had committed.

Both are fixed by the same `finalize` path rather than two separate
patches, per the issues' own explicit request to avoid duplicate or
competing finalization logic — an abnormal close is treated exactly like
an explicit upstream error (`OutcomeError`, an explicit `finish_reason:
"error"` terminal chunk, no Sticky/LKGP success update), just with a
different `Reason` string ("stream closed without a terminal
completion").

### `AttemptInfo` gained real fields, kept the old ones

`Outcome` (`trying`/`success`/`error`/`rejected`/`skipped`), `Reason`,
`HTTPStatus`, `Score`, `Attempt`, and `Model` are new; `Provider`, `OK`,
and `LatencyMS` are unchanged, and `OK` still equals
`Outcome == OutcomeSuccess` — anything that only ever read the old
two-field shape keeps working. `HTTPStatus` comes from a new
`provider.HTTPError` type the three adapters (anthropic, gemini,
openai-compat) now return instead of a plain `fmt.Errorf`-wrapped string,
so a real upstream status code survives the layers between the adapter
and the wire response instead of only living inside an error message
string.

### `RankedProviderInfo`: every candidate's score is visible, not just who was tried

`ChatCompletionResponse`/`ChatCompletionChunk` gained `Ranking
[]RankedProviderInfo` and `Strategy string` — the *entire* ranking a
scoring strategy produced, including candidates the fallback chain never
actually reached, plus each one's per-factor score breakdown
(`ScoreFactor{Name, Weight, Value, Contribution}`). This is a pure,
lossless projection of what the router already decided
(`router.ToRankedProviderInfo`) — nothing about a score is computed twice,
and nothing here is invented for display purposes. This is what lets a UI
show `gemini .842, openai .816` as context even when only the top
candidate was ever called, and what section-19-style score-breakdown
explainability (health 30% × 1.00 = .300, ...) reads from directly instead
of recomputing.

### `config.StrategyOptions`/`ResponseGateConfig`: fully optional, backward compatible

A `combos:` entry with just `strategy: prefix-affinity` and a `providers:`
list — the entire v0 shape — still loads and behaves identically.
`strategy_options` and `response` are both new, both optional blocks; every
field inside them (`Sticky *bool`, `LKGPBoost *float64`,
`Exploration *float64`, `Weights map[string]float64`, plus the gate's four
fields) defaults to "use the strategy's/gate's own sensible default" when
omitted, which is what lets `strategy: smart` alone (no options block at
all) behave well out of the box.

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

### A dead transport gets reconnected, with bounded backoff, not left dead

`Client.dispatch()` already noticed a dying transport (it unblocks every
pending caller when `Recv()` closes) but nothing acted on it. Now
`Manager` supervises every server it connected: a `Client.Done()` channel
(closed once, by `dispatch()`, whether the death was a crash or a
deliberate `Close()`) tells a per-server goroutine when to redial, with
backoff (1s, 2s, 4s, 8s, 16s, ... capped at 60s) and a hard stop at 5
attempts.

**Why capped, not infinite:** the same "one broken server costs only
itself" contract `ConnectAll` already established — a server that's
genuinely gone (uninstalled, its host is down) must not leave a goroutine
spinning on it for the rest of the daemon's life. It just goes back to
being absent, exactly like a server that failed to connect at startup,
recoverable by a daemon restart.

**Why the Manager's own child context, not the one `ConnectAll` was
given:** `Close()` needs the authority to unblock every supervisor
goroutine (including one mid-backoff-sleep) on its own schedule, without
depending on whoever owns the outer context to have canceled it yet — and
it cancels that child context *before* closing any client, specifically
so a supervisor never observes its own client's deliberate shutdown and
mistakes it for a crash worth reconnecting from.

### `tools/list_changed` updates a snapshot, read again only at the next turn

`dispatch()` used to drop every message with no `id` — spec-correct for
anything unrecognized, but that included this notification. It now
recognizes `notifications/tools/list_changed` specifically, re-fetches
`tools/list` in its own goroutine (so it doesn't block the dispatch loop,
which is itself needed to receive that fetch's response), and swaps
`Client`'s tool snapshot under a lock.

**Why this can't retroactively change a call already in flight:**
`Registry.Definitions()` is read fresh at the top of each turn's loop
iteration in `agent.go`, before that turn's model call is made — never
mid-call. `loadTools` *replaces* the snapshot (a new slice assigned in,
never mutated in place), so anything that already captured the old tool
list for the in-flight turn keeps its own independent copy regardless of
when the swap happens. The refresh lands whenever it lands; what changes
is only what the *next* `Tools()` call sees.

### Schema cache: one file, keyed by server name + a config fingerprint

`kramhome/mcp-cache.json`, not one file per server — at the scale of "how
many MCP servers does one workspace configure" (single digits), one file
is simpler to read/write/reason about atomically than a directory, with
no orphaned per-server files left behind when an entry is removed from
`mcp.json`. Keyed by a SHA-256 fingerprint over every connection-identity
field (kind, command, args, env, url, headers) — deliberately excluding
`Enabled`, since toggling a server off and on again doesn't change what it
would serve. 24h TTL on top of the fingerprint check, since a server can
change its own tool set without Kram's config changing at all.

**What's actually wired up:** every real `tools/list` (initial connect,
a successful reconnect, or a `tools/list_changed` refresh) writes an
entry. **What's deliberately not wired up:** nothing reads the cache to
decide whether `ConnectAll` can skip starting a server's process for pure
discovery — that would change `ConnectAll`'s "always dial every enabled
server, log and skip failures" contract, which this work didn't need to
touch. `cachedTools` is the seam a future lazy-discovery mode would call;
today it's exercised only by its own tests. Recorded here so it doesn't
read as a half-finished feature — it's infrastructure ahead of the need
that would consume it.

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

## Artifacts

### The producer, not just the reported size, had to be bounded

`bash`/custom tools used to capture output as `var out bytes.Buffer`, and
only checked a size cap *after* `cmd.Run()` returned. That cap bounded what
was reported, never what was actually held in memory while the command
was still producing output — a runaway command could inflate that buffer
arbitrarily before the cap was ever consulted.

**Fix:** `artifact.SpillWriter` is used directly as `cmd.Stdout`/`Stderr`.
It buffers up to a threshold, and the moment a write would cross it,
switches to streaming straight to a temp file for everything after — the
in-memory buffer provably never exceeds the threshold, at any point during
the command's entire run, not just at the end.

### Spilled output becomes an artifact, not a truncated-and-lost tail

Where the old cap just cut the output off (`[output truncated]`, the rest
gone forever), a result that crosses the threshold is now saved whole to
`.kram/artifacts/<id>.bin` (plus a `.json` sidecar of metadata — session,
tool, size, sha256), and the tool result becomes a short preview plus an
`artifact_read`-able reference. Nothing is lost; it's just not inline.

### `artifact_read` resolves ids, never paths

An artifact id (`art_<16 hex chars>`) is validated against a fixed pattern
before ever being joined into a filesystem path (`Store.paths`) — there is
deliberately no way to pass a real path through this tool. The model gets
a reference to something Kram itself created, never an arbitrary-path
reader.

### Deterministic filtering still runs first — but only on inline results

`filterCommandOutput` (see Tools, above) still applies to output that
stayed under the spill threshold. A spilled result skips it: filtering
exists to compress noisy multi-thousand-line output down to signal, and a
spilled result is already a short preview plus a reference, not something
that benefits from the same treatment.

### The aggregate per-turn budget is enforced by truncation, not retroactive spill

`maxTurnToolOutputChars` bounds the *combined* size of every tool result
within one model turn's batch of calls — several individually-fine
results (each under its own tool's cap) can still add up. This is
deliberately simpler than what a "spill the largest until under budget"
approach would need: buffering an entire turn's tool results before
persisting any of them, a bigger structural change to a loop this project
has already had one real silent-failure bug in (see Boundaries, "A weak
model's empty final answer..."). A result that crosses the aggregate
budget is truncated with an explicit notice — never silently dropped,
same discipline as everything else in this section.

### GC is best-effort disk hygiene, not correctness

`Store.GC` runs once per daemon startup (registry construction), deleting
artifacts older than 7 days. Not a background timer, not something a
long-running daemon needs to schedule — a workspace gets reopened often
enough in practice that "once at startup" is enough, and a garbage-
collected artifact simply becomes an unresolvable id if referenced later,
the same outcome as if it had never spilled.

---

## Workspace snapshots

### A second, isolated git repository as the storage engine

`internal/snapshot` gives Kram a way back if an agent breaks something:
`Store.Create` captures the workspace's current files, `Store.Restore`
brings them back. The engine is git — but a completely separate
repository, `--git-dir` pointed at `<workspace>/.kram/snapshots/.git`,
operating against the real workspace as its `--work-tree`. Every
operation (`add`, `commit`, `diff`, `reset --hard`) targets that isolated
`--git-dir` exclusively, via `os/exec` calling the system `git` binary —
same philosophy as `internal/daemon/tools/git.go`'s read-only
`git_status`/`git_diff`, and the same "external capability, explicit
subprocess" pattern MCP/LSP/custom tools already use.

**Alternative considered:** a pure-Go git implementation (e.g. `go-git`)
instead of shelling out. **Rejected:** it would be a new, fairly heavy
dependency for functionality the system `git` binary already provides
correctly, and this project already treats "shell out to a real binary"
as the default posture for external capabilities (shell, LSP servers, MCP
servers) rather than reimplementing protocols in Go where a battle-tested
binary exists. `go-git`'s `reset --hard`-equivalent semantics are also
less battle-tested than actual git's for this exact operation — being
this careful about "never touch the user's real repo" is not where you
want the least mature implementation of the mechanics doing the work.
This mirrors the MCP decision ("No SDK... it preserves the pure-Go,
static-binary, no-surprise-dependencies property") in spirit, but lands
on the opposite side, because here git the binary — not a Go
implementation of git — is genuinely the more reliable, better-tested
tool for the job, and the project already depends on git being present
for `git_status`/`git_diff`.

**Why this never violates "never touch the user's real git":** the
isolated repository shares a filesystem with the user's repo but nothing
else — different `--git-dir`, different index, different HEAD, different
branch, different identity (`user.email`/`user.name` set locally to this
hidden repo's own config only), different history. `git reset --hard`
against *this* `--git-dir` is not the dangerous command the invariant
bans — that ban is specifically about the user's own `.git`, which this
package never points at for any command. Verified directly, not just
argued: `snapshot_test.go`'s `TestSnapshotNeverTouchesUserGitState`
stages a real change in the user's real repo, takes a snapshot, restores
it, and asserts the user's branch, HEAD, staged blob, and `git status
--short` output are byte-identical before and after.

### The .gitignore question: respected, on purpose

Deciding what "the current state of the workspace" means for `Create`
had two options: capture literally everything (minus `.git`/`.kram`), or
respect the user's own `.gitignore`. **Chosen: respect it.** Concretely,
this falls out of the isolated-repo design for free — git discovers
`.gitignore` files by walking the `--work-tree`, which is the same
directory the user's own `.gitignore` files live in, regardless of which
`--git-dir` is doing the walking. `node_modules`, build output, `.env`,
and anything else the user already excludes from their own history stays
excluded from snapshots too.

**Why this is the right default:** it matches what `git status` already
tells the user is "their" content worth tracking, keeps snapshots small
and fast even in JS-shaped repositories, and means a `.env` with real
secrets doesn't get duplicated into a second, less-obviously-guarded
git history under `.kram/`. The tradeoff is real and worth naming: a
gitignored file (a local override config, a generated file the user
actually cares about recovering) is not protected by this mechanism at
all. `.git` and `.kram` are excluded unconditionally regardless of
`.gitignore` content, via the isolated repo's own `info/exclude` — not
delegated to the user's `.gitignore`, since a workspace that happens not
to ignore `.kram` must still never snapshot its own snapshot history.

### Restore over a stale snapshot: overwrite and report, never silent, never refuse

Between taking a snapshot and restoring it, the workspace can keep
changing — including via further snapshots. Two honest options existed:
refuse to restore if anything changed since, or apply it anyway.
**Chosen: apply it anyway, but never silently.** `Store.Restore` always
returns a `RestoreResult` naming every path it touched and how (`will be
overwritten` / `will be restored` / `will be removed`), computed via
`git diff --name-status` *before* the mutating `reset --hard` runs, and
the `snapshot_restore` tool's result text lists every one of them.

**Why not refuse on staleness:** detecting "has anything changed" well
enough to be trustworthy is closer to a three-way merge problem than a
timestamp check, and a refusal still needs a way to proceed — which just
becomes a second code path to get right. Reporting what changed, always,
does the actual job: nothing about a restore's effect is hidden from
whoever asked for it. This is consistent with the project's existing
"never silently drop or truncate" discipline (see Tools, "Deterministic
output filtering" and Artifacts, "Spilled output becomes an artifact").
Consistent too with the Permissions section's default posture
(compatibility default is Allow, not a blocking gate) — the mutating gate
for `snapshot_restore` is the permission engine every tool already goes
through by being registered normally, not a bespoke confirmation step
reimplemented here.

**A deliberate, narrower boundary, also tested:** `Restore` only ever
undoes what some snapshot *actually captured*. A file created in the
workspace after the most recent `Create` call, never captured by any
snapshot, is left completely alone by `Restore` — this falls directly out
of using `git reset --hard` against the isolated repo (it only touches
paths that differ between the isolated index and the target commit;
a file the isolated repo has no index entry for at all is invisible to
it, by git's own design). This is a feature, not a gap: Kram will never
delete a file it has no record of, the same conservative instinct behind
`delete_file` refusing directories. See
`TestRestoreLeavesNeverSnapshottedFileAlone`.

### Registered as normal tools, not a special-cased mechanism

`snapshot_create`/`snapshot_list`/`snapshot_diff`/`snapshot_restore` are
ordinary entries in `internal/daemon/tools`'s `Registry`, exactly like
every other tool — they inherit `permission.Evaluator`/`Approver` gating
automatically by virtue of being registered, same as the Permissions
section's "a policy layer is a different thing from enabled/disabled"
already guarantees for everything in the registry. No bespoke plumbing
was needed or added. `snapshot_restore`'s description states plainly
that it is destructive and requires an explicit id (never "restore the
latest") — the tool-level protection against acting on stale information
is that explicitness plus the always-reported change list, not a second
confirmation mechanism duplicating what the permission engine's `ask`
policy already exists to provide for exactly this kind of call.

### Known gap: no automatic snapshotting

This round implements the capability only — `snapshot_create` must be
called explicitly (by the model or the user), never automatically before
a mutating tool call. Auto-snapshotting before every `write_file`/
`edit_file`/`delete_file`/`bash` call was explicitly out of scope for this
round, to keep it small and let the primitive prove itself first. A
natural next step, not built: a policy-driven "snapshot before this
category of call" hook, likely living in `internal/permission` or the
agent loop rather than this package, so `internal/snapshot` itself stays
a pure capability with no opinion about when it's used.

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

### The right-aligned prompt block is a narrower exception, not a reversal of the reversal

Combos v2 gave the user's own message a right-aligned block (a small
"you" label, a colored vertical accent on the left edge, width shrinking
to fit short content) — worth being precise about why this doesn't
contradict the entry above.

**The distinction:** the earlier reversal was specifically about
*two-sided chat bubbles* — full boxes on both sides, agent left / user
right, reading like a messaging app. That's still rejected. What's new is
an asymmetric exception for the user's own prompt *only*: no full border,
no tail, no background card spanning the width, and Kram's own replies
stay exactly as they were (left-aligned, unstyled, integrated into the
transcript). The goal is hierarchy — instantly telling apart "what I
typed" from "what Kram said" — not turning the transcript into a
conversation UI.

**The bug this reversal already caught stayed fixed:** the block's width
is computed from the actual wrapped content (via `reflow/wordwrap`, then
measuring the longest resulting line), never from `lipgloss.Style.Width()`
— avoiding exactly the "short content padded into a wide invisible box"
mistake recorded above.

### Nothing in the UI is simulated

The footer's latency, fallback trail, breaker states and token counts all
come from the gateway's own counters. The context panel is computed by
the same code path that decides when to compact. Combos v2 extends this
same rule to routing: the route bar, the Ctrl+R trace, and Ctrl+P's score
breakdown all render facts the router/gateway already decided — nothing
is guessed, interpolated, or recomputed in the TUI. The one deliberate
exception is a generic "routing…" pulse while a model call is actually in
flight, and even that's disclosed as a generic pulse rather than faked
per-attempt progress (see "Route bar: per-model-call granularity, not
per-attempt" below) — there was never a version of this that simulated
which candidate was "currently" being tried.

**Why:** a panel that can disagree with reality is worse than no panel. It
also means the two can never drift.

### Route bar: per-model-call granularity, not per-attempt

The route bar shows a generic "routing…" pulse while a model call is in
flight, then the real fallback trail once it's done — it does not show
"now trying candidate 2 of 3" live, mid-call.

**Why this isn't a shortcut:** it's structurally true, not a scope cut.
The gateway's fallback loop (try candidate 1, try candidate 2, ...) runs
entirely inside a single HTTP round-trip the daemon only ever observes
the *result* of. For a streaming request specifically, no bytes are even
sent to the client until a candidate's `router.BoundedPeek` commits — a
failed early candidate never gets an open connection to report progress
over in the first place. Showing fake "now trying X" progress would be
exactly the kind of simulation the interface principle above forbids.
Real per-attempt live events would require the gateway itself to push
progress over a channel that exists before any candidate has committed,
which is a materially different transport (a second connection, or a
protocol upgrade) — deferred, not silently dropped; see "Known gaps."

### Live routing events are per model call, not a new event bus

`route_start`/`route_done` reuse the exact SSE/event-callback machinery
`tool_start`/`tool_result`/`notice`/etc. already use — no new transport,
no second WebSocket, no polling endpoint. `route_done` carries one
`RouteCall` (see below); the final `done` event carries the whole
`RouteTrace` for the completed turn.

### `RouteTrace` fixes a real bug: only the last model call's trail was ever visible

`agent.go`'s turn loop used to do `result.Attempts = callResult.Attempts`
on every iteration — a turn with several tool round-trips (read a file,
grep, edit, run tests, answer) silently discarded every earlier model
call's real fallback trail except the very last one. A user watching a
multi-step task would only ever see routing information for the *final*
answer, never for the four model calls that preceded it, even though
those all really happened and may have had real fallbacks of their own.

`RunResult.Attempts` is kept exactly as it was (the last call's trail —
still what the plain footer would use, before the footer itself dropped
routing detail entirely, see below); `RunResult.RouteTrace` is the new,
complete accumulator (`agent.RouteTrace`/`agent.RouteCall`), and Ctrl+R
is what makes it visible.

### Footer stops duplicating the route bar

The footer used to be two lines: a provider/latency/sparkline summary,
then a row of anonymous colored dots (one per attempt) plus a count.
Once the route bar showed the *same* fallback trail with real provider
names, outcome glyphs, and latencies — strictly more information, in a
more prominent, always-visible position — the footer's routing detail
became pure duplication. The footer is now one line: token usage on the
left, the context-usage icon and keyboard shortcuts (including the new
`^r`) on the right. Detailed routing lives in exactly two places now: the
route bar (live summary) and Ctrl+R (full trace) — never a third,
redundant copy.

### Ctrl+R and Ctrl+P: the router's own data, rendered, never recomputed

Both panels are strict projections of data the gateway/router already
computed and sent over the wire (`openai.AttemptInfo`/
`RankedProviderInfo`/`ScoreFactor`) — see "Strategy explainability" in
the Routing section above for why the wire format carries the full
ranking (every eligible candidate, not just the ones actually attempted)
specifically so this is possible without a second scoring pass in the
TUI. Ctrl+P falls back to the pre-Combos-v2 provider-list/telemetry view
for non-scoring strategies (priority, round-robin, prefix-affinity),
which never produce a ranking — there's nothing to show a breakdown of.

### Two real bugs, found only by testing this live

Both were caught by actually running the CLI against real mock providers
in a tmux session and reading the output — not by any unit test, since
neither is the kind of thing a value-level test would think to check:

1. **Route bar spurious truncation at exact width.** `reflow`'s
   ANSI-aware `truncate.StringWithTail` unconditionally reserves room for
   its tail string, even when the input isn't actually over the limit —
   so a line landing at exactly the terminal width got a spurious `…`
   appended and its real last character silently dropped. Fixed by only
   invoking `truncate` when the rendered width genuinely exceeds the
   target (`lipgloss.Width(result) > m.width`), confirmed by a test that
   sweeps widths 10 through 140 against long real provider names.
2. **Ctrl+P's score breakdown silently truncated by panel height.** The
   provider-list header plus a full six-factor breakdown together
   routinely exceeded the shared panel height budget, so `padLines`'s
   `lines[:height]` cut the output off *before* the factor lines were
   ever appended — this looked exactly like the ranking data being empty
   end to end, and was diagnosed as a data bug for a while before a
   direct `curl` against the gateway's raw SSE output proved the wire
   format was always complete. Fixed two ways: the panel now shows a
   *focused* single-candidate breakdown instead of the provider list plus
   the breakdown at once when real ranking data exists (matching the
   original design mockup more closely, and using far less vertical
   space), and the shared panel height itself grew (`height/3`, floor 9,
   was `height/4`, floor 6).

Both are recorded here as a concrete argument for the "test live in a
browser/terminal before calling a UI change done" rule already in this
project's own operating instructions — a passing `go build` and `go vet`
told us nothing about either bug.

---

## First-run setup wizard

### The trigger is onboarding state, not "is a provider configured"

**Found as a real gap in the first draft of this feature.** The obvious
trigger — run the wizard when `detectGatewayConfig` would fail with "no
provider configured" — has a real hole: someone who already exports
`ANTHROPIC_API_KEY` would never see the wizard at all, and so would never
get asked about routing, permissions, or tools. It also means a user who
pastes one key via the wizard's provider step and then cancels *before*
finishing would silently never see the wizard again — that credential is
now enough to make `detectGatewayConfig` succeed on the next run.

The actual trigger is a small versioned marker
(`internal/onboarding`, `kramhome.Path("onboarding.json")`,
`{version, completed, projects_root, last_workspace}`): the wizard runs
when `!completed || version < currentVersion`, full stop, independent of
what's already configured. `-setup` forces it regardless of `completed`.
This is also what makes a version bump a clean way to force existing
installs through a redesigned wizard later — no migration logic, just a
fresh run.

### Two stages, one continuous-feeling flow

Steps 1-5 (Environment, Projects, Providers, Routing, Permissions) run in
a **second, standalone `tea.Program`** (`app.RunWizard`), entirely before
the gateway/daemon exist — they have to, since producing the config those
two need to start *is* what these steps do. Steps 6-8 (Tools & Skills,
System Check, Ready) run in the **normal post-daemon program**, entered
directly via a new `app.New` parameter instead of the session picker,
because listing what tools/skills are really registered needs a live
daemon connection (`GET /tools`) that can't exist yet during Stage 1.

Both stages share one `renderWizardHeader` (`N/8 Title`) so the split is
invisible to the user — matching the explicit design goal that the
wizard read as one flow, not "the setup ended and I fell into an
unrelated tools screen." The Providers step doesn't get a third phase of
its own; it reuses `phaseAccounts` (the existing accounts screen)
directly, gated by a `wizardMode` bool, since nothing in that screen's
render/handle path touches the daemon/gateway clients that don't exist
yet — confirmed before reusing it, not assumed.

### Provider validation is real, not inferred from "a key exists"

Each key paste or completed OAuth immediately triggers the same
`providerping` check the accounts screen already uses for its status
dots (`internal/providerping`, a minimal "list models" call — never a
full chat request), instead of waiting for a manual re-check. A
"Gateway mode: BASIC/RESILIENT" line reports genuine independent-account
count, explicitly not counting OpenRouter's three free-model *routes* as
three upstreams — they share one account and one rate limit.

### Permission presets are real `PolicyFile` values, checked against the real engine

`internal/permission` gained `RecommendedPolicy()`/`StrictPolicy()`/
`AutonomousPolicy()` plus `SavePolicy` — not a parallel, simplified rule
language for onboarding, the exact same `Rule{Tool, Pattern, Decision}`
matching described in "Permissions" above. Writing the three presets
surfaced two things worth being explicit about in the wizard's own copy
rather than leaving as a surprise:

- Strict's `default: "ask"` means *every* unlisted tool asks, including
  every `mcp__*` tool — not just tool names the preset doesn't mention.
- Autonomous's one safety rule (`bash`, pattern `"rm -rf /*"`, deny) is a
  prefix glob, so it blocks any `rm -rf` on an absolute path
  (`rm -rf /home/x`, not just literally `rm -rf /`) while leaving
  relative-path deletes (`rm -rf node_modules`) alone — a real behavior
  test (`internal/permission/presets_test.go`) caught the difference
  between "blocks one literal command" (what the first draft's comment
  claimed) and "blocks a whole class of absolute-path deletes" (what the
  prefix-glob engine actually does) before it shipped as a doc lie.

### Config precedence gained a global tier, and the port-reuse trap that comes with it

`loadOrDetectGatewayConfig` now resolves, in order: explicit `-config` →
`<workspace>/.kram/config.yaml` (a per-project override, hand-written or
future-wizard-generated — nothing writes this automatically today) →
`kramhome.Path("config.yaml")` (what the wizard actually writes) → plain
env-var autodetection. Both new file tiers deliberately do **not** trust
whatever port is written in the file — they call `freePort()` just like
plain autodetection does, unless `-gateway-port` was explicit. A global
config is shared across every workspace; reusing its file's port would
let two `kram` instances in two different project directories collide
trying to bind the same one, a real regression an early draft of this
change would have introduced.

`config.Save` writes with a temp-file-then-rename (removing any existing
file first, since `os.Rename` fails over an existing target on Windows,
unlike POSIX) rather than truncating the destination in place — a reader
should never be able to observe a half-written config. The generated
file also always sets `response: {reject_empty: true, require_terminal:
true}` on the wizard's combo — a safe default for a brand-new install,
never retrofitted onto a config the wizard didn't generate.

### Two real bugs found only by running the wizard in tmux, not by `go build`/`go vet`/tests

1. **`syncViewportSize` panicked in wizard mode.** `Update`'s
   `tea.WindowSizeMsg` case unconditionally called
   `m.input.SetWidth(...)` on the chat composer — but `newWizardModel`
   never calls `textarea.New()` for Stage 1 (there's no message composer
   in the wizard), leaving `m.input` a zero-value `textarea.Model`.
   `SetWidth` on that zero value panics. Every unit test, `go build`, and
   `go vet` passed; only starting the actual binary in tmux crashed
   immediately on the first resize event. Fixed by skipping
   `syncViewportSize`/markdown-renderer setup entirely when
   `wizardMode` — Stage 1 never renders any of that.
2. **The Ready screen showed "Routing: auto" and a blank "Permissions"
   row no matter what was actually chosen.** Stage 1 and Stage 2 are
   *separate* `Model` instances in separate programs — Stage 2's summary
   step was reading `m.wizardChosenStrategy`/`m.wizardChosenPermPreset`
   off its own, freshly-constructed `Model`, which never inherited
   anything Stage 1 decided. `go build` had no way to catch this: both
   fields exist, both are valid strings, they're just always empty on
   the model that was actually rendering. Only watching the real summary
   screen (mid-manual-walkthrough, with `smart`/`recommended` chosen
   moments earlier) revealed it showing the zero-value fallback instead.
   Fixed by threading a small `WizardResult` (Stage 1's actual choices)
   through `app.New` as a new parameter, read only when
   `openOnToolsPreset` is true.

Both are recorded here as further evidence for the same rule the route
bar/score-breakdown bugs above already argued for: a passing test suite
proves the code is internally correct, not that the feature behaves
correctly end to end. This wizard was walked through fully in tmux,
including a real OpenRouter account, before being called done — both
bugs were caught that way, neither by static checks.

### What Kram does *not* do here, on purpose

No shared/relay API key ships with Kram, and no session gets bootstrapped
against another product's hosted service. OpenCode Zen (one of Kram's own
catalog providers) was checked directly and requires billing details to
sign up — not a genuine zero-friction option — so it isn't presented as
one. OpenRouter's already-working, per-user OAuth flow (no card, a real
key, Kram's own rate limit exposure is zero) is the wizard's actual
"click one button" path. Building and hosting a shared relay the way a
company-backed product might is a real, ongoing cost/liability decision
for a solo maintainer — not something to introduce as a side effect of an
onboarding flow.

## Browser login for Anthropic and OpenAI (Claude Pro/Max, ChatGPT Pro/Plus)

The wizard's Phase 2 backlog (see above) named "OAuth/browser-auth for
providers beyond OpenRouter" as deferred. This closes that specific item
for Anthropic and OpenAI — Gemini and OpenCode Zen remain key-only, since
neither offers a subscription-login flow to build against.

The first pass shipped here got Anthropic's actual credential shape
wrong, and only real end-to-end testing against a live account (not
`go build`, not a fake authorization code) caught it — worth recording
in detail since it changed the design, not just a bug fix.

### Anthropic: the OAuth access token is not itself a usable credential — it mints one

The first implementation sent Anthropic's OAuth access token straight
through as `Authorization: Bearer` on every request, mirroring
OpenAI's flow. Live-tested against a real Claude Pro/Max account: every
request built that way — `/v1/messages`, `/v1/models`, ping included —
came back `403 permission_error`, `"OAuth token does not meet scope
requirement any_of(user:inference, ...)"`, even though `user:inference`
was explicitly requested in the authorize call's scope. An active Pro/Max
subscription didn't change this.

What actually works, also confirmed live against the same account: the
token's `org:create_api_key` scope is for exactly one thing — a single
POST to `https://api.anthropic.com/api/oauth/claude_cli/create_api_key`
(`Authorization: Bearer <oauth access token>`), which mints a real,
permanent `sk-ant-...` API key (`expires_at: null`) on the account that
just authorized Kram. That key works precisely like any pasted one —
confirmed by sending it as `x-api-key` against `/v1/messages` and getting
a coherent, unrelated error back (`"Your credit balance is too low"` —
an Anthropic Developer Console billing state, entirely separate from a
Claude Pro/Max chat subscription; the two are unrelated balances). So
`internal/oauthflow.AnthropicAuthorize` does the code exchange and this
create-key call back to back and hands back a permanent string, the same
shape `OpenRouterAuthorize` already returns — there is no refresh path
for Anthropic, and no second `Bearer`-auth mode in
`internal/provider/anthropic.go`. Both existed in the first pass and were
removed once this was understood; `internal/credentials.Store`'s
`OAuthToken`/`Resolve`/refresh machinery still exists, but Anthropic
never touches it.

### OpenAI's ChatGPT login really does need the refreshable-token machinery — and needed a fixed callback port

OpenAI's flow is the case that machinery was built for: the access token
*is* the usable credential (Bearer, straight to the Codex Responses
backend), but it's short-lived and must be refreshed — confirmed by the
same live-testing discipline, sending a fabricated code straight to
`auth.openai.com/oauth/token` and getting a coherent `token_expired`
rejection back rather than a client or shape error.

A second real bug surfaced the same way: the authorize URL was rejected
outright by `auth.openai.com` — `"invalid_authorize_request"` — visible
directly in a browser, before any login prompt. The cause was the local
callback listener's port: it was ephemeral (`net.Listen("...:0")`), but
OpenAI's authorization server validates `redirect_uri` against an
allowlist registered for this client_id, and the `opencode` client this
was extracted from hardcodes port `1455`, not a dynamically chosen one.
Kram's callback listener now binds that same fixed port — if something
else already owns it locally, the flow fails to start with a clear error
naming the requirement, rather than silently trying a different,
guaranteed-to-be-rejected one.

### OpenAI's ChatGPT login is a different product, not an alternate way to fill `OPENAI_API_KEY`

It only authorizes access to a separate, Codex-branded backend
(`chatgpt.com/backend-api/codex/responses`, the Responses wire format,
not Chat Completions) serving a restricted model set — never
`api.openai.com`. Presenting it as a second way to configure the existing
`openai` catalog entry would be a real correctness bug dressed up as a
convenience: the resulting token simply doesn't work there. It's its own
catalog entry (`openai-chatgpt`) and its own adapter
(`internal/provider/openai_responses.go`, `Kind: "openai-responses"`).

### Anthropic's client_id is real but unofficial — said so, not hidden

There is no public, documented OAuth client Anthropic offers third-party
tools. The `client_id` this uses (`9d1c250a-e61b-44d9-88ed-5944d1962f5e`)
is only known because it's been reverse-engineered from Anthropic's own
`claude` CLI by outside projects — not something extracted from `opencode`
(which, unlike its fully-implemented "login with ChatGPT" flow, turned
out to have no working Anthropic OAuth implementation anywhere in its
shipped bundle, despite showing the UI label for one). Both the
authorize/token exchange and the create-key call were live-verified
against a real account, not just probed with a fabricated code — see
above. Framed honestly in the accounts screen's label ("beta") rather
than presented as sanctioned, first-class support.

### The lesson this reinforces: `go build` proves nothing about whether an OAuth flow actually works

Every prior finding in this file about live-tmux-testing catching what a
clean build can't (see "Two real bugs found only by running the wizard
in tmux" above) applied again here, at higher stakes: a scope-mismatched
Bearer token and a rejected authorize URL both look identical to a
successful flow in source code, and both were only caught by sending
real requests to Anthropic's and OpenAI's real servers — first with
fabricated codes to sanity-check endpoints/shapes without needing a
login, then, for Anthropic, by working through the actual live account
data once a real connection had been made through the wizard.

## The default combo's fallback chain, diagnosed from real gateway logs

A real symptom from actual daily use, not a synthetic test: a wizard-
configured "RESILIENT, 4 upstreams" combo was failing *every* candidate
on most second-and-later prompts in a session, traced from the real
`kram.log` a live session produced (not guessed from source reading —
see "Two real bugs found only by running the wizard in tmux" above for
why that distinction matters here too). Six candidates, six different
failure reasons, three worth recording:

**Gemini's pinned model was retired out from under it.** `gemini-2.5-pro`
(the catalog's `DefaultModel`) 404'd on every single real generation
call: `"This model models/gemini-2.5-pro is no longer available to new
users."` The trap: `GET /v1beta/models` — the same endpoint
`providerping`'s Gemini check calls — still lists it, unflagged, as
supporting `generateContent`. A ping that only checks the model's
presence in that list, or a hypothetical future "fetch available models"
feature that trusted it, would both have kept reporting this as healthy.
`gemini-2.5-flash` turned out to be retired the same way when tried as a
replacement. Google's own `gemini-flash-latest`/`gemini-pro-latest`
aliases exist specifically to not rot like a numbered release does —
live-tested (a real streamed response, then a real tool call) before
pinning `gemini-flash-latest` as the new default.

**OpenRouter's free reasoning models were never actually broken — Kram
was.** `openai/gpt-oss-20b:free` and `nvidia/nemotron-3-super-120b-a12b:free`
failed with `"no meaningful content within the peek window"` on nearly
every attempt. Direct testing (`curl -N` against a real streaming
request) showed why: both are reasoning models that stream a chain-of-
thought (OpenRouter's `delta.reasoning` field) for several seconds
*before* any real answer content — and `internal/provider/openai_compat.go`
didn't capture that field at all, so every reasoning fragment produced
zero `StreamEvent`s. From `router.BoundedPeek`'s side, that reads as
total silence; its fixed 5-second-from-start timer fired before real
content ever arrived, and it gave up on a model that was actively working
the whole time. Fixed at both ends: `openai_compat.go` now forwards
reasoning fragments as `StreamEvent.Reasoning` (a new field, kept
strictly separate from `Delta` — reasoning is not the model's answer and
must never be relayed to a caller as assistant content), and
`BoundedPeek`'s timer is now an *idle* timeout that resets on any event,
reasoning included. An earlier version of this fix also added a fixed
overall ceiling on top of the idle timer, on the theory that a model
reasoning forever without ever answering needed a hard stop regardless —
removed again after real traffic showed it actively harmful: a genuine
120B-class reasoning model (OpenRouter's nemotron) legitimately took
longer than that ceiling on real prompts, not stalled, just still
thinking, and got rejected mid-answer. The real "never answers at all"
backstop already exists one layer down: every provider adapter's
`http.Client` carries its own 120s timeout, which doesn't fire while
tokens keep arriving — the right place for an absolute ceiling.

**The two failures compounded into "almost never two prompts in a row."**
With Anthropic permanently unusable (see "Browser login" above — no API
credit balance) and Gemini permanently 404ing, only four of six
candidates were ever real options; two of those four were being killed
by the peek-timeout bug on nearly every attempt, and the remaining two
are OpenRouter's genuinely shared, genuinely rate-limited free capacity.
After live-verifying the fixes (three consecutive real prompts against
the actual configured combo, zero provider failures in the log, each
answered by the first candidate tried), that reduces to: one config
correction plus one real bug, not six independent flakes.

### A menu of available models would not have caught the Gemini failure on its own

Asked directly, and worth recording: Gemini's own `ListModels` response
listed the retired `gemini-2.5-pro` as supporting `generateContent` with
no warning — the "menu" itself was wrong, not just unconsulted. A
same-session check of OpenRouter's `/models` listing for the three
pinned free-tier slugs found no equivalent problem there — all three are
listed accurately (correct free pricing, correct tool support) and
degrade for real reasons (rate limits, the reasoning-timeout bug above),
not stale/wrong listings. So a future "discover available models
automatically" feature is a real, reasonable direction, but it can't be
"trust the list" — at least for Gemini, it would need an actual
generation-capable liveness check per candidate model, not just
presence in a catalog response, or it would inherit exactly this bug in
a more automated, harder-to-notice form.

### Gemini's tool-calling protocol had two more real bugs, both found from a real "all providers failed" log after the fixes above

Reviewing a fresh real session log (after the fixes above had already
shipped) showed a *different* Gemini failure — `400 Bad Request`, no
longer the 404 from the retired model. Traced the same way as
everything else in this section: reproduce the exact wire request the
real conversation would have sent, against the real API.

**`role: "function"` isn't valid.** `buildGeminiContents` sent tool
results as `{"role": "function", ...}`, matching Kram's own internal
"tool" role name — Gemini's real API rejects it outright: `"Role
'function' is not supported... USER, ... MODEL."` Fixed to `"user"`,
which is what Google's documented convention actually is; confirmed
live, and confirmed separate (non-grouped) turns per tool result — one
`user`-role content per result, matching Kram's existing per-message
structure — are structurally accepted, so no restructuring was needed
beyond the role name itself.

**Thinking-enabled models require their `thoughtSignature` echoed
back.** Past that, a second, different 400 appeared:
`"Function call is missing a thought_signature."` Gemini's newer
"thinking" models (the "latest" aliases pinned above default to this)
attach an opaque signed blob to each `functionCall` part they emit, and
require it echoed back unchanged on that exact call in later turns —
skip it and the request is rejected outright. This is genuinely new
provider-specific state with nowhere to live in Kram's existing
pipeline, so `openai.ToolCall` gained a `GeminiThoughtSignature` field
— it round-trips for free through the daemon's session storage, which
already `json.Marshal`s `[]openai.ToolCall` directly with no
intermediate stripped-down type. `gemini.go` now captures it off each
streamed `functionCall` part and re-attaches it when rebuilding history
for the next request.

Both were live-verified with the real wire format before writing any
Go — first via direct `curl` reproduction with a real, previously-issued
signature (confirmed a real `200` only once the role was `user` *and*
the real signature was echoed back correctly), then via the actual
built binary: a fresh session, forced onto a Gemini-only combo, given a
prompt that triggers three genuinely parallel tool calls
(`list_dir`/`git_status`/`read_file`) — all three ran and Gemini
correctly synthesized the final answer from their results, the exact
shape of turn that used to 400 on every attempt.

## Custom providers: a user-registered OpenAI-compatible endpoint (local/LAN servers)

`internal/providercatalog` was a fixed, compile-time list — no way to
point Kram at a local server (llama.cpp, LM Studio, Ollama's OpenAI
endpoint, vLLM) without hand-editing a generated `config.yaml`.
`internal/customprovider` adds a small user-editable store (name, URL,
optional model) reachable from the accounts screen's new "+ adicionar
provedor customizado" row — multiple entries, each independently named,
since "servidor de casa" and "servidor do trampo" are both real
simultaneous cases. The API key is optional (most local servers have no
auth) and, when present, is *not* stored in this new package at all — it
reuses `internal/credentials.Store` under a synthesized env var
(`CUSTOM_<ID>_API_KEY`), the same pattern an OAuth-connected account's
synthetic env var already established. One place for every secret.

### A custom provider with no key crashed the entire gateway, not just that one provider

Live-tested (see below) with a real no-auth local server and caught
immediately: `config.ProviderConfig.APIKey()` treated an unset
`APIKeyEnv` as a hard error unconditionally, which every provider path
before this one could safely assume never fires — `detectGatewayConfig`
only ever included a *catalog* provider when its env var was already
confirmed non-empty, so the error branch was structurally unreachable
for them. Custom providers break that assumption on purpose (existence
in the store, not a populated env var, is what "configured" means for
one — see the optional-key design above), and `provider.Build` calling
`APIKey()` for one with no key turned a "log a warning and skip this one
provider" situation into `gateway.Run` returning an error before
`ListenAndServe` — the whole gateway, and with it the whole `kram`
process, never came up. Fixed with a new `ProviderConfig.KeyOptional`
field, set only on custom-provider entries, so every other provider path
(catalog or a hand-written config.yaml) keeps today's strict behavior —
an unset required env var stays a clear startup error, not a silently
missing credential discovered later as a confusing 401.

### Live-verified against a real LAN server, not just the bundled mock

`devtools/mock-provider` (a real local OpenAI-compatible server already
in this repo, built for exactly this kind of testing) confirmed the
basic flow, but the user supplied a real server on their own network
mid-verification, which ended up testing more of the real path at once:
registered it through the actual accounts screen (name "lab", a real
`http://192.168.x.x:20128/v1` URL, a real key, later a pinned model), it
pinged green at real latency, and — forced onto a single-provider combo
to guarantee the fallback chain couldn't route around it — a real chat
turn completed through it end to end. This is also what surfaced the
`KeyOptional` bug above: the crash only reproduced against a real
gateway startup with a no-auth entry present, not against any unit test.

## A single missing provider credential crashed the whole gateway

Real user report, reproduced exactly: `kram` refused to start at all —
`gateway didn't come up: ... connection refused` — with no indication of
why. `waitHealthy`'s timeout error was the only thing surfaced; the real
cause had already landed on `gateway.Run`'s internal `errCh` and was
silently discarded. Fixed first in `cmd/kram/main.go` (peek `errCh`
before falling back to the generic timeout message) and
`internal/gateway/gateway.go` (log the real cause too, so it survives in
the log file even if a caller ignores it) — which surfaced the actual
error: `building provider "anthropic": env var ANTHROPIC_API_KEY is not
set`.

That pointed at the deeper bug: a global `config.yaml`, written once by
an earlier wizard run, still listed Anthropic as a required provider —
its credential had since been deleted (via the accounts screen's "d"
key, confirmed working; see below). `provider.Build` correctly failed
for that one provider, but `gateway.Run`'s build loop treated *any*
single provider failure as fatal for the entire process — one stale
config line took down all six configured providers, the opposite of the
fallback-chain resilience this project is built around (see "The
default combo's fallback chain" above).

Fixed at two layers, since fixing only one just moves the same crash:
`gateway.Run` now logs a warning and skips a provider that fails to
build instead of aborting, only erroring if literally zero providers
end up built. `sanitizeCombosForBuiltProviders` (new, `gateway.go`) then
strips any now-missing provider ID out of every combo before handing
the config to `router.New` — which keeps its own stricter check
unchanged (a combo referencing a provider ID that was never *declared*
in `cfg.Providers` at all is still a real config typo worth a hard
error). A combo left with zero providers after filtering is dropped
entirely; if it was `default_combo`, the first surviving combo takes
over that role, logged either way. Deliberately *not* touched:
`config.Validate()`'s static check that every combo references a
provider ID that exists somewhere in the declared provider list — that
catches a different, still-real bug class (hand-edited config typos)
and stays strict.

Live-verified with the user's own real state (`/home/codexmark/.kram`,
Anthropic/Gemini/OpenCode-Zen credentials genuinely absent, three
OpenRouter providers genuinely present): the real log now reads three
`skipping provider that failed to build` warnings, three `provider
ready` lines, three `dropping unbuilt provider from combo` warnings, and
`kram-gateway listening` — followed by real `/health` 200s from both the
gateway and the daemon. Also covered by
`internal/gateway/gateway_test.go` (new): `sanitizeCombosForBuiltProviders`
directly (dropping a provider from a combo, dropping an empty combo and
reassigning `default_combo`), and `Run` end-to-end (one broken + one
working provider still serves; all-broken still fails fast).

### The "d" key not deleting a provider was two separate, narrower bugs, not the router

Investigated in parallel since the user reported it alongside the crash
above. `internal/cli/app/accounts.go`'s `"d"` handler blocked *all*
deletion unconditionally whenever `wizardMode` was true — a leftover
from before custom providers existed, when catalog credentials really
were the only thing on this screen and "nothing to undo during setup"
was a reasonable rule. Custom providers changed that: a mistyped
URL/name registered mid-wizard now has no way to be corrected before
finishing setup. Fixed by scoping the wizard-mode restriction to only
the catalog-row case, confirmed by six new focused tests in
`accounts_test.go` covering every row type and mode combination
(`TestDeleteKeyRemovesStoredCatalogCredential`,
`TestDeleteKeyOnCustomProviderWorksDuringWizardSetup`, etc.) — direct
`handleAccountsKey` calls, not live TUI, once tmux in this environment
became too unstable to drive reliably for this particular investigation.
Separately, direct inspection of the user's real
`~/.config/kram-gateway/credentials.json` showed Anthropic's key was, in
fact, already gone — proof the delete logic itself had already worked
correctly at least once; the crash above is what made it *look* like
deletion had failed.

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

## Continuous integration — *deferred*

`.github/workflows/` was built out in full (see the design below — kept
verbatim, not deleted, since it's exactly what gets restored if this
comes back) and then removed. Diagnosing the original observability
complaint (see below) surfaced that GitHub Actions could not execute at
all on this account — every run, including GitHub's own internal
Dependabot dependency-graph workflow with no user-authored YAML, failed
instantly with `startup_failure`, which points at an Actions
billing/spending-limit condition rather than anything fixable by editing
workflow files. Rather than ship CI infrastructure that cannot prove it
runs, the workflows were removed and releases/local validation went
manual instead:

- `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./... -race`
  run locally before every push (same commands the removed workflow
  would have run);
- releases are cut with `scripts/build-release.sh` plus `gh release
  create` directly — see the README's "Building releases".

This is a cost decision, not a technical one — GitHub Actions on a free
personal plan comes with a limited included-minutes quota, and this
account is past whatever threshold requires a paid spending limit. The
design below is ready to restore as-is the moment that's revisited.

### Check Runs and Commit Statuses are two different APIs, not two names for the same thing

**Found as a real observability gap.** `.github/workflows/ci.yml` had run since the workflow was first added, and GitHub's own Actions UI showed it green — but `GET /repos/{owner}/{repo}/commits/{sha}/status` (the classic Commit Status API) returned `statuses: []` for every commit, even ones CI had clearly checked.

This is not a bug in that endpoint. GitHub Actions jobs automatically create **Check Runs** (grouped into a Check Suite owned by the "github-actions" App) — that's what populates the PR "Checks" tab and the green check mark next to a commit in the GitHub UI. The classic **Commit Status API** (`POST/GET .../statuses/{sha}`) is a separate, older mechanism predating Actions, originally built for external CI services (Travis, CircleCI, Jenkins) that had no other way to report into GitHub. Actions never populates it on its own. A repo that only uses Actions will always show `statuses: []` to anything querying that specific endpoint — regardless of whether CI is passing, failing, or has never run — because nothing ever calls it.

Since external tooling and scripted audits commonly assume the classic API is authoritative (it's the older, simpler, more widely-integrated one), the fix publishes **both**: native Check Runs (automatic, zero extra code, the richer/native GitHub UX) and explicit classic Commit Statuses (`.github/actions/report-status`, a small composite action calling `POST .../statuses/{sha}` once per job) with matching `kram/*` contexts. Neither replaces the other; they're kept in sync by construction, not by best effort.

### A second, unrelated, worse problem was already there: `startup_failure` on every run

Diagnosing the above surfaced something more serious: every CI run since the workflow existed had `conclusion: "startup_failure"` — `jobs: []`, zero check runs created, nothing ever actually executed. Both `.github/workflows/ci.yml` and `.github/workflows/release.yml` parse as valid YAML (verified with `actionlint`, which reported zero issues against the rewritten workflow), which ruled out a workflow-file syntax problem as the cause. The conclusive signal: GitHub's own internal Dependabot dependency-graph-update workflow — which has no YAML file in this repo and nothing to do with `ci.yml`'s content — failed identically on the same pushes. A workflow the repo owner has zero authorship over cannot fail because of a mistake in `ci.yml`; whatever is blocking execution is scoped above the repository, most consistent with a GitHub Actions billing/spending-limit condition on the account (the most common cause of *every* workflow failing to schedule at once) rather than anything this task can fix by editing YAML. This is called out explicitly in the report to the user rather than declared "fixed" — the workflow is now correct, but has not yet been *proven* to execute, and won't be provable until whatever is blocking scheduling at the account level is resolved.

### Jobs, not steps — parallel, independently named, individually queryable

`build`, `vet`, `format`, and `test-race` are four separate jobs (their own runner, their own logs, their own Check Run/commit status), not four steps inside one `test` job. A single job collapses everything into one pass/fail signal — `go vet` failing looks identical to `gofmt` failing looks identical to a real test failure until someone opens the log and reads it. Separate jobs run in parallel (no reason `go vet` should wait for `go test -race` to finish) and let an external consumer ask "did the race detector pass?" without parsing log text.

An aggregate `ci` job (`needs: [build, vet, format, test-race]`, `if: always()`) is not a fifth job re-running everything — it's three lines of bash checking `needs.*.result` for `failure`/`cancelled`. `if: always()` is required so it still runs (and reports its own failure) when a dependency fails; without it, a broken `build` would leave `Kram / CI` stuck at `pending` forever instead of turning red.

The workflow is literally named `Kram` (not `ci`) because GitHub names each Check Run `<workflow name> / <job name>` — this is what produces the stable, human-readable `Kram / Build`, `Kram / Vet`, `Kram / Format`, `Kram / Test Race`, `Kram / CI` names, which is also exactly what a future `required status checks` branch-protection rule would reference. Classic commit-status contexts (`kram/build`, `kram/vet`, `kram/format`, `kram/test-race`, `kram/ci`) are named separately since that API has its own flat namespace with no workflow/job grouping concept.

### Status lifecycle has to actually terminate

Each job reports `pending` as its second step (right after checkout — checkout has to come first since the status-reporting step is a local composite action, `uses: ./.github/actions/report-status`, which only exists once the repo is checked out), then exactly one of `success`/`failure`/`error` at the end via three mutually exclusive step conditions: `if: success()`, `if: failure()`, `if: cancelled()`. The classic Commit Status API has no `cancelled` state, so a cancelled job reports `error` — the closest fit, and the important part is that *something* always fires. Without the `cancelled()` branch, a job cancelled mid-run (e.g. by `concurrency.cancel-in-progress` superseding it) would leave its status stuck at `pending` indefinitely, which is worse than not reporting anything at all.

### SHA correctness: `github.sha` is not always the SHA that matters

On a `pull_request` event, `github.sha` is GitHub's ephemeral, synthetic merge commit — it never exists as a real commit in the repo's permanent history and disappears once the PR closes. Reporting a commit status against it would make that status permanently unqueryable by the actual SHA anyone cares about. `env.SHA` is computed once at the workflow level as `github.event.pull_request.head.sha || github.sha` — the real PR branch tip on `pull_request`, falling through to the real pushed commit on `push` (where no `pull_request` context exists at all). Native Check Runs don't need this handling; GitHub's Actions/Checks integration already associates them with the correct head SHA for `pull_request` events on its own — this correction is specific to the classic Statuses API, which has no equivalent built-in awareness.

### `pull_request`, not `pull_request_target` — and least-privilege token permissions

`pull_request_target` runs workflow code with the base repository's token permissions and secrets even for a PR from a fork, which is a well-known privilege-escalation vector when combined with checking out and running PR-authored code — exactly the shape of this workflow (it runs `go build`/`go test` on the PR's own code). Plain `pull_request` was kept instead, at the cost of slightly more limited `GITHUB_TOKEN` permissions for fork PRs, which is the correct tradeoff here. Workflow-level `permissions: contents: read, statuses: write` is the actual minimum this workflow needs — nothing writes repository contents, comments on PRs, or triggers other workflows, so nothing broader was granted.

### Caching: `actions/setup-go`'s built-in cache, not a hand-rolled one

`cache: true` on `actions/setup-go@v5` (keyed from `go.sum`, covering both the module download cache and the build cache) is what the task asked to prefer over inventing manual `actions/cache` wiring — it already solves this correctly and is one line per job.

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

A later, larger round closed the gaps that mattered most for capability,
safety, and reliability rather than raw feature count:

- ~~Cross-platform shell / process-tree cleanup.~~ Closed: `internal/shell`
  centralizes command construction and kill for `bash`/`run_background`/
  custom tools — a real process group + SIGTERM/SIGKILL on Unix, a Job
  Object on Windows (`cmd.exe`, not an assumed `sh.exe`). See "Cross-
  platform shell" above.
- ~~Tool permission policy.~~ Closed: `internal/permission` gates every
  tool call (built-in, custom, MCP) with ALLOW/ASK/DENY rules, a
  compatibility default of Allow, and a distinct approval flow from
  `ask_question`. See "Permissions" above.
- ~~Unbounded tool output.~~ Closed: `internal/artifact`'s `SpillWriter`
  bounds real memory use for any command's output (not just the reported
  size), spilling past a threshold to a retrievable artifact instead of
  truncating and losing it; a per-turn aggregate budget catches several
  individually-fine results adding up. See "Artifacts" above.
- ~~Semantic code navigation.~~ Closed: `internal/lsp` gives
  `lsp_diagnostics`/`lsp_definition`/`lsp_references` on top of grep/glob,
  lazy per-language server startup, never required for Kram to run. See
  "LSP" above.
- ~~Cross-session history search.~~ Closed: `session_search`, deterministic
  FTS5 over real conversation history — distinct from `memory_write`'s
  curated notes. See "Session search" above.
- ~~Workspace rollback.~~ Closed: `internal/snapshot`, reversible
  workspace snapshots via an isolated git repository, never touching the
  user's own `.git`. See "Workspace snapshots" above.
- ~~MCP lifecycle hardening.~~ Closed: dropped transports reconnect with
  backoff, `tools/list_changed` is handled, and tool schemas are cached
  by config fingerprint. See "MCP" above.
- ~~Eval false positives.~~ Closed: the harness reports SKIP, not a false
  PASS, when a scenario's check never observed the model taking the
  action it depends on.
- ~~Ordered-fallback-only routing.~~ Closed by Combos v2: a
  `Strategy`-ranked pipeline (priority/round-robin/prefix-affinity plus
  smart/quality/fast/cheap/reliable as presets of one weighted engine,
  lkgp, p2c), run-scoped sticky routing, an LKGP boost modifier,
  capability/breaker hard constraints applied before any scoring, a
  `ResponseGate` distinguishing technical failure from quality rejection,
  a bounded streaming peek (`StreamGate`) so fallback survives past
  `stream: true`, a full-turn `RouteTrace` (fixing a real bug where only
  the last model call's fallback trail was ever visible), and a route
  bar / Ctrl+R / Ctrl+P explainability UI reading that data directly with
  nothing recomputed. See the expanded "Routing" and "Interface"
  sections above for the full rationale.

- ~~Automated CI.~~ *Reversed* — see "Continuous integration" above.
  Built out in full (five granular jobs plus classic commit-status
  publishing), then removed once diagnosis showed GitHub Actions
  couldn't execute at all on this account (billing/spending-limit, not a
  workflow-content problem). Validation is manual (`go build`/`vet`/
  `gofmt`/`test -race` locally before every push) until that's revisited.

Still open, in rough priority order: a **context policy engine** (deferred
until artifact spill, above, had proven stable, which it now has — this
was Wave 8 in the mission that drove the round *before* Combos v2's own
numbering, worth being explicit about since this file now has two
different "Wave 8"s from two different rounds), real per-attempt live
routing progress (structurally blocked on the gateway's fallback loop
running inside one HTTP round-trip — see "Route bar: per-model-call
granularity" above), scheduling, async delegation with a real task-status
subsystem, a more sophisticated extension host, and pluggable memory/
terminal backends. None of these are accidental gaps — each was
evaluated and set aside as narrower than what it would extend, the same
discipline this file exists to make visible rather than silently
re-litigated.

The first-run wizard (see its own section above) also has a deliberately
scoped Phase 2, called out explicitly at the time rather than discovered
later: ~~OAuth/browser-auth for providers beyond OpenRouter~~ *closed for
Anthropic and OpenAI* — see "Browser login for Anthropic and OpenAI"
above; the richer "projects root + picker" launcher for a bare `kram`
invocation (today's wizard only persists `projects_root` as seed data, no
picker UI reads it yet); more advanced/tunable permission presets; deeper
system diagnostics; and actual migration logic across future
`currentOnboardingVersion` bumps (today a version bump just re-triggers
the wizard from scratch, which is intentional, not a stopgap).
