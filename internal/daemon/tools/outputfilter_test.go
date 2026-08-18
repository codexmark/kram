package tools

import (
	"strings"
	"testing"
)

// The whole risk of a filter that trims tool output is that it eats
// something that mattered. These tests exist to make that failure mode
// impossible to ship quietly: every filter is checked to preserve
// failures, and any new drop rule that swallows an error will fail here
// rather than silently hiding a broken build from the model.

func TestFilterPreservesFailures(t *testing.T) {
	cases := []struct {
		name     string
		command  string
		output   string
		mustKeep []string
	}{
		{
			name:    "go test failure survives the passing-noise rules",
			command: "go test ./...",
			output: strings.Join([]string{
				"=== RUN   TestOne",
				"--- PASS: TestOne (0.00s)",
				"ok  	github.com/x/a	0.012s",
				"=== RUN   TestTwo",
				"    thing_test.go:42: expected 3, got 4",
				"--- FAIL: TestTwo (0.01s)",
				"FAIL	github.com/x/b	0.013s",
			}, "\n"),
			mustKeep: []string{"--- FAIL: TestTwo", "expected 3, got 4", "FAIL\tgithub.com/x/b"},
		},
		{
			name:    "npm install error survives the progress rules",
			command: "npm install",
			output: strings.Join([]string{
				"⠋ reify:lodash: timing reifyNode:node_modules/x",
				"+ lodash@4.17.21",
				"added 214 packages, and audited 215 packages in 3s",
				"npm ERR! code ERESOLVE",
				"npm ERR! ERESOLVE unable to resolve dependency tree",
			}, "\n"),
			mustKeep: []string{"npm ERR! code ERESOLVE", "unable to resolve dependency tree"},
		},
		{
			name:    "jest failure survives the passing-case rules",
			command: "npx jest",
			output: strings.Join([]string{
				"✓ adds numbers (2 ms)",
				"✓ subtracts numbers",
				"✗ divides numbers",
				"  Expected: 2",
				"  Received: 3",
				"Tests: 1 failed, 2 passed, 3 total",
			}, "\n"),
			mustKeep: []string{"✗ divides numbers", "Tests: 1 failed"},
		},
		{
			name:     "compiler diagnostics survive the build rules",
			command:  "go build ./...",
			output:   "compiling package x\n./main.go:12:6: undefined: doesNotExist\n",
			mustKeep: []string{"./main.go:12:6: undefined: doesNotExist"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterCommandOutput(tc.command, tc.output)
			for _, want := range tc.mustKeep {
				if !strings.Contains(got, want) {
					t.Errorf("filter dropped a line that must survive.\nwanted to keep: %q\ngot:\n%s", want, got)
				}
			}
		})
	}
}

func TestFilterActuallyTrims(t *testing.T) {
	// A filter that keeps everything is useless; this is the other half
	// of the contract.
	output := strings.Join([]string{
		"=== RUN   TestOne",
		"--- PASS: TestOne (0.00s)",
		"=== RUN   TestTwo",
		"--- PASS: TestTwo (0.00s)",
		"PASS",
		"ok  	github.com/x/a	0.012s",
	}, "\n")

	got := filterCommandOutput("go test ./...", output)
	if len(got) >= len(output) {
		t.Errorf("all-passing test output was not trimmed at all:\n%s", got)
	}
	if !strings.Contains(got, "hidden") {
		t.Errorf("expected a note saying output was hidden, got:\n%s", got)
	}
}

func TestFilterNeverReturnsEmpty(t *testing.T) {
	// If every line matched a drop rule, returning nothing would tell the
	// model the command produced no output at all — a different and worse
	// claim than "it ran and was unremarkable".
	output := "--- PASS: TestOne (0.00s)\nPASS\n"
	got := filterCommandOutput("go test ./...", output)
	if strings.TrimSpace(got) == "" {
		t.Error("filter returned empty output")
	}
	if !strings.Contains(got, "routine output") {
		t.Errorf("an all-routine result should say so, got: %q", got)
	}
}

func TestFilterStripsANSI(t *testing.T) {
	got := filterCommandOutput("echo hi", "\x1b[31mred text\x1b[0m")
	if strings.Contains(got, "\x1b[") {
		t.Errorf("ANSI escapes survived: %q", got)
	}
	if !strings.Contains(got, "red text") {
		t.Errorf("stripping ANSI ate the text: %q", got)
	}
}
