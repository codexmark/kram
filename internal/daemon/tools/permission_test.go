package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// countingTool is a fake Tool used to observe whether Execute actually ran
// the underlying tool, without depending on a real shell — white-box
// (package tools) so it can be injected directly into a Registry's byName
// map, which real callers never do.
type countingTool struct {
	calls int
}

func (t *countingTool) Name() string            { return "fake_tool" }
func (t *countingTool) Description() string     { return "a fake tool for permission tests" }
func (t *countingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *countingTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	t.calls++
	return "ran", nil
}

// fakeApprover answers every Approve call with a fixed decision, and
// records whether it was called at all.
type fakeApprover struct {
	decision ApprovalDecision
	err      error
	called   bool
}

func (a *fakeApprover) Approve(ctx context.Context, toolName, subject string) (ApprovalDecision, error) {
	a.called = true
	return a.decision, a.err
}

func newPermTestRegistry(t *testing.T, permissionsJSON string) (*Registry, *countingTool) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	if permissionsJSON != "" {
		dir := filepath.Join(workspace, ".kram")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "permissions.json"), []byte(permissionsJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRegistry(workspace, nil, nil)
	ct := &countingTool{}
	r.byName[ct.Name()] = ct
	return r, ct
}

func TestPermissionAllowRunsTool(t *testing.T) {
	r, ct := newPermTestRegistry(t, "")
	out, err := r.Execute(context.Background(), "fake_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "ran" || ct.calls != 1 {
		t.Errorf("expected the tool to actually run under the default (allow) policy, got out=%q calls=%d", out, ct.calls)
	}
}

func TestPermissionDenyNeverRuns(t *testing.T) {
	r, ct := newPermTestRegistry(t, `{"rules":[{"tool":"fake_tool","decision":"deny"}]}`)
	out, err := r.Execute(context.Background(), "fake_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if ct.calls != 0 {
		t.Errorf("a denied tool must never actually run, calls=%d", ct.calls)
	}
	if out == "ran" {
		t.Errorf("denied call should not return the tool's real result, got %q", out)
	}
}

func TestPermissionAskWithoutApproverNeverRuns(t *testing.T) {
	r, ct := newPermTestRegistry(t, `{"rules":[{"tool":"fake_tool","decision":"ask"}]}`)
	// No approver in context — simulates a context where approval plumbing
	// isn't wired (e.g. a stray call path that forgot WithApprover).
	out, err := r.Execute(context.Background(), "fake_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if ct.calls != 0 {
		t.Errorf("an ask-gated tool with no approver available must never run, calls=%d", ct.calls)
	}
	if out == "ran" {
		t.Error("should not report the tool as having run")
	}
}

func TestPermissionAskOnceRunsButDoesNotPersist(t *testing.T) {
	r, ct := newPermTestRegistry(t, `{"rules":[{"tool":"fake_tool","decision":"ask"}]}`)
	approver := &fakeApprover{decision: ApprovalOnce}
	ctx := WithApprover(context.Background(), approver)

	out, err := r.Execute(ctx, "fake_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !approver.called || ct.calls != 1 || out != "ran" {
		t.Errorf("expected approve-once to run the tool exactly once, got called=%v calls=%d out=%q", approver.called, ct.calls, out)
	}
	if len(r.grants.Rules()) != 0 {
		t.Errorf("a one-time approval must not be persisted as a grant, got %d grants", len(r.grants.Rules()))
	}

	// A second call still asks — nothing was remembered.
	approver2 := &fakeApprover{decision: ApprovalDeny}
	ctx2 := WithApprover(context.Background(), approver2)
	_, _ = r.Execute(ctx2, "fake_tool", json.RawMessage(`{}`))
	if !approver2.called {
		t.Error("a second call after a once-approval should still require approval")
	}
}

func TestPermissionAskAlwaysPersistsAndSkipsFutureAsks(t *testing.T) {
	r, ct := newPermTestRegistry(t, `{"rules":[{"tool":"fake_tool","decision":"ask"}]}`)
	approver := &fakeApprover{decision: ApprovalAlways}
	ctx := WithApprover(context.Background(), approver)

	if _, err := r.Execute(ctx, "fake_tool", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if ct.calls != 1 {
		t.Fatalf("expected 1 call, got %d", ct.calls)
	}
	if len(r.grants.Rules()) != 1 {
		t.Fatalf("expected the always-approval to persist a grant, got %d", len(r.grants.Rules()))
	}

	// The grant took effect at NewRegistry time for *this* Evaluator
	// instance's rule list, but the Evaluator itself is immutable — a
	// grant earned mid-run takes effect on the next daemon/registry
	// lifetime, not instantly (see Evaluator's doc comment). Build a fresh
	// Registry against the same workspace to simulate that next run and
	// confirm the persisted grant is now honored without asking again.
	reloaded := NewRegistry(r.workspace, nil, nil)
	ct2 := &countingTool{}
	reloaded.byName[ct2.Name()] = ct2
	noApprover := context.Background() // deliberately no approver — must not be needed
	out, err := reloaded.Execute(noApprover, "fake_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "ran" || ct2.calls != 1 {
		t.Errorf("a persisted always-grant should allow the call without an approver on the next run, got out=%q calls=%d", out, ct2.calls)
	}
}

func TestPermissionAskDenyNeverRuns(t *testing.T) {
	r, ct := newPermTestRegistry(t, `{"rules":[{"tool":"fake_tool","decision":"ask"}]}`)
	approver := &fakeApprover{decision: ApprovalDeny}
	ctx := WithApprover(context.Background(), approver)

	if _, err := r.Execute(ctx, "fake_tool", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if ct.calls != 0 {
		t.Errorf("a call the user denied must never run, calls=%d", ct.calls)
	}
}

func TestPermissionDisabledPrevailsOverPolicy(t *testing.T) {
	r, ct := newPermTestRegistry(t, `{"rules":[{"tool":"fake_tool","decision":"allow"}]}`)
	r.disabled["fake_tool"] = true
	out, err := r.Execute(context.Background(), "fake_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if ct.calls != 0 || out == "ran" {
		t.Error("a disabled tool must not run even if the policy would allow it")
	}
}

func TestPermissionFullDenyHidesToolFromDefinitions(t *testing.T) {
	r, _ := newPermTestRegistry(t, `{"rules":[{"tool":"fake_tool","decision":"deny"}]}`)
	for _, d := range r.Definitions() {
		if d.Function.Name == "fake_tool" {
			t.Fatal("a fully-denied tool must not appear in Definitions()")
		}
	}
}

func TestPermissionPartialDenyStillShowsInDefinitions(t *testing.T) {
	r, _ := newPermTestRegistry(t, `{"rules":[
		{"tool":"bash","pattern":"rm -rf *","decision":"deny"},
		{"tool":"bash","pattern":"*","decision":"allow"}
	]}`)
	found := false
	for _, d := range r.Definitions() {
		if d.Function.Name == "bash" {
			found = true
		}
	}
	if !found {
		t.Error("a partially-denied tool (some patterns denied, others allowed) must still be offered")
	}
}

func TestPermissionCustomToolGoesThroughEngine(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	toolsDir := filepath.Join(workspace, ".kram", "tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"my_custom","description":"d","command":"cat"}`
	if err := os.WriteFile(filepath.Join(toolsDir, "my_custom.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	permJSON := `{"rules":[{"tool":"my_custom","decision":"deny"}]}`
	if err := os.WriteFile(filepath.Join(workspace, ".kram", "permissions.json"), []byte(permJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(workspace, nil, nil)
	out, err := r.Execute(context.Background(), "my_custom", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("expected a denial message")
	}
	for _, d := range r.Definitions() {
		if d.Function.Name == "my_custom" {
			t.Error("a fully-denied custom tool must not appear in Definitions() either")
		}
	}
}

func TestPolicySubjectExtractsCommandAndPath(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"bash", `{"command":"git push origin main"}`, "git push origin main"},
		{"delete_file", `{"path":"/tmp/x"}`, "/tmp/x"},
		{"move_file", `{"old_path":"a","new_path":"b"}`, "a -> b"},
		{"unknown_tool", `{"x":1}`, `{"x":1}`},
	}
	for _, c := range cases {
		got := policySubject(c.name, json.RawMessage(c.args))
		if got != c.want {
			t.Errorf("policySubject(%q, %q) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}
