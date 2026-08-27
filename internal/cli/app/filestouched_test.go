package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

func TestTouchedFilesCollectsMutationToolsOnly(t *testing.T) {
	activities := []daemonclient.ToolActivity{
		{Name: "read_file", Args: `{"path":"a.go"}`},
		{Name: "grep", Args: `{"pattern":"foo"}`},
		{Name: "edit_file", Args: `{"path":"b.go","old_string":"x","new_string":"y"}`},
		{Name: "write_file", Args: `{"path":"c.go","content":"z"}`},
		{Name: "delete_file", Args: `{"path":"d.go"}`},
	}
	got := touchedFiles(activities)
	want := []string{"b.go", "c.go", "d.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("touchedFiles(...) = %v, want %v (read_file/grep excluded)", got, want)
	}
}

func TestTouchedFilesMoveFileIncludesBothPaths(t *testing.T) {
	activities := []daemonclient.ToolActivity{
		{Name: "move_file", Args: `{"old_path":"old.go","new_path":"new.go"}`},
	}
	got := touchedFiles(activities)
	want := []string{"old.go", "new.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("touchedFiles(move) = %v, want %v", got, want)
	}
}

func TestTouchedFilesDeduplicatesPreservingFirstSeenOrder(t *testing.T) {
	activities := []daemonclient.ToolActivity{
		{Name: "edit_file", Args: `{"path":"a.go"}`},
		{Name: "edit_file", Args: `{"path":"b.go"}`},
		{Name: "edit_file", Args: `{"path":"a.go"}`},
	}
	got := touchedFiles(activities)
	want := []string{"a.go", "b.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("touchedFiles(dup) = %v, want %v", got, want)
	}
}

func TestTouchedFilesMalformedArgsAreSkippedNotPanicked(t *testing.T) {
	activities := []daemonclient.ToolActivity{
		{Name: "edit_file", Args: `not json`},
		{Name: "edit_file", Args: `{"path":"ok.go"}`},
	}
	got := touchedFiles(activities)
	want := []string{"ok.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("touchedFiles(malformed) = %v, want %v", got, want)
	}
}

func TestTouchedFilesEmptyActivitiesReturnsNil(t *testing.T) {
	if got := touchedFiles(nil); got != nil {
		t.Errorf("touchedFiles(nil) = %v, want nil", got)
	}
}

func TestRenderFilesTouchedEmptyReturnsEmptyString(t *testing.T) {
	if got := renderFilesTouched(nil); got != "" {
		t.Errorf("renderFilesTouched(nil) = %q, want empty", got)
	}
}

func TestRenderFilesTouchedShowsEveryPathUnderLimit(t *testing.T) {
	paths := []string{"a.go", "b.go", "c.go"}
	got := renderFilesTouched(paths)
	for _, p := range paths {
		if !strings.Contains(got, p) {
			t.Errorf("renderFilesTouched(...) missing %q, got: %q", p, got)
		}
	}
	if strings.Contains(got, "more") {
		t.Errorf("expected no overflow suffix under the limit, got: %q", got)
	}
}

func TestRenderFilesTouchedFoldsOverflowIntoSuffix(t *testing.T) {
	paths := make([]string, filesTouchedShownLimit+3)
	for i := range paths {
		paths[i] = string(rune('a'+i)) + ".go"
	}
	got := renderFilesTouched(paths)
	if !strings.Contains(got, "+3 more") {
		t.Errorf("expected a \"+3 more\" overflow suffix, got: %q", got)
	}
	if strings.Contains(got, paths[filesTouchedShownLimit]) {
		t.Errorf("expected the (limit+1)th path to be folded into the overflow, not shown, got: %q", got)
	}
}
