package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codexmark/kram-gateway/internal/kramhome"
)

// Skill is one discovered skill: a folder containing a SKILL.md with a
// small frontmatter block (name, description) followed by the actual
// instructions as markdown. This is the same shape opencode's Agent
// Skills, Hermes Agent's agentskills.io-based skills, and OpenClaude's
// DiscoverSkillsTool all converge on — a lightweight open convention, not
// something Kram invented.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`     // SKILL.md path, for error messages / the toggle UI
	Scope       string `json:"scope"`    // "project" or "global"
	Disabled    bool   `json:"disabled"` // set by Registry.Skills(), not discoverSkills itself
	Body        string `json:"-"`        // everything after the frontmatter
}

// discoverSkills scans both skill directories — project-local
// (workspace/.kram/skills/<name>/SKILL.md) and global
// (kramhome/skills/<name>/SKILL.md) — and returns every skill found,
// project skills first. A missing directory is normal (no skills authored
// yet) and not an error; a malformed SKILL.md is skipped rather than
// failing the whole scan, since one bad file shouldn't hide every other
// skill from the model.
func discoverSkills(workspace string) []Skill {
	var out []Skill
	out = append(out, scanSkillDir(filepath.Join(workspace, ".kram", "skills"), "project")...)
	if globalDir, err := kramhome.Path("skills"); err == nil {
		out = append(out, scanSkillDir(globalDir, "global")...)
	}
	return out
}

func scanSkillDir(dir, scope string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		name, description, body := parseSkillFrontmatter(string(data))
		if name == "" {
			name = e.Name() // fall back to the directory name
		}
		out = append(out, Skill{Name: name, Description: description, Path: path, Scope: scope, Body: body})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// parseSkillFrontmatter reads a minimal "---\nkey: value\n---\nbody" block
// — just enough for name/description, not a general YAML parser (adding a
// YAML dependency for two fields isn't worth it). It does handle YAML's
// block-scalar description forms (`description: >` folded, `description:
// |` literal, each followed by indented continuation lines) since that's
// a common real-world shape for a longer description — skipping it would
// silently produce an empty description for any skill authored that way,
// which is exactly the bug found copying in a real external skill
// (ponytail, github.com/DietrichGebert/ponytail) that uses `>`.
func parseSkillFrontmatter(content string) (name, description, body string) {
	const delim = "---"
	if !strings.HasPrefix(content, delim) {
		return "", "", content
	}
	rest := content[len(delim):]
	end := strings.Index(rest, "\n"+delim)
	if end == -1 {
		return "", "", content
	}
	frontmatter := rest[:end]
	body = strings.TrimLeft(rest[end+len(delim)+1:], "\n")

	lines := strings.Split(frontmatter, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if key == "description" && (value == ">" || value == "|") {
			folded := value == ">"
			var parts []string
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "\t")) {
				i++
				parts = append(parts, strings.TrimSpace(lines[i]))
			}
			sep := "\n"
			if folded {
				sep = " "
			}
			value = strings.Join(parts, sep)
		}

		switch key {
		case "name":
			name = value
		case "description":
			description = value
		}
	}
	return name, description, body
}

type skillList struct {
	registry *Registry
}

func newSkillList(r *Registry) *skillList { return &skillList{registry: r} }

func (t *skillList) Name() string { return "skill_list" }
func (t *skillList) Description() string {
	return "List every available skill (name and one-line description only — call the skill tool with a name to load its full instructions). Skills are optional, curated playbooks for specific kinds of tasks; check this when a request matches a specialized workflow you're not sure how to approach."
}
func (t *skillList) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}
func (t *skillList) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var b strings.Builder
	shown := 0
	for _, s := range t.registry.Skills() {
		if s.Disabled {
			continue
		}
		fmt.Fprintf(&b, "- %s [%s]: %s\n", s.Name, s.Scope, s.Description)
		shown++
	}
	if shown == 0 {
		return "(no skills installed)", nil
	}
	return b.String(), nil
}

type skillLoad struct {
	registry *Registry
}

func newSkillLoad(r *Registry) *skillLoad { return &skillLoad{registry: r} }

func (t *skillLoad) Name() string { return "skill" }
func (t *skillLoad) Description() string {
	return "Load one skill's full instructions by name (see skill_list for available names). Returns the skill's complete markdown body — follow it for the task at hand."
}
func (t *skillLoad) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Exact skill name, as returned by skill_list."}
		},
		"required": ["name"]
	}`)
}

type skillLoadArgs struct {
	Name string `json:"name"`
}

func (t *skillLoad) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args skillLoadArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	for _, s := range t.registry.Skills() {
		if s.Name != args.Name {
			continue
		}
		if s.Disabled {
			return fmt.Sprintf("error: skill %q is disabled", args.Name), nil
		}
		return s.Body, nil
	}
	return fmt.Sprintf("error: no skill named %q (call skill_list to see what's available)", args.Name), nil
}
