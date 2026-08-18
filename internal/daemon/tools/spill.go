package tools

import (
	"fmt"
	"os"
	"time"

	"github.com/codexmark/kram/internal/artifact"
)

// artifactMaxAge is how long a spilled tool result stays on disk before
// Registry construction's best-effort GC pass may remove it — generous
// enough that an artifact referenced earlier in a long session is still
// there later, short enough that .kram/artifacts doesn't grow forever
// across a project's lifetime.
const artifactMaxAge = 7 * 24 * time.Hour

// artifactPreviewChars bounds the "here's a taste" preview shown right
// after a spill — enough to tell the model whether the full artifact is
// worth reading back, small enough that the preview itself can never
// recreate the unbounded-context problem artifacts exist to avoid.
const artifactPreviewChars = 2000

// spillResult turns a finished SpillWriter into the text a tool should
// return: the buffered output as-is if it never crossed the spill
// threshold, or a preview plus an artifact:// reference if it did. Callers
// (bash, custom tools) pass the same writer they used as cmd.Stdout/Stderr.
func spillResult(store *artifact.Store, sw *artifact.SpillWriter, toolName string) (text string, spilled bool, err error) {
	sw.Close()

	if !sw.Spilled() {
		return string(sw.Bytes()), false, nil
	}

	a, saveErr := store.SaveFile("", "", toolName, sw.TempPath())
	if saveErr != nil {
		os.Remove(sw.TempPath())
		return "", true, saveErr
	}

	preview, _ := store.Preview(a.ID, artifactPreviewChars)
	text = fmt.Sprintf(
		"[kram: output was %d bytes, too large to inline — saved as artifact %s]\n\npreview:\n%s\n\nUse artifact_read with id %q (and an offset) to read more of it.",
		a.Size, a.ID, preview, a.ID,
	)
	return text, true, nil
}
