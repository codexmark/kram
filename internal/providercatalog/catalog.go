// Package providercatalog is the single list of LLM providers Kram knows
// how to auto-configure — shared by cmd/kram's env-var autodetection and
// the CLI's "accounts" screen, so the two never drift into disagreeing
// about what a provider is called or which env var it reads.
package providercatalog

// Provider is one entry in the catalog: everything needed to build a
// gateway config.ProviderConfig from a raw API key, plus the metadata the
// accounts UI needs to display it and point the user at where to get one.
type Provider struct {
	ID             string
	Label          string // human-readable name shown in the accounts UI
	Kind           string // gateway provider kind: anthropic, gemini, openai-compat
	BaseURL        string
	EnvVar         string // env var name the gateway reads the key from
	DefaultModel   string
	SupportsImages bool
	SupportsTools  bool
	SignupURL      string // where to get a key/account
	SupportsOAuth  bool   // true for providers with a real browser-login flow (see internal/oauthflow)
	FreeTier       bool   // zero-cost tier: rate-limit-bound rather than cost-bound, which changes how the router should treat it
}

const openRouterBaseURL = "https://openrouter.ai/api/v1"

// Providers is the full catalog, in priority order — the same order
// cmd/kram's autodetect uses to build a deterministic combo, and the
// order the accounts screen lists them in.
//
// OpenRouter free-tier models: several entries sharing one key so they
// form a real fallback chain (a $0 combo) rather than a single pinned
// model. Free-model slugs rotate on OpenRouter's end (verified against GET
// https://openrouter.ai/api/v1/models on 2026-08-17 — the previous picks
// here had already been retired) — check
// https://openrouter.ai/models?max_price=0 and update if one of these has
// been retired too. Only models whose supported_parameters include
// "tools" were picked. Conservatively marked as not supporting images:
// capability varies per free model and Kram never assumes.
//
// OpenCode Zen: sst/opencode's own hosted free-tier relay
// (https://opencode.ai/docs/zen/), OpenAI-compatible wire format. This is
// a single legitimate account's own key pointed at a documented public
// endpoint — not the account-rotation/evasion approach that was
// explicitly turned down earlier in this project. SupportsTools is
// unverified: OpenCode's own docs don't state whether the free models
// accept tool-calling, so this is an experiment, not a confirmed-working
// fallback — if it can't take tools, expect it to answer in plain text
// only rather than using Kram's tools.
var Providers = []Provider{
	{ID: "anthropic", Label: "Anthropic (Claude)", Kind: "anthropic", EnvVar: "ANTHROPIC_API_KEY", DefaultModel: "claude-sonnet-4-5", SupportsImages: true, SupportsTools: true, SignupURL: "https://console.anthropic.com/settings/keys", SupportsOAuth: true},
	{ID: "openai", Label: "OpenAI", Kind: "openai-compat", BaseURL: "https://api.openai.com/v1", EnvVar: "OPENAI_API_KEY", DefaultModel: "gpt-5", SupportsImages: true, SupportsTools: true, SignupURL: "https://platform.openai.com/api-keys"},
	// openai-chatgpt is a distinct product from openai above: a ChatGPT
	// Plus/Pro/Team subscription's Codex access via browser login, not a
	// developer API key. It talks to a different backend
	// (chatgpt.com/backend-api/codex/responses, Responses wire format —
	// see internal/provider/openai_responses.go) and only serves
	// Codex-branded models, so it can never share openai's env var or
	// config entry — doing so would silently misrepresent what the
	// credential can actually do. EnvVar here is only a lookup key into
	// the OAuth token store (internal/credentials), never a real
	// environment variable.
	{ID: "openai-chatgpt", Label: "OpenAI (via login ChatGPT — beta)", Kind: "openai-responses", EnvVar: "OPENAI_CHATGPT_ACCESS_TOKEN", DefaultModel: "gpt-5.5", SupportsTools: true, SignupURL: "https://chatgpt.com", SupportsOAuth: true},
	// DefaultModel is Google's own "latest" alias, not a numbered release —
	// deliberately, after live-testing found both gemini-2.5-pro and
	// gemini-2.5-flash rejected with a 404 ("no longer available to new
	// users") despite still appearing, unflagged, in the real ListModels
	// response (see DECISIONS.md). Numbered Gemini releases retire on a
	// timeline the models list doesn't warn about; the "latest" aliases
	// exist specifically so a pinned reference doesn't rot the same way.
	{ID: "gemini", Label: "Google AI Studio (Gemini)", Kind: "gemini", EnvVar: "GEMINI_API_KEY", DefaultModel: "gemini-flash-latest", SupportsImages: true, SupportsTools: true, SignupURL: "https://aistudio.google.com/apikey"},
	// DeepSeek is OpenAI-compatible (confirmed against its own API docs,
	// api-docs.deepseek.com — no dedicated adapter needed, same as
	// OpenAI/OpenRouter/OpenCode Zen below). BaseURL deliberately has no
	// "/v1" suffix: DeepSeek's documented endpoint is
	// https://api.deepseek.com/chat/completions, not .../v1/chat/completions
	// — confirmed live against their docs, not guessed from the OpenAI
	// convention other entries here happen to follow. DeepSeek's
	// reasoning/"thinking mode" models stream chain-of-thought via a
	// "reasoning_content" delta field — internal/provider/openai_compat.go
	// already captures that field name (added earlier for DeepSeek-R1-
	// compatible local servers), so no provider-side change was needed to
	// support it correctly instead of it being misread as a stalled
	// attempt. Not marked SupportsImages: deepseek-v4-flash-vision-exp is
	// the only vision-capable model in the lineup and is explicitly
	// experimental — the catalog pins the non-experimental flash model as
	// DefaultModel instead, so this stays false until a stable vision
	// model ships. See issue #20.
	{ID: "deepseek", Label: "DeepSeek", Kind: "openai-compat", BaseURL: "https://api.deepseek.com", EnvVar: "DEEPSEEK_API_KEY", DefaultModel: "deepseek-v4-flash", SupportsTools: true, SignupURL: "https://platform.deepseek.com/api_keys"},
	{ID: "openrouter-gptoss", Label: "OpenRouter (free: gpt-oss-20b)", Kind: "openai-compat", BaseURL: openRouterBaseURL, EnvVar: "OPENROUTER_API_KEY", DefaultModel: "openai/gpt-oss-20b:free", SupportsTools: true, FreeTier: true, SignupURL: "https://openrouter.ai/keys", SupportsOAuth: true},
	{ID: "openrouter-gemma", Label: "OpenRouter (free: gemma-4-31b)", Kind: "openai-compat", BaseURL: openRouterBaseURL, EnvVar: "OPENROUTER_API_KEY", DefaultModel: "google/gemma-4-31b-it:free", SupportsTools: true, FreeTier: true, SignupURL: "https://openrouter.ai/keys", SupportsOAuth: true},
	{ID: "openrouter-nemotron", Label: "OpenRouter (free: nemotron-3-super)", Kind: "openai-compat", BaseURL: openRouterBaseURL, EnvVar: "OPENROUTER_API_KEY", DefaultModel: "nvidia/nemotron-3-super-120b-a12b:free", SupportsTools: true, FreeTier: true, SignupURL: "https://openrouter.ai/keys", SupportsOAuth: true},
	{ID: "opencode-zen", Label: "OpenCode Zen (free: big-pickle)", Kind: "openai-compat", BaseURL: "https://opencode.ai/zen/v1", EnvVar: "OPENCODE_ZEN_API_KEY", DefaultModel: "big-pickle", SupportsTools: true, FreeTier: true, SignupURL: "https://opencode.ai/auth"},
}

// EnvVars returns every distinct env var the catalog reads from, in
// catalog order — used for the "export one of %v" error message when no
// provider is configured at all.
func EnvVars() []string {
	seen := make(map[string]bool)
	var out []string
	for _, p := range Providers {
		if !seen[p.EnvVar] {
			seen[p.EnvVar] = true
			out = append(out, p.EnvVar)
		}
	}
	return out
}

// Account is one credential the accounts screen manages. Several Provider
// entries can share a single Account — OpenRouter's three free-model
// combo entries above all read OPENROUTER_API_KEY — so this is the
// deduplicated, one-row-per-credential view the UI actually shows,
// distinct from Providers' one-row-per-combo-entry view.
type Account struct {
	ID            string
	Label         string
	EnvVar        string
	SignupURL     string
	SupportsOAuth bool
	// OAuthOnly is true for an account with no paste-a-key path at all —
	// its only credential shape is a browser-login OAuth token (so far,
	// just openai-chatgpt). The accounts screen hides "enter cola api
	// key" for these rows.
	OAuthOnly bool
}

// Accounts is the deduplicated credential list for the CLI's accounts
// screen, in the same priority order as Providers.
var Accounts = []Account{
	{ID: "anthropic", Label: "Anthropic (Claude)", EnvVar: "ANTHROPIC_API_KEY", SignupURL: "https://console.anthropic.com/settings/keys", SupportsOAuth: true},
	{ID: "openai", Label: "OpenAI", EnvVar: "OPENAI_API_KEY", SignupURL: "https://platform.openai.com/api-keys"},
	{ID: "openai-chatgpt", Label: "OpenAI (via login ChatGPT — beta)", EnvVar: "OPENAI_CHATGPT_ACCESS_TOKEN", SignupURL: "https://chatgpt.com", SupportsOAuth: true, OAuthOnly: true},
	{ID: "gemini", Label: "Google AI Studio (Gemini)", EnvVar: "GEMINI_API_KEY", SignupURL: "https://aistudio.google.com/apikey"},
	{ID: "deepseek", Label: "DeepSeek", EnvVar: "DEEPSEEK_API_KEY", SignupURL: "https://platform.deepseek.com/api_keys"},
	{ID: "openrouter", Label: "OpenRouter", EnvVar: "OPENROUTER_API_KEY", SignupURL: "https://openrouter.ai/keys", SupportsOAuth: true},
	{ID: "opencode-zen", Label: "OpenCode Zen", EnvVar: "OPENCODE_ZEN_API_KEY", SignupURL: "https://opencode.ai/auth"},
}
