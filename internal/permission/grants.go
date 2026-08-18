package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Grant is one persisted "always" approval — the exact subject the user
// approved, not a broadened wildcard. If a user approves `bash` running
// `git push origin feature/foo`, the grant remembers exactly that command,
// never "bash: allow *" — a policy engine that silently widens what it
// remembers approving is more dangerous than one that asks too often.
type Grant struct {
	Tool      string `json:"tool"`
	Pattern   string `json:"pattern"` // literal subject text, matched exactly (see Rule.matchesSubject)
	CreatedAt int64  `json:"created_at"`
}

// GrantStore persists grants per project (<workspace>/.kram/permission_grants.json)
// — deliberately not global: an approval made in one project has no
// business silently applying to another.
type GrantStore struct {
	path   string
	grants []Grant
}

// LoadGrants reads the project's grant file, or returns an empty store if
// it doesn't exist yet (the normal case — most projects have never
// approved anything "always").
func LoadGrants(workspace string) (*GrantStore, error) {
	gs := &GrantStore{path: filepath.Join(workspace, ".kram", "permission_grants.json")}
	data, err := os.ReadFile(gs.path)
	if os.IsNotExist(err) {
		return gs, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &gs.grants); err != nil {
		return nil, err
	}
	return gs, nil
}

// Rules exposes every persisted grant as an Allow rule, ready to feed into
// NewEvaluator.
func (gs *GrantStore) Rules() []Rule {
	if gs == nil {
		return nil
	}
	out := make([]Rule, 0, len(gs.grants))
	for _, g := range gs.grants {
		out = append(out, Rule{Tool: g.Tool, Pattern: g.Pattern, Decision: Allow})
	}
	return out
}

// Add records a new "always" grant and persists it immediately. Takes
// effect starting with the *next* agent run in this workspace (the
// Evaluator that gated the call being approved right now was already built
// for this run — see Evaluator's doc comment for why that's deliberate).
func (gs *GrantStore) Add(tool, subject string) error {
	if gs == nil {
		return nil
	}
	gs.grants = append(gs.grants, Grant{Tool: tool, Pattern: subject, CreatedAt: time.Now().Unix()})
	if err := os.MkdirAll(filepath.Dir(gs.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(gs.grants, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(gs.path, data, 0o600)
}
