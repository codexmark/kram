package agent

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/codexmark/kram/internal/openai"
)

// Verification gate (#116): "run the tests before declaring success" is
// advice the model can ignore when it only lives in a skill or the system
// prompt. This file makes it structural: when a run has modified source
// files and no verification-capable tool ran afterwards, runLoop refuses
// the first final (no-tool-call) answer and continues one extra iteration
// with an explicit postscript nudge (verifyNudge). Bounded by design:
// exactly one nudge per run, never on the near-budget wrap-up turn, and
// doc-only edits never trigger it — a structural check in the loop that
// cannot loop.

// verifyTracker follows, batch by batch and in execution order, whether
// the run currently has an unverified source mutation outstanding.
//
// It deliberately observes tool *calls* rather than their outcomes, the
// same batch-level view batchHasMutation takes for auto-checkpoints. The
// trade-off is documented rather than hidden: a mutation the permission
// engine later denies can still arm the gate (one spurious, cheap nudge),
// and a denied bash can disarm it — both rare, both bounded, and both
// simpler than teaching this tracker to re-derive execution results.
type verifyTracker struct {
	pending bool
}

// observe walks one model turn's tool batch in its execution order
// (runToolBatch preserves batch order): a source-file mutation arms the
// gate, a verification call after it disarms it. A bash that runs before
// the batch's mutation therefore does not count for that mutation.
func (v *verifyTracker) observe(calls []openai.ToolCall) {
	for _, tc := range calls {
		name := tc.Function.Name
		switch {
		case isVerificationCall(name):
			v.pending = false
		case isFileMutationCall(name):
			if !mutationIsDocOnly(name, tc.Function.Arguments) {
				v.pending = true
			}
		}
	}
}

// isVerificationCall reports the calls that count as "the model checked
// its work": a foreground command (builds, tests, linters run through
// bash) or a language-server diagnostics pass. run_background does not
// count — starting a dev server is not verifying a change. An exit code
// is deliberately not required to be zero: running the tests and honestly
// reporting a failure satisfies the gate; hiding one is a model-behavior
// problem no loop check can fix.
func isVerificationCall(name string) bool {
	return name == "bash" || name == "lsp_diagnostics"
}

// isFileMutationCall lists the structured mutating file tools. bash can
// also mutate files, but it is unknowable from here and already counts as
// verification; the structured tools are the path the system prompt
// steers edits through.
func isFileMutationCall(name string) bool {
	switch name {
	case "write_file", "edit_file", "delete_file", "move_file":
		return true
	}
	return false
}

// docExtensions is the "trivial/doc-only" carve-out from #116: prose
// files whose edits have no build or test to run.
var docExtensions = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".rst": true, ".adoc": true,
}

// mutationIsDocOnly reports whether this mutating call touches only
// documentation paths. Unparseable or empty paths count as source: the
// gate must fail toward one extra nudge, never toward silence.
func mutationIsDocOnly(name, arguments string) bool {
	var args struct {
		Path    string `json:"path"`
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return false
	}
	paths := []string{args.Path}
	if name == "move_file" {
		paths = []string{args.OldPath, args.NewPath}
	}
	for _, p := range paths {
		if p == "" || !docExtensions[strings.ToLower(path.Ext(p))] {
			return false
		}
	}
	return true
}

// verifyNudge is the one-shot postscript the gate injects — see
// compileTurnPostscript. It explicitly offers the "state why not" exit so
// a change with genuinely nothing to run (config tweak in a repo with no
// test harness, say) costs one sentence, not a fabricated test run.
const verifyNudge = "[Kram verification gate: this run modified source files, but no build, test, or check command ran afterwards. Before finishing, run the project's relevant build or tests and report the result — or state explicitly in your answer why verification is not needed here.]"
