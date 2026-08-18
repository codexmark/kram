package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSkillFrontmatterSimpleValues(t *testing.T) {
	content := "---\nname: my-skill\ndescription: a one-line description\n---\nBody text here.\n"
	name, desc, body := parseSkillFrontmatter(content)
	if name != "my-skill" {
		t.Errorf("name = %q, want my-skill", name)
	}
	if desc != "a one-line description" {
		t.Errorf("description = %q, want a one-line description", desc)
	}
	if strings.TrimSpace(body) != "Body text here." {
		t.Errorf("body = %q", body)
	}
}

// Regression test: the ponytail skill (a real, external, MIT-licensed
// skill actually installed into Kram) uses YAML's folded block scalar for
// its description, and the naive line-by-line parser silently produced
// an empty string for it. This is the exact case that broke.
func TestParseSkillFrontmatterFoldedBlockScalar(t *testing.T) {
	content := strings.Join([]string{
		"---",
		"name: ponytail",
		"description: >",
		"  Forces the laziest solution that actually works, simplest, shortest, most",
		"  minimal. Channels a senior dev who has seen everything.",
		"argument-hint: \"[lite|full|ultra]\"",
		"---",
		"# Ponytail",
		"",
		"Body.",
	}, "\n")

	name, desc, _ := parseSkillFrontmatter(content)
	if name != "ponytail" {
		t.Errorf("name = %q, want ponytail", name)
	}
	want := "Forces the laziest solution that actually works, simplest, shortest, most minimal. Channels a senior dev who has seen everything."
	if desc != want {
		t.Errorf("folded description not joined correctly:\ngot:  %q\nwant: %q", desc, want)
	}
}

func TestParseSkillFrontmatterLiteralBlockScalar(t *testing.T) {
	content := strings.Join([]string{
		"---",
		"name: multiline",
		"description: |",
		"  line one",
		"  line two",
		"---",
		"body",
	}, "\n")

	_, desc, _ := parseSkillFrontmatter(content)
	if desc != "line one\nline two" {
		t.Errorf("literal block scalar should join with real newlines, got %q", desc)
	}
}

func TestParseSkillFrontmatterNoDelimiter(t *testing.T) {
	content := "just a plain markdown file, no frontmatter\n"
	name, desc, body := parseSkillFrontmatter(content)
	if name != "" || desc != "" {
		t.Errorf("expected no name/description without frontmatter, got name=%q desc=%q", name, desc)
	}
	if body != content {
		t.Errorf("without frontmatter, the whole content should be returned as body")
	}
}

func TestParseSkillFrontmatterUnclosedDelimiter(t *testing.T) {
	content := "---\nname: broken\nno closing delimiter here\n"
	name, _, body := parseSkillFrontmatter(content)
	if name != "" {
		t.Errorf("an unclosed frontmatter block should not be parsed, got name=%q", name)
	}
	if body != content {
		t.Error("unclosed frontmatter should fall back to returning the whole content as body")
	}
}

func TestDiscoverSkillsFallsBackToDirNameWhenNameMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate from the real global skills dir
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".kram", "skills", "my-directory-name")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\ndescription: no name field in here\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	skills := discoverSkills(dir)
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Name != "my-directory-name" {
		t.Errorf("name = %q, want fallback to directory name", skills[0].Name)
	}
}

func TestDiscoverSkillsSortedByName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	for _, name := range []string{"zzz-skill", "aaa-skill"} {
		skillDir := filepath.Join(dir, ".kram", "skills", name)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: " + name + "\ndescription: d\n---\nbody\n"
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	skills := discoverSkills(dir)
	if len(skills) != 2 || skills[0].Name != "aaa-skill" || skills[1].Name != "zzz-skill" {
		t.Errorf("expected sorted [aaa-skill, zzz-skill], got %+v", skills)
	}
}

func TestDiscoverSkillsMissingDirIsNotAnError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir() // no .kram/skills at all
	skills := discoverSkills(dir)
	if len(skills) != 0 {
		t.Errorf("expected no skills when the directory doesn't exist, got %+v", skills)
	}
}
