package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// Tool output is where a coding agent's context budget actually goes. A
// single `npm install` or `go test ./...` can be thousands of lines of
// progress bars and passing-test noise carrying maybe five lines of
// signal, and it lands in history verbatim, on every subsequent turn, for
// the rest of the session.
//
// This is a deterministic filter pass over that output, adapted from
// OmniRoute's RTK compression engine — the specific idea worth stealing
// being that it's keyed on the *command that produced the output* and
// uses plain regex rules, so it costs no LLM call, adds no latency, and
// can't hallucinate. The critical invariant is that it must never eat an
// error: preserve patterns are checked before any drop rule, so a line
// that looks like a failure survives no matter what else matches.

// outputFilter is one rule set, applied when its command pattern matches
// the command that was run.
type outputFilter struct {
	name string
	// match selects which commands this applies to.
	match *regexp.Regexp
	// drop removes matching lines — noise, progress, per-file success.
	drop []*regexp.Regexp
	// preserve wins over drop: a line matching any of these is always
	// kept, which is what keeps a filter from swallowing failures.
	preserve []*regexp.Regexp
	// maxLines caps the result; beyond it, head and tail are kept and the
	// middle is replaced with a count of what was elided. Whatever was
	// preserved is kept regardless.
	maxLines int
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// errorish are the patterns that mean "something went wrong" across the
// toolchains a coding agent actually drives. Shared by every filter as a
// baseline preserve set, on top of whatever each adds.
var errorish = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\berror\b`),
	regexp.MustCompile(`(?i)\bfail(ed|ure|ing)?\b`),
	regexp.MustCompile(`(?i)\bpanic\b`),
	regexp.MustCompile(`(?i)\bfatal\b`),
	regexp.MustCompile(`(?i)\bexception\b`),
	regexp.MustCompile(`(?i)\bwarn(ing)?\b`),
	regexp.MustCompile(`(?i)\bcannot\b|\bunable to\b|\bnot found\b`),
	regexp.MustCompile(`(?i)\bdenied\b|\brefused\b|\btimed? ?out\b`),
	regexp.MustCompile(`^\s*at\s+.+:\d+`),    // stack frames
	regexp.MustCompile(`^[^:]+:\d+:\d+:`),    // file:line:col diagnostics
	regexp.MustCompile(`(?i)^\s*---\s*FAIL`), // go test
}

var outputFilters = []outputFilter{
	{
		name:  "package-install",
		match: regexp.MustCompile(`^\s*(npm|pnpm|yarn|bun)\s+(i|install|add|ci)\b`),
		drop: []*regexp.Regexp{
			regexp.MustCompile(`^\s*[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]`),                   // spinners
			regexp.MustCompile(`^\s*\[?[=#\-]{3,}.*\]?\s*\d*%?\s*$`), // progress bars
			regexp.MustCompile(`(?i)^\s*(added|removed|changed|audited)\s+\d+\s+package`),
			regexp.MustCompile(`(?i)^\s*(reify|idealTree|timing|npm notice)`),
			regexp.MustCompile(`^\s*[+-]\s+\S+@\S+\s*$`), // per-package lines
		},
		maxLines: 40,
	},
	{
		name:  "go-test",
		match: regexp.MustCompile(`^\s*go\s+test\b`),
		drop: []*regexp.Regexp{
			regexp.MustCompile(`^ok\s+\S+\s+[\d.]+s`),   // passing package
			regexp.MustCompile(`^\?\s+\S+\s+\[no test`), // no test files
			regexp.MustCompile(`^=== RUN\s`),
			regexp.MustCompile(`^\s*--- PASS:`),
			regexp.MustCompile(`^PASS$`),
		},
		preserve: []*regexp.Regexp{regexp.MustCompile(`^(FAIL|ok\s+\S+\s+\[build failed\])`)},
		maxLines: 80,
	},
	{
		name:  "js-test",
		match: regexp.MustCompile(`^\s*(npx\s+)?(jest|vitest|mocha|ava)\b|^\s*(npm|pnpm|yarn|bun)\s+(run\s+)?test\b`),
		drop: []*regexp.Regexp{
			regexp.MustCompile(`^\s*[✓✔√]\s`), // passing cases
			regexp.MustCompile(`^\s*PASS\s`),
			regexp.MustCompile(`^\s*$`),
		},
		preserve: []*regexp.Regexp{
			regexp.MustCompile(`^\s*[✗✘×]\s|^\s*FAIL\s`),
			regexp.MustCompile(`(?i)tests?:.*(failed|passed)`),
		},
		maxLines: 80,
	},
	{
		name:  "build",
		match: regexp.MustCompile(`^\s*(go\s+build|tsc|npx\s+tsc|vite\s+build|webpack|cargo\s+build|make)\b`),
		drop: []*regexp.Regexp{
			regexp.MustCompile(`(?i)^\s*(compiling|building|transforming|bundling)\b`),
			regexp.MustCompile(`^\s*\d+\s+modules? transformed`),
			regexp.MustCompile(`(?i)^\s*(built|done|finished) in [\d.]+`),
		},
		maxLines: 60,
	},
	{
		name:  "git-status",
		match: regexp.MustCompile(`^\s*git\s+status\b`),
		drop: []*regexp.Regexp{
			regexp.MustCompile(`^\s*\(use "git`), // hint lines
			regexp.MustCompile(`^\s*$`),
		},
		maxLines: 60,
	},
	{
		name:     "generic-long",
		match:    regexp.MustCompile(`.`), // last resort: any command
		maxLines: 200,
	},
}

// filterCommandOutput trims noise from output produced by command. It
// always returns something: if filtering would leave nothing, the
// original is returned rather than an empty result, since a silent empty
// tool result is worse than a noisy one.
func filterCommandOutput(command, output string) string {
	if strings.TrimSpace(output) == "" {
		return output
	}
	clean := ansiPattern.ReplaceAllString(output, "")

	var f outputFilter
	for _, candidate := range outputFilters {
		if candidate.match.MatchString(command) {
			f = candidate
			break
		}
	}

	lines := strings.Split(strings.TrimRight(clean, "\n"), "\n")
	kept := make([]string, 0, len(lines))
	dropped := 0
	for _, line := range lines {
		if matchesAny(line, f.preserve) || matchesAny(line, errorish) {
			kept = append(kept, line)
			continue
		}
		if matchesAny(line, f.drop) {
			dropped++
			continue
		}
		kept = append(kept, line)
	}

	kept, elided := truncateMiddle(kept, f.maxLines)

	// Everything matched a drop rule, which means every line was routine
	// — the clean-run case. Returning the full original here (the obvious
	// "never return empty" fallback) would be the worst of both worlds:
	// no signal added, no tokens saved. Say what happened instead, and
	// keep the last line as evidence of how it ended.
	if len(kept) == 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" {
			return fmt.Sprintf("[kram: %d lines of routine output, nothing notable]", dropped)
		}
		return fmt.Sprintf("%s\n\n[kram: %d lines of routine output hidden]", last, dropped-1)
	}

	result := strings.Join(kept, "\n")
	if hidden := dropped + elided; hidden > 0 {
		result += fmt.Sprintf("\n\n[kram: %d lines of routine output hidden]", hidden)
	}
	return result
}

// truncateMiddle keeps the head and tail of an over-long result — the
// start says what ran, the end says how it finished, and the middle is
// almost always the repetitive part.
func truncateMiddle(lines []string, maxLines int) ([]string, int) {
	if maxLines <= 0 || len(lines) <= maxLines {
		return lines, 0
	}
	head := maxLines / 2
	tail := maxLines - head
	out := make([]string, 0, maxLines)
	out = append(out, lines[:head]...)
	out = append(out, lines[len(lines)-tail:]...)
	return out, len(lines) - maxLines
}

func matchesAny(line string, patterns []*regexp.Regexp) bool {
	for _, p := range patterns {
		if p.MatchString(line) {
			return true
		}
	}
	return false
}
