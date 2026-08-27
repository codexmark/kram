package app

// User-facing strings for the accounts screen (accounts.go), centralized here
// for the pt-BR -> English migration (issue #74). Wizard and footer hints are
// intentionally all-lowercase; badges and status lines keep their glyphs
// (✓ ● ○ ▸ · — …), key names, and format verbs exactly.

const (
	// Wizard-mode Providers step header hints, and the non-wizard title.
	accountsWizardIntro   = "configure at least one provider to use the gateway and combos."
	accountsWizardFastest = "fastest: pick OpenRouter and press \"o\" — authorize in the browser, no card, free."
	accountsTitle         = "accounts"
)

const (
	// Per-provider status badges and row affordances.
	accountsRowNotConfigured     = "— not configured"
	accountsBadgeEnv             = "✓ set (environment)"
	accountsBadgeSaved           = "✓ set (saved)"
	accountsBadgeRegisteredNoKey = "✓ registered (no key)"
	accountsOAuthAffordance      = "(o: authorize in browser)"
	accountsModelPrefix          = "model: "

	// The "+ add custom provider" row.
	accountsAddCustomRow = "+ add custom provider (URL + optional key — local/network server)"
)

const (
	// Masked key-entry prompt + footer.
	accountsPasteKeyPrompt = "paste the API key:"
	accountsKeyEntryHint   = "enter saves · esc cancels"

	// OAuth-pending prompt + footer.
	accountsOAuthPendingPrompt = "authorize in the browser that opened — if it didn't open, paste this link:"
	accountsOAuthPendingHint   = "waiting for authorization… · esc cancels"
)

const (
	// Footer key-bar actions (assembled in renderAccounts as wizardKey
	// pairs and rendered via renderWizardKeybar — accent key, muted label).
	accountsActionPasteKey    = "paste API key"
	accountsActionOAuth       = "browser login"
	accountsActionRemoveSaved = "remove saved key"
	accountsActionSetUpdate   = "set/update key"
	accountsActionRemove      = "remove"
	accountsActionAddCustom   = "add custom provider"
	accountsActionRecheck     = "re-check"
	accountsActionEscCancel   = "cancel setup"
	accountsActionEscBack     = "back"

	// The wizard's explicit continue line, rendered above the key bar so
	// the way forward is a visible call to action rather than a fragment
	// buried mid-sentence ("· n continues").
	accountsContinueKey         = "n"
	accountsContinueLabel       = "continue to Routing →"
	accountsContinueAnywayKey   = "c"
	accountsContinueAnywayLabel = "continue anyway — provider not responding"
	accountsContinueLockedNote  = "connect a provider to unlock continue"
)

const (
	// Gateway-mode summary line (wizardGatewayModeLine).
	accountsGatewayModeNone             = "Gateway mode: —  (no provider configured yet)"
	accountsGatewayModeBasicTail        = "  · 1 upstream configured — multi-provider fallback is limited"
	accountsGatewayModeResilientTailFmt = "  · %d independent upstreams"
)

const (
	// accountsStatus lines (rendered via styleHint). Several are suffixes
	// concatenated after a provider Label/Name prefix.
	accountsStatusKeySaved          = ": key saved — restart kram to use it."
	accountsStatusSaveErrorPrefix   = "error saving: "
	accountsStatusCredentialRemoved = ": credential removed."
	accountsStatusProviderRemoved   = ": provider removed."
	accountsStatusNeedProvider      = "configure at least one provider before continuing."
	accountsStatusWaitCheck         = "wait for the check to finish or press r to try again."
	accountsStatusNoOperational     = "no operational provider — press r to try again or c to continue anyway."
	accountsStatusForceContinue     = "continuing by explicit choice, despite the validation failure."
	accountsStatusProviderAdded     = ": provider added — restart kram to use it."
	accountsStatusConnected         = ": connected — restart kram to use it."
)

const (
	// "+ add custom provider" form: field labels, header, hints, placeholder,
	// the pre-filled tool-calling default, and the fetched-models picker.
	customFormLabelName          = "name"
	customFormLabelURL           = "url"
	customFormLabelAPIKey        = "api key (optional)"
	customFormLabelModel         = "model"
	customFormLabelTools         = "accepts tool calling? (y/n)"
	customFormLabelContextWindow = "context window (optional)"

	customFormContextWindowPlaceholder = "e.g. 32768 — leave blank if unsure"

	customFormHeader          = "new custom provider:"
	customFormHintBase        = "tab next · shift+tab back · enter saves · esc cancels"
	customFormHintFetchPrefix = "ctrl+l fetch models · "
	customFormFetchingMsg     = "fetching models…"
	customFormPickerHint      = "↑↓ choose · enter use · esc cancel (back to typing manually)"
	customFormModelsFoundFmt  = "models found (%d):"

	customFormNamePlaceholder = "My Server"

	// Pre-filled value of the "accepts tool calling?" field (y = yes).
	customFormToolsDefault = "y"

	// Custom-provider form status lines.
	customFormStatusNeedURL      = "enter the url before fetching models."
	customFormStatusNoStore      = "error: local storage unavailable."
	customFormStatusKeySaveError = "provider saved, but error saving the key: "
)
