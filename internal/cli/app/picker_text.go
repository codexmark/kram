package app

// User-facing strings for the session picker (picker.go), centralized here
// for the pt-BR -> English migration (issue #74). Footer hints are
// intentionally all-lowercase; glyphs (↑↓ ·), the em dash, leading list
// indentation, and the %d format verb are preserved exactly.

const (
	// Picker screen titles.
	pickerTitle         = "sessions"
	pickerSubagentTitle = "subagent sessions"
)

const (
	// New-session title prompt: label above the input and its hint line.
	pickerNewSessionLabel = "new session — title:"
	pickerNewSessionHint  = "enter confirms · esc cancels"
)

const (
	// Status / list rows. pickerErrPrefix is concatenated with an
	// err.Error() suffix; pickerLoading keeps its leading space (it follows
	// the spinner glyph); the empty-state lines keep their two-space list
	// indentation.
	pickerErrPrefix      = "error: "
	pickerLoading        = " loading…"
	pickerNewSessionRow  = "+ new session"
	pickerEmptySessions  = "  (no existing sessions yet)"
	pickerEmptySubagents = "  (no subagent sessions yet)"
)

const (
	// Footer hints. pickerSubagentCountFmt is a Sprintf prefix prepended to
	// the main hint; it keeps the %d verb and trailing ' · '.
	pickerFooterMain       = "↑↓ choose · enter confirm · a accounts · f tools · ctrl+c quit"
	pickerFooterSubagent   = "↑↓ choose · enter confirm · s normal sessions · ctrl+c quit"
	pickerSubagentCountFmt = "s %d subagent sessions · "
)

const (
	// Relative-age labels (formatAge). The Fmt consts keep the %d verb.
	ageNow        = "now"
	ageMinutesFmt = "%dmin ago"
	ageHoursFmt   = "%dh ago"
	ageDaysFmt    = "%dd ago"
)
