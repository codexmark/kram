package app

// User-facing strings for the first-run setup wizard (wizard.go).
// Centralized here for the pt-BR -> English migration (issue #74).
// Footer/hint lines are intentionally all-lowercase.
const (
	// Step 1: Environment
	wizardEnvSystemLabel     = "System"
	wizardEnvCurrentDirLabel = "Current directory"
	wizardEnvGitNotFound     = "not found"
	wizardEnvGitFound        = "found"
	wizardEnvWelcome         = "welcome to kram. let's set up the essentials — it takes less than a minute."
	wizardEnvFooter          = "enter continues · esc/ctrl+c cancels"

	// Step 2: Projects
	wizardProjectsRootHint      = "  (where you usually keep your projects)"
	wizardProjectsWorkspaceHint = "  (the project for this session)"
	wizardProjectsFooter        = "tab switches field · enter continues · esc back"
	wizardErrCreateWorkspaceFmt = "couldn't create %s: %w"

	// Step 4: Routing
	wizardRoutingAutoLabel      = "Auto (recommended)"
	wizardRoutingAutoDesc       = "kram picks based on the configured providers"
	wizardRoutingSmartDesc      = "health + reliability + latency + cache affinity"
	wizardRoutingRoundRobinDesc = "spreads calls across eligible providers"
	wizardRoutingHint           = "advanced strategies, weights and gates remain adjustable in the generated config.yaml."

	// Step 5: Permissions
	wizardPermRecommendedDesc = "asks before rm -rf, git push, deleting/moving a file — everything else allowed"
	wizardPermStrictDesc      = "asks before almost everything, including MCP tools — only reads and git status allowed"
	wizardPermAutonomousDesc  = "few confirmations — only blocks rm -rf with an absolute path"

	// Shared footer hint (Routing + Permissions steps)
	wizardFooterChooseContinueBack = "↑↓ choose · enter continues · esc back"

	// Step 6: Tools & Skills preset
	wizardToolsRecommendedDesc = "standard development set — nothing disabled"
	wizardToolsMinimalDesc     = "read, search, navigation and code intelligence — everything else disabled"
	wizardToolsCustomDesc      = "choose individually"
	wizardToolsLoading         = " loading…"
	wizardToolsRegisteredFmt   = "%d tools/skills registered."
	wizardToolsFooter          = "↑↓ choose · enter applies · Custom opens the per-tool screen"
	wizardToolsErrSavePreset   = "error saving preset: "
	wizardToolsApplying        = "applying to the current daemon…"
	wizardErrToolsStorage      = "tools storage unavailable"

	// Step 7: System Check
	wizardCheckOptional     = "optional"
	wizardCheckProvidersFmt = "%d configured"
	wizardCheckMCPFmt       = "%d server(s) configured"
	wizardCheckHint         = "optional items don't block — informational only."
	wizardCheckFooter       = "enter continues"

	// Step 8: Summary / Ready
	wizardSummaryFooter     = "enter opens a session and starts using kram · kram --setup reconfigures anytime"
	wizardWelcomeBody       = "Kram is ready. I can start by mapping this project, reviewing a task, or working directly on a change."
	wizardWelcomeSuggestion = `suggestion: "Map this repository and explain its architecture"`

	// Shared error prefixes (concatenated with a Go error's message)
	wizardErrPrefix           = "error: "
	wizardErrCompletionPrefix = "couldn't finish: "
)
