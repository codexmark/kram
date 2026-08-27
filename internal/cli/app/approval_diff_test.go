package app

import (
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

func approvalEvent(tool, subject, diff string) daemonclient.StreamEvent {
	return daemonclient.StreamEvent{
		Type: "approval", ApprovalID: "ap1", Tool: tool, Subject: subject, Diff: diff,
		Options: []string{"once", "always", "deny"},
	}
}

func TestNewPendingApprovalWithDiff(t *testing.T) {
	a := newPendingApproval(approvalEvent("edit_file", "f.txt", "--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-old\n+new\n"), 80)
	if a.diff == "" {
		t.Fatal("expected the diff to be carried")
	}
	// With a diff, the viewport is initialized (sized to content, capped).
	if a.diffVP.Height <= 0 || a.diffVP.Height > approvalDiffMaxRows {
		t.Errorf("diff viewport height = %d, want 1..%d", a.diffVP.Height, approvalDiffMaxRows)
	}

	// Without a diff, no viewport content is set (the common non-file case).
	b := newPendingApproval(approvalEvent("bash", "rm -rf x", ""), 80)
	if b.diff != "" {
		t.Errorf("bash approval should carry no diff, got %q", b.diff)
	}
}

func TestRenderApprovalShowsDiffAndAlwaysCopy(t *testing.T) {
	diff := "--- a/f.txt\n+++ b/f.txt\n@@ -1,2 +1,2 @@\n-alpha\n+ALPHA\n beta\n"
	m := Model{width: 80, approval: newPendingApproval(approvalEvent("edit_file", "f.txt", diff), 80)}
	out := m.renderApproval()

	if !strings.Contains(out, "diff · f.txt") {
		t.Errorf("render missing the diff header:\n%s", out)
	}
	// The diff content (via the viewport) should be present.
	if !strings.Contains(out, "ALPHA") {
		t.Errorf("render missing diff content:\n%s", out)
	}
	// The "always" option must make the whole-path grant explicit for a file tool.
	if !strings.Contains(out, "allow all future edits to f.txt") {
		t.Errorf("render missing the per-path always clarification:\n%s", out)
	}
	// The scroll hint appears when there's a diff.
	if !strings.Contains(out, "scroll diff") {
		t.Errorf("render missing the scroll hint:\n%s", out)
	}
}

func TestRenderApprovalNoDiffUnchanged(t *testing.T) {
	m := Model{width: 80, approval: newPendingApproval(approvalEvent("bash", "git push", ""), 80)}
	out := m.renderApproval()
	if strings.Contains(out, "diff ·") || strings.Contains(out, "scroll diff") {
		t.Errorf("a diffless approval should not show diff chrome:\n%s", out)
	}
	// A non-file tool's "always" must NOT claim per-path edit access.
	if strings.Contains(out, "allow all future edits") {
		t.Errorf("bash approval should not show the file-edit always copy:\n%s", out)
	}
}

func TestColorizeUnifiedDiffPreservesLinesAndContent(t *testing.T) {
	diff := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-gone\n+added\n unchanged"
	got := colorizeUnifiedDiff(diff)
	// One rendered line per input line — no lines dropped or added (styling
	// wraps each line but must not change their count).
	if a, b := strings.Count(got, "\n"), strings.Count(diff, "\n"); a != b {
		t.Errorf("line count changed: got %d newlines, want %d", a, b)
	}
	// The textual content survives colorization (color may be stripped in a
	// non-TTY test env, but the words must still be there).
	for _, want := range []string{"gone", "added", "unchanged", "@@"} {
		if !strings.Contains(got, want) {
			t.Errorf("colorized diff lost %q:\n%s", want, got)
		}
	}
}

// TestClassifyDiffLineInsideHunk is the regression test for the miscoloring
// bug: a deleted source line reading "--i;" is emitted as "---i;" and an
// added "++i;" as "+++i;" — inside a hunk these must classify as del/add,
// not as file headers.
func TestClassifyDiffLineInsideHunk(t *testing.T) {
	// File headers (before any @@) ARE headers.
	if k, in := classifyDiffLine("--- a/x", false); k != diffHeader || in {
		t.Errorf("file header --- : kind=%v inHunk=%v, want header/false", k, in)
	}
	if k, in := classifyDiffLine("+++ b/x", false); k != diffHeader || in {
		t.Errorf("file header +++ : kind=%v inHunk=%v, want header/false", k, in)
	}
	// The @@ line flips us into the hunk body.
	if k, in := classifyDiffLine("@@ -1,3 +1,3 @@", false); k != diffHeader || !in {
		t.Errorf("hunk header: kind=%v inHunk=%v, want header/true", k, in)
	}
	// Inside a hunk, "---i;" (a deleted "--i;") is a DELETION, not a header.
	if k, _ := classifyDiffLine("---i;", true); k != diffDel {
		t.Errorf("--- prefixed content inside hunk = %v, want deletion", k)
	}
	if k, _ := classifyDiffLine("+++i;", true); k != diffAdd {
		t.Errorf("+++ prefixed content inside hunk = %v, want addition", k)
	}
	// Ordinary add/del/context inside a hunk.
	if k, _ := classifyDiffLine("-gone", true); k != diffDel {
		t.Errorf("-gone = %v, want deletion", k)
	}
	if k, _ := classifyDiffLine("+added", true); k != diffAdd {
		t.Errorf("+added = %v, want addition", k)
	}
	if k, _ := classifyDiffLine(" context", true); k != diffContext {
		t.Errorf("' context' = %v, want context", k)
	}
}

// TestNewPendingApprovalSizesViewportToDiff confirms a small diff doesn't
// reserve the full max height (no dead padding), while a large one caps.
func TestNewPendingApprovalSizesViewportToDiff(t *testing.T) {
	small := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n" // 5 lines + trailing newline
	a := newPendingApproval(approvalEvent("edit_file", "x", small), 80)
	if a.diffVP.Height >= approvalDiffMaxRows {
		t.Errorf("small diff viewport height = %d, want < cap %d (sized to content)", a.diffVP.Height, approvalDiffMaxRows)
	}
	big := newPendingApproval(approvalEvent("edit_file", "x", strings.Repeat("+line\n", 100)), 80)
	if big.diffVP.Height != approvalDiffMaxRows {
		t.Errorf("large diff viewport height = %d, want capped at %d", big.diffVP.Height, approvalDiffMaxRows)
	}
}

func TestApprovalDiffScrollKeysDoNotPanic(t *testing.T) {
	m := Model{width: 80, approval: newPendingApproval(approvalEvent("edit_file", "f.txt", strings.Repeat("+line\n", 100)), 80)}
	for _, key := range []string{"pgdown", "pgup", "end", "home", "down", "up"} {
		next, _ := m.handleApprovalKey(keyMsg(key))
		m = next.(Model)
		if m.approval == nil {
			t.Fatalf("approval cleared unexpectedly on %q", key)
		}
	}
}
