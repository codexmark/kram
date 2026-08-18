package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSavePolicyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "permissions.json")
	want := RecommendedPolicy()

	if err := SavePolicy(want, path); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got PolicyFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Default != Allow || len(got.Rules) != len(want.Rules) {
		t.Errorf("saved policy did not round-trip: %+v", got)
	}
}

func TestSavePolicyOverwritesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permissions.json")
	if err := SavePolicy(RecommendedPolicy(), path); err != nil {
		t.Fatal(err)
	}
	if err := SavePolicy(StrictPolicy(), path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var got PolicyFile
	_ = json.Unmarshal(data, &got)
	if got.Default != Ask {
		t.Errorf("SavePolicy should overwrite, got Default=%q want %q", got.Default, Ask)
	}
}

// TestPresetsMatchDocumentedBehavior exercises each preset through the
// real Evaluator, not just checking the JSON shape — these are the exact
// scenarios the first-run wizard's copy promises the user.
func TestPresetsMatchDocumentedBehavior(t *testing.T) {
	t.Run("recommended allows routine work but asks before destructive ops", func(t *testing.T) {
		e := NewEvaluator(RecommendedPolicy(), nil)
		if got := e.Evaluate("read_file", "main.go"); got != Allow {
			t.Errorf("read_file: got %q, want allow", got)
		}
		if got := e.Evaluate("bash", "rm -rf node_modules"); got != Ask {
			t.Errorf("rm -rf: got %q, want ask", got)
		}
		if got := e.Evaluate("bash", "git push origin main"); got != Ask {
			t.Errorf("git push: got %q, want ask", got)
		}
		if got := e.Evaluate("delete_file", "foo.txt"); got != Ask {
			t.Errorf("delete_file: got %q, want ask", got)
		}
	})

	t.Run("strict asks for anything not explicitly allow-listed, including MCP tools", func(t *testing.T) {
		e := NewEvaluator(StrictPolicy(), nil)
		if got := e.Evaluate("read_file", "main.go"); got != Allow {
			t.Errorf("read_file: got %q, want allow", got)
		}
		if got := e.Evaluate("bash", "git status"); got != Allow {
			t.Errorf("git status: got %q, want allow", got)
		}
		if got := e.Evaluate("bash", "rm -rf /"); got != Deny {
			t.Errorf("rm -rf: got %q, want deny", got)
		}
		if got := e.Evaluate("mcp__github__create_issue", `{"title":"x"}`); got != Ask {
			t.Errorf("unlisted mcp tool: got %q, want ask (Strict's default, not just unlisted tool names)", got)
		}
		if got := e.Evaluate("write_file", `{"path":"x"}`); got != Ask {
			t.Errorf("unlisted write_file: got %q, want ask", got)
		}
	})

	t.Run("autonomous allows everything except absolute-path recursive deletes", func(t *testing.T) {
		e := NewEvaluator(AutonomousPolicy(), nil)
		if got := e.Evaluate("bash", "rm -rf node_modules"); got != Allow {
			t.Errorf("rm -rf node_modules (relative): got %q, want allow", got)
		}
		if got := e.Evaluate("bash", "rm -rf /"); got != Deny {
			t.Errorf("rm -rf / (absolute): got %q, want deny", got)
		}
		if got := e.Evaluate("bash", "rm -rf /tmp/scratch"); got != Deny {
			t.Errorf("rm -rf /tmp/scratch (absolute): got %q, want deny — the prefix glob catches any absolute path, not just \"/\" itself", got)
		}
		if got := e.Evaluate("delete_file", "foo.txt"); got != Allow {
			t.Errorf("delete_file: got %q, want allow", got)
		}
		if got := e.Evaluate("mcp__anything__anything", "{}"); got != Allow {
			t.Errorf("unlisted mcp tool: got %q, want allow", got)
		}
	})
}
