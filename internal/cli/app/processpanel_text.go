package app

// User-facing strings for the background-process observer (processpanel.go),
// centralized here for the pt-BR -> English migration (issue #74). Glyphs
// (● ✓ ✗ ↑↓ ·), key names, embedded newlines, the run_background tool
// literal, and the %d format verbs are preserved exactly.

const (
	// Process viewport body messages. processRefreshErrPrefix keeps its
	// trailing newline before the concatenated error text;
	// processHistoryTruncated keeps its trailing blank line.
	processRefreshErrPrefix = "couldn't refresh the process:\n"
	processNoOutput         = "(no output produced yet)"
	processHistoryTruncated = "[earlier history unavailable: the buffer/window kept only the tail]\n\n"
)

const (
	// Panel header and list-status lines. processDaemonErrPrefix is
	// concatenated with an err.Error() suffix.
	processPanelTitle      = "PROCESSES"
	processPanelSubtitle   = "local observation · zero tokens"
	processEmptyList       = "no process started by run_background"
	processLoadingStatus   = "loading processes…"
	processDaemonErrPrefix = "daemon unavailable: "
)

const (
	// Selected-process state badges. processStateExitFmt keeps the %d verb.
	processStateRunning  = "● running"
	processStateExitZero = "✓ exited 0"
	processStateExitFmt  = "✗ exited %d"
)

const (
	// Detail prompt and footer hints. processNewBytesFmt keeps the ↓ glyph
	// and %d verb.
	processSelectPrompt = "select a process"
	processFooterHint   = "tab switch · ↑↓ scroll · end follow · esc close"
	processNewBytesFmt  = "↓ %d new bytes · end to follow"
)
