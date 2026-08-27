package app

// User-facing strings for the first-run setup wizard (wizard.go,
// wizard_chrome.go). Centralized here for the pt-BR -> English migration
// (issue #74). Key-bar entries live at the call sites as wizardKey
// pairs; hint copy uses readable colors (see wizard_chrome.go) rather
// than the near-invisible faint style it once had.
const (
	// Card titles, one per step (the trail shows the same names).
	wizardTitleWelcome     = "Welcome"
	wizardTitleProjects    = "Projects"
	wizardTitleProviders   = "Providers"
	wizardTitleRouting     = "Routing"
	wizardTitlePermissions = "Permissions"
	wizardTitleTools       = "Tools & Skills"
	wizardTitleCheck       = "System Check"
	wizardTitleReady       = "Ready"

	// Step 1: Welcome
	wizardEnvSystemLabel     = "System"
	wizardEnvCurrentDirLabel = "Directory"
	wizardEnvGitNotFound     = "not found"
	wizardEnvGitFound        = "found"
	wizardEnvGitOptionalNote = "optional — recommended for snapshots and diffs"
	wizardEnvWelcome         = "Your local coding agent. Let's set up the essentials — it takes less than a minute."

	// Step 2: Projects
	wizardProjectsRootHint      = "where you usually keep your projects"
	wizardProjectsWorkspaceHint = "the project kram will work on in this session"
	wizardErrCreateWorkspaceFmt = "couldn't create %s: %w"

	// Step 4: Routing
	wizardRoutingAutoLabel         = "Auto (recommended)"
	wizardRoutingAutoDesc          = "kram picks based on the configured providers"
	wizardRoutingSmartDesc         = "health + reliability + latency + cache affinity"
	wizardRoutingRoundRobinDesc    = "spreads calls across eligible providers"
	wizardRoutingAutoPreviewPrefix = "auto currently resolves to: "
	wizardRoutingHint              = "advanced strategies, weights and gates remain adjustable in the generated config.yaml."

	// Step 5: Permissions
	wizardPermRecommendedDesc = "asks before rm -rf, git push, deleting/moving a file — everything else allowed"
	wizardPermStrictDesc      = "asks before almost everything, including MCP tools — only reads and git status allowed"
	wizardPermAutonomousDesc  = "few confirmations — only blocks rm -rf with an absolute path"

	// Step 6: Tools & Skills preset
	wizardToolsRecommendedDesc = "standard development set — nothing disabled"
	wizardToolsMinimalDesc     = "read, search, navigation and code intelligence — everything else disabled"
	wizardToolsCustomDesc      = "choose individually on the per-tool screen"
	wizardToolsLoading         = " loading…"
	wizardToolsRegisteredFmt   = "%d tools/skills registered."
	wizardToolsErrSavePreset   = "error saving preset: "
	wizardToolsApplying        = "applying to the current daemon…"
	wizardErrToolsStorage      = "tools storage unavailable"

	// Step 6b: starter skill pack offer (#135)
	wizardSkillsIntro       = "Skills are installable playbooks (debugging, reviews, TDD...) the agent checks before specialized work."
	wizardSkillsInstallDesc = "14 curated playbooks from github.com/codexmark/kram-skills"
	wizardSkillsSkipDesc    = "start bare — install anytime by asking kram to use skill_install"
	wizardSkillsInstalling  = "cloning and installing…"
	wizardSkillsErrPrefix   = "install failed (skip is safe): "

	// Step 7: System Check
	wizardCheckOptional     = "optional"
	wizardCheckProvidersFmt = "%d configured"
	wizardCheckMCPFmt       = "%d server(s) configured"
	wizardCheckHint         = "optional items don't block — informational only."

	// Step 8: Summary / Ready
	wizardSummaryHeadline   = "✓ Kram is ready"
	wizardSummaryNote       = "you can reconfigure anytime with kram -setup"
	wizardWelcomeBody       = "Kram is ready. I can start by mapping this project, reviewing a task, or working directly on a change."
	wizardWelcomeSuggestion = `suggestion: "Map this repository and explain its architecture"`

	// Shared error prefixes (concatenated with a Go error's message)
	wizardErrPrefix           = "error: "
	wizardErrCompletionPrefix = "couldn't finish: "
)
