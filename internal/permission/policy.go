// Package permission evaluates whether a tool call is allowed to run
// without asking, must ask the user first, or is blocked outright — a
// deterministic policy layer between the model deciding to call a tool and
// the tool actually running. This is distinct from toolsettings
// (enabled/disabled): disabling a tool removes it entirely, while a
// permission policy can allow a tool in general but ask/deny specific uses
// of it (e.g. "bash: allow `go test *`, ask before `git push*`").
package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/codexmark/kram/internal/kramhome"
)

// Decision is the outcome of evaluating a rule against a tool call.
type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

func (d Decision) valid() bool {
	return d == Allow || d == Ask || d == Deny
}

// Rule matches a tool call and says what to do with it. Tool is either an
// exact tool name ("bash", "delete_file") or a "*"-suffixed prefix glob
// ("mcp__github__*", "mcp__*") — the only wildcard form supported, which is
// enough to cover MCP's real `mcp__<server>__<tool>` namespacing without
// inventing categories the code doesn't actually have. Pattern is matched
// against the tool's "subject" (see policySubject in
// internal/daemon/tools/tools.go — bash's command, a file tool's path,
// everything else's raw argument JSON as a fallback): "" or "*" matches any
// subject, a "*"-suffixed string is a prefix glob, anything else must match
// exactly (this last form is what a persisted "always" grant uses — see
// grants.go — so approving one exact command never silently broadens into
// approving a whole prefix the user never saw).
type Rule struct {
	Tool     string   `json:"tool"`
	Pattern  string   `json:"pattern,omitempty"`
	Decision Decision `json:"decision"`
}

func (r Rule) matchesTool(tool string) bool {
	if !strings.Contains(r.Tool, "*") {
		return r.Tool == tool
	}
	return strings.HasPrefix(tool, strings.TrimSuffix(r.Tool, "*"))
}

func (r Rule) matchesSubject(subject string) bool {
	if r.Pattern == "" || r.Pattern == "*" {
		return true
	}
	if strings.HasSuffix(r.Pattern, "*") {
		return strings.HasPrefix(subject, strings.TrimSuffix(r.Pattern, "*"))
	}
	return r.Pattern == subject
}

// specificity ranks a matching rule so the evaluator can pick the most
// specific one rather than depend on declaration order — "first match
// wins" would make behavior silently depend on file layout. Exact tool +
// exact pattern beats exact tool + wildcard pattern beats a glob tool name.
func (r Rule) specificity() int {
	exactTool := !strings.Contains(r.Tool, "*")
	exactPattern := r.Pattern != "" && r.Pattern != "*"
	switch {
	case exactTool && exactPattern:
		return 3
	case exactTool:
		return 2
	default:
		return 1
	}
}

// PolicyFile is the on-disk shape of a permissions.json — global
// (kramhome/permissions.json) or project (<workspace>/.kram/permissions.json).
type PolicyFile struct {
	// Default is the decision when no rule matches at all. Empty means
	// Allow — a fresh install with no policy file behaves exactly like
	// Kram did before this feature existed. This compatibility default is
	// deliberate: shipping this change must never make existing
	// installations start asking about everything.
	Default Decision `json:"default,omitempty"`
	Rules   []Rule   `json:"rules,omitempty"`
}

// LoadConfig merges the global policy (kramhome/permissions.json) with the
// project's own (<workspace>/.kram/permissions.json), project rules
// appended after global ones. Best-effort like mcp.LoadConfig and
// toolsettings.Load: a missing file is the normal case (most projects have
// no custom policy) and a malformed one must not stop the daemon from
// starting — it just contributes nothing.
func LoadConfig(workspace string) PolicyFile {
	var pf PolicyFile
	if global, err := kramhome.Path("permissions.json"); err == nil {
		mergeConfigFile(&pf, global)
	}
	mergeConfigFile(&pf, filepath.Join(workspace, ".kram", "permissions.json"))
	return pf
}

func mergeConfigFile(into *PolicyFile, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f PolicyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	if f.Default.valid() {
		into.Default = f.Default
	}
	for _, r := range f.Rules {
		if r.Decision.valid() && r.Tool != "" {
			into.Rules = append(into.Rules, r)
		}
	}
}
