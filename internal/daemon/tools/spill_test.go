package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codexmark/kram-gateway/internal/artifact"
)

func TestBashSpillsLargeOutputAsArtifactAndArtifactReadRetrievesIt(t *testing.T) {
	workspace := t.TempDir()
	store := artifact.Open(workspace)
	b := newBash(workspace, store)

	// Print well past bashMaxOutputBytes (50_000) so this must spill.
	args, _ := json.Marshal(bashArgs{Command: "yes xxxxxxxxxx | head -c 200000"})
	out, err := b.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "artifact art_") {
		t.Fatalf("expected a spilled result to reference an artifact id, got: %q", truncateForTest(out))
	}
	if len(out) > 4000 {
		t.Errorf("a spilled result should be a short preview, not the full 200000 bytes — got %d chars", len(out))
	}

	id := extractArtifactID(t, out)
	read := newArtifactRead(store)
	readOut, err := read.Execute(context.Background(), json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readOut, "xxxx") {
		t.Errorf("artifact_read should return the real spilled content, got: %q", truncateForTest(readOut))
	}
}

func TestBashSmallOutputStaysInlineNoArtifact(t *testing.T) {
	workspace := t.TempDir()
	store := artifact.Open(workspace)
	b := newBash(workspace, store)

	args, _ := json.Marshal(bashArgs{Command: "echo small-output"})
	out, err := b.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "artifact art_") {
		t.Errorf("small output should never spill to an artifact, got: %q", out)
	}
	if !strings.Contains(out, "small-output") {
		t.Errorf("expected the real output inline, got: %q", out)
	}
}

func TestCustomToolSpillsLargeOutput(t *testing.T) {
	workspace := t.TempDir()
	store := artifact.Open(workspace)
	ct := &customTool{
		workspace: workspace, artifacts: store,
		manifest: toolManifest{Name: "bigout", Command: "yes xxxxxxxxxx | head -c 200000"},
	}
	out, err := ct.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "artifact art_") {
		t.Fatalf("expected a spilled result to reference an artifact id, got: %q", truncateForTest(out))
	}
}

func TestArtifactReadOffsetPaging(t *testing.T) {
	workspace := t.TempDir()
	store := artifact.Open(workspace)
	a, err := store.Save("", "", "test", strings.NewReader(strings.Repeat("0123456789", 1000))) // 10000 bytes
	if err != nil {
		t.Fatal(err)
	}

	read := newArtifactRead(store)
	out, err := read.Execute(context.Background(), json.RawMessage(`{"id":"`+a.ID+`","limit":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "more bytes available") {
		t.Errorf("expected a paging hint for a partial read, got: %q", out)
	}
}

func TestArtifactReadUnknownID(t *testing.T) {
	store := artifact.Open(t.TempDir())
	read := newArtifactRead(store)
	out, err := read.Execute(context.Background(), json.RawMessage(`{"id":"art_0000000000000000"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("expected an error message for an unknown artifact id, got: %q", out)
	}
}

func truncateForTest(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func extractArtifactID(t *testing.T, s string) string {
	t.Helper()
	i := strings.Index(s, "art_")
	if i == -1 {
		t.Fatalf("no artifact id found in %q", truncateForTest(s))
	}
	end := i
	for end < len(s) && s[end] != '\n' && s[end] != ']' && s[end] != ' ' {
		end++
	}
	return s[i:end]
}
