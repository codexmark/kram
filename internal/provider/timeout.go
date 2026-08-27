package provider

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// DefaultTimeout bounds each *phase* of a provider call, not the whole
// exchange: first the connect/headers wait, then every quiet stretch of
// the response stream (the watchdog resets on each byte read — see
// newStreamWatchdog). Its old meaning — an http.Client.Timeout covering
// the entire request including reading the streamed body — killed any
// legitimately long generation mid-answer with "stream read: context
// deadline exceeded ... while reading body", which is exactly what a
// slow-but-healthy reasoning model produces on a big response.
// config.Tunables.ProviderTimeout raises or lowers it.
const DefaultTimeout = 120 * time.Second

// timeoutSetter is implemented by every adapter whose per-phase timeout
// can be overridden after construction. Build uses it to apply the
// configured provider timeout without threading the value through all four
// constructors (and their ~40 test call sites) — the constructors keep
// their DefaultTimeout, and Build overrides it only when a non-zero value
// is configured.
type timeoutSetter interface {
	setTimeout(time.Duration)
}

func (p *OpenAICompatible) setTimeout(d time.Duration) { p.timeout = d }
func (p *Anthropic) setTimeout(d time.Duration)        { p.timeout = d }
func (p *Gemini) setTimeout(d time.Duration)           { p.timeout = d }
func (p *OpenAIResponses) setTimeout(d time.Duration)  { p.timeout = d }

// streamWatchdog turns one timeout value into per-phase liveness
// enforcement for a streaming provider call. Armed at creation, it
// cancels the derived context if the current phase — connect+headers at
// first, then each stretch between successful body reads once Body() has
// wrapped the response — goes idle longer than the threshold. A dead
// upstream therefore still fails at the same threshold the old
// whole-call timeout used, while a healthy stream that keeps delivering
// bytes (tokens, SSE events, keep-alives) can run indefinitely.
type streamWatchdog struct {
	cancel context.CancelFunc
	timer  *time.Timer
	idle   time.Duration
	id     string
	fired  atomic.Bool
}

// newStreamWatchdog derives the context a streaming provider call should
// run under (request, credential resolution and body reads included) and
// arms the watchdog for the connect/headers phase.
func newStreamWatchdog(ctx context.Context, idle time.Duration, providerID string) (context.Context, *streamWatchdog) {
	if idle <= 0 {
		idle = DefaultTimeout
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &streamWatchdog{cancel: cancel, idle: idle, id: providerID}
	w.timer = time.AfterFunc(idle, func() {
		w.fired.Store(true)
		cancel()
	})
	return wctx, w
}

// Body wraps the response body so every successful read pushes the
// watchdog forward — converting the connect-phase timer into an idle
// detector for the stream phase. Closing the wrapped body disarms the
// watchdog and releases the derived context.
func (w *streamWatchdog) Body(rc io.ReadCloser) io.ReadCloser {
	return &watchdogBody{rc: rc, w: w}
}

// Stop disarms the watchdog and releases the derived context — for
// early-error paths that never reach Body(); the wrapped body's Close
// does the same on the normal path.
func (w *streamWatchdog) Stop() {
	w.timer.Stop()
	w.cancel()
}

// wrapErr labels a read failure the watchdog itself caused, so the
// surfaced error says "upstream went quiet" instead of the generic
// context-canceled text that misreads as a client-side bug.
func (w *streamWatchdog) wrapErr(err error) error {
	if err != nil && w.fired.Load() {
		return fmt.Errorf("no data from upstream for %s (idle timeout)", w.idle)
	}
	return err
}

type watchdogBody struct {
	rc io.ReadCloser
	w  *streamWatchdog
}

func (b *watchdogBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.w.timer.Reset(b.w.idle)
	}
	return n, b.w.wrapErr(err)
}

func (b *watchdogBody) Close() error {
	b.w.Stop()
	return b.rc.Close()
}
