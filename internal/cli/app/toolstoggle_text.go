package app

// Tools/skills toggle screen strings: title, status lines, and footer hint.
// The footer hint is intentionally all-lowercase, matching the other wizard
// hints.
const (
	toolsToggleTitle = "tools and skills"
	toolsErrPrefix   = "error: "
	toolsLoading     = " loading…"
	toolsEmpty       = "(nothing registered)"
	toolsFooterHint  = "↑↓ choose · space/enter toggle · a enable all · d disable all · esc back"

	toolsApplyingDaemon = "applying to current daemon…"
	toolsSaveErrPrefix  = "error saving: "
	toolsItemDisabled   = ": disabled."
	toolsItemEnabled    = ": enabled."
	toolsBulkDisabled   = "%d disabled."
	toolsBulkEnabled    = "%d enabled."
)
