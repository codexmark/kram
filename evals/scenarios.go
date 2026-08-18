package main

import (
	"os"
	"path/filepath"
	"strings"
)

// verdict is a scenario's outcome — three states, not two, specifically to
// avoid a false PASS: a scenario whose check depends on the model taking a
// particular action (calling a specific tool, say) but the model simply
// never attempted that action at all didn't pass the property under test —
// it never exercised it. Reporting that as PASS would make the harness lie
// about what it actually verified; SkipVerdict says "inconclusive" instead.
type verdict string

const (
	PassVerdict verdict = "PASS"
	FailVerdict verdict = "FAIL"
	SkipVerdict verdict = "SKIP"
)

// scenarios is the actual eval suite. Each one is a regression test for
// specific, real behavior — most traced directly back to a bug found by
// hand earlier in this project (see DECISIONS.md for the full story
// behind each). Hard scenarios must pass regardless of which model is
// configured; soft ones are informational since they depend on a
// specific model's capability level, not just Kram's own code being
// correct. A soft scenario can still FAIL (the property was exercised and
// the model didn't do the right thing) or SKIP (the model never
// exercised the property at all, so nothing was actually checked) — see
// verdict's doc comment for why that distinction matters.
var scenarios = []scenario{
	{
		name: "never_returns_empty_final_answer",
		// Regression test for the exact bug reported as "Kram stops
		// silently": a free-tier model reading bash's old "[exit error:
		// ...]" framing for a routine non-zero exit sometimes gave up and
		// returned a genuinely empty completion. The fix (bash's
		// non-editorializing exit framing, plus the agent loop's
		// one-retry-then-visible-fallback for an empty final answer)
		// guarantees the user always sees *something* — this checks that
		// guarantee holds against the real configured provider, not just
		// the mock. Every call to sendAndWait exercises this property, so
		// this scenario is never SKIP — only PASS or FAIL.
		check: func(e *env) (verdict, string) {
			final, _, err := e.sendAndWait("say hello in one short sentence")
			if err != nil {
				return FailVerdict, err.Error()
			}
			if strings.TrimSpace(final.Message.Content) == "" {
				return FailVerdict, "final answer was empty"
			}
			return PassVerdict, "got a non-empty final answer"
		},
	},
	{
		name: "grep_never_returns_binary_garbage",
		soft: true,
		// Regression test for grep walking into .kram/ (the daemon's own
		// live SQLite database) and returning control-byte garbage as
		// search matches — found by hand while diagnosing the empty-
		// answer bug above. Confirms the fix (`.kram` in the ignore list,
		// plus a NUL-byte sniff for binary files elsewhere) holds when a
		// real model actually drives the tool, not just the unit test
		// calling grep directly.
		//
		// This is *the* scenario that motivated verdict having a SKIP
		// state: whether the model chooses to call grep for this prompt
		// is not something the eval controls, so "grep was never called"
		// is not evidence the fix works — it's evidence of nothing. The
		// old boolean-pass version of this check reported that case as a
		// PASS, which was a real false positive: a run where the model
		// never touched grep at all looked identical in the report to a
		// run that genuinely exercised the fix.
		check: func(e *env) (verdict, string) {
			final, tools, err := e.sendAndWait("use the grep tool to search this whole project for the word \"the\"")
			if err != nil {
				return FailVerdict, err.Error()
			}
			if !contains(tools, "grep") {
				return SkipVerdict, "model didn't call grep for this prompt — the fix was never exercised"
			}
			if strings.Contains(final.Message.Content, "kram-daemon.db") {
				return FailVerdict, "response referenced the daemon's own database file — grep should never have surfaced it"
			}
			return PassVerdict, "grep was called and no binary garbage surfaced"
		},
	},
	{
		name: "skill_used_when_explicitly_relevant",
		soft: true, // depends on the model actually reading and following skillsGuidance
		setup: func(e *env) error {
			dir := filepath.Join(e.workspace, ".kram", "skills", "eval-marker-skill")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			content := "---\nname: eval-marker-skill\ndescription: Use this skill whenever the user says the exact phrase \"zzz-eval-trigger-zzz\".\n---\nIf this skill loaded, say the word BANANA somewhere in your reply.\n"
			return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644)
		},
		// Unlike the grep scenario above, this prompt *is* an unmistakable,
		// deliberately engineered trigger — the eval itself guarantees the
		// property is exercised (the trigger phrase always appears), so
		// the model not calling skill_list is a genuine soft-fail, not an
		// inconclusive result. SKIP is for "the eval didn't force this
		// path to be taken", not for "the model chose wrong".
		check: func(e *env) (verdict, string) {
			_, tools, err := e.sendAndWait("zzz-eval-trigger-zzz — please proceed")
			if err != nil {
				return FailVerdict, err.Error()
			}
			if contains(tools, "skill_list") || contains(tools, "skill") {
				return PassVerdict, "model checked skills for an unmistakably skill-shaped trigger phrase"
			}
			return FailVerdict, "model never called skill_list/skill despite an exact, unambiguous trigger phrase"
		},
	},
	{
		name: "memory_saved_when_explicitly_asked",
		// The floor, not the ceiling: proactive memory (saving a fact
		// volunteered without being asked to remember it) is the soft
		// scenario below — this one just confirms memory_write actually
		// works end to end when directly instructed, which must never
		// regress regardless of model capability. Hard, and always
		// exercised (the instruction is explicit and unambiguous), so
		// never SKIP.
		check: func(e *env) (verdict, string) {
			_, tools, err := e.sendAndWait("remember this fact permanently: my favorite number is 8675309")
			if err != nil {
				return FailVerdict, err.Error()
			}
			if contains(tools, "memory_write") {
				return PassVerdict, "memory_write was called when explicitly asked to remember"
			}
			return FailVerdict, "memory_write was never called despite an explicit, unambiguous request to remember something"
		},
	},
	{
		name: "memory_saved_proactively",
		soft: true, // this is exactly the capability-dependent behavior skillsGuidance/memoryGuidance target
		// Same reasoning as skill_used_when_explicitly_relevant: the
		// prompt always volunteers a durable preference, so the property
		// is always exercised. A model that doesn't save it soft-fails;
		// there's no scenario here where nothing was tested.
		check: func(e *env) (verdict, string) {
			_, tools, err := e.sendAndWait("hi, just so you know for later, I always prefer terse answers with no preamble")
			if err != nil {
				return FailVerdict, err.Error()
			}
			if contains(tools, "memory_write") {
				return PassVerdict, "model proactively saved a volunteered preference without being asked to remember it"
			}
			return FailVerdict, "model stated a durable preference but never proactively called memory_write"
		},
	},
	{
		name: "core_tools_are_registered",
		// Doesn't depend on the model choosing to call anything — just
		// that these capabilities exist and are reachable, which would
		// regress silently (no error, no crash, just a quietly smaller
		// tool list) if a future refactor dropped one from the registry.
		// Deterministic and always exercised: never SKIP.
		check: func(e *env) (verdict, string) {
			tools, _, err := e.client.ListTools(e.ctx)
			if err != nil {
				return FailVerdict, err.Error()
			}
			names := make(map[string]bool, len(tools))
			for _, t := range tools {
				names[t.Name] = true
			}
			var missing []string
			for _, want := range []string{
				"ask_question", "memory_write", "skill_list", "delegate_task",
				"run_background", "artifact_read", "session_search", "lsp_diagnostics",
			} {
				if !names[want] {
					missing = append(missing, want)
				}
			}
			if len(missing) > 0 {
				return FailVerdict, "missing from the registry: " + strings.Join(missing, ", ")
			}
			return PassVerdict, "all core tools present in the registry"
		},
	},
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
