package app

// User-facing strings owned by model.go (the core Bubble Tea program:
// input placeholders, footer notices, and the status lines set from the
// Update message handlers), centralized here for the pt-BR -> English
// migration (issue #74). Format verbs (%d), glyphs (✓ ·), and the trailing
// spaces/colons that callers concatenate onto are preserved exactly.

const (
	// Text-input placeholders built in New().
	composerPlaceholder        = "message…"
	newSessionTitlePlaceholder = "title (optional, enter to skip)"
	answerPlaceholder          = "answer…"
)

// Strategy-switched notice (footer); concatenated with the uppercased
// strategy name.
const strategyNoticePrefix = "✓ strategy: "

const (
	// accountsStatus lines set while fetching a custom provider's models or
	// running the OAuth flow. The *Prefix consts are concatenated with an
	// err.Error() suffix.
	customModelsFetchErrPrefix = "error fetching models: "
	customModelsNoneFound      = "no models found on that server."
	oauthStartErrPrefix        = "error starting oauth: "
	oauthFailedPrefix          = "oauth failed: "
	oauthSaveErrPrefix         = "error saving: "
)

const (
	// toolsStatus lines set from the tools/skills toggle apply result.
	toolsApplyDaemonErrPrefix = "error applying to daemon: "
	toolsAppliedDaemon        = "configuration applied to the current daemon."
)

// Notice appended to the last message when the user hits Esc to interrupt a
// turn.
const interruptedByUser = "interrupted by user"

const (
	// Live drag-selection / copy-confirmation footer notices. Format strings
	// keep the %d verb; copyConfirmationFmt keeps the ✓ glyph and ' · '
	// separator.
	selectionInProgressFmt = "selecting %d characters…"
	copyConfirmationFmt    = "✓ copied · %d characters"
)
