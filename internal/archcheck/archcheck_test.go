package archcheck

import (
	"context"
	"strings"
	"testing"
)

func p(path string, imports ...string) PackageImports {
	return PackageImports{Path: Module + "/" + path, Imports: prefix(imports)}
}

func prefix(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		// Leave already-external paths (no slash-free stdlib like "fmt", or
		// third-party) unqualified; qualify bare internal-looking ones.
		if strings.HasPrefix(p, "internal/") {
			out[i] = Module + "/" + p
		} else {
			out[i] = p
		}
	}
	return out
}

func TestAnalyzeFlagsForbiddenEdge(t *testing.T) {
	pkgs := []PackageImports{
		p("internal/cli/app", "internal/daemon/store", "fmt"),
	}
	got := Analyze(pkgs, DefaultRules())
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 violation, got %d: %v", len(got), got)
	}
	if got[0].Package != Module+"/internal/cli/app" || got[0].Import != Module+"/internal/daemon/store" {
		t.Errorf("violation = %+v, want cli/app -> daemon/store", got[0])
	}
}

func TestAnalyzeCleanGraphHasNoViolations(t *testing.T) {
	pkgs := []PackageImports{
		p("internal/cli/app", "internal/cli/daemonclient", "internal/config", "fmt"),
		p("internal/daemon/server", "internal/daemon/gatewayclient", "internal/config"),
		p("internal/gateway", "internal/router", "internal/provider"),
	}
	if got := Analyze(pkgs, DefaultRules()); len(got) != 0 {
		t.Fatalf("expected no violations, got %v", got)
	}
}

// TestAnalyzeSegmentMatchingAvoidsPrefixFalsePositives is the trap the
// naive "does the import string contain 'internal/gateway'" check falls
// into: internal/gatewayclient and internal/gatewayconfig share a string
// prefix with internal/gateway but are entirely different packages that
// must NOT be flagged. Likewise a rule on internal/cli must not fire on a
// hypothetical internal/climate.
func TestAnalyzeSegmentMatchingAvoidsPrefixFalsePositives(t *testing.T) {
	pkgs := []PackageImports{
		// daemon importing gatewayCLIENT and gatewayCONFIG is fine.
		p("internal/daemon/agent", "internal/daemon/gatewayclient", "internal/gatewayconfig"),
		// a package whose name merely starts with "internal/cli" letters.
		p("internal/climate", "internal/daemon/store"),
	}
	if got := Analyze(pkgs, DefaultRules()); len(got) != 0 {
		t.Fatalf("segment matching should not flag prefix-only lookalikes, got %v", got)
	}
}

// TestAnalyzeCatchesTheExactGatewayImport is the positive counterpart: an
// actual internal/gateway (or internal/gateway/x) import from the daemon
// IS a violation.
func TestAnalyzeCatchesTheExactGatewayImport(t *testing.T) {
	pkgs := []PackageImports{
		p("internal/daemon/server", "internal/gateway"),
		p("internal/daemon/agent", "internal/router/strategy"),
	}
	got := Analyze(pkgs, DefaultRules())
	if len(got) != 1 {
		t.Fatalf("expected 1 violation (daemon->gateway; daemon->router is not a rule), got %d: %v", len(got), got)
	}
	if got[0].Import != Module+"/internal/gateway" {
		t.Errorf("violation import = %q, want internal/gateway", got[0].Import)
	}
}

func TestAnalyzeExceptAllowsListedPrefix(t *testing.T) {
	rules := []Rule{{From: "internal/cli", To: "internal/daemon", Except: []string{"internal/daemon/gatewayclient"}}}
	pkgs := []PackageImports{
		p("internal/cli/app", "internal/daemon/gatewayclient"), // allowed by Except
		p("internal/cli/app", "internal/daemon/store"),         // still forbidden
	}
	got := Analyze(pkgs, rules)
	if len(got) != 1 || got[0].Import != Module+"/internal/daemon/store" {
		t.Fatalf("Except should permit only the listed prefix, got %v", got)
	}
}

func TestViolationString(t *testing.T) {
	v := Violation{
		Package: Module + "/internal/cli/app",
		Import:  Module + "/internal/daemon/store",
		Rule:    Rule{From: "internal/cli", To: "internal/daemon", Why: "over HTTP only"},
	}
	got := v.String()
	// Module-relative on both sides, and the Why is included so a failure
	// explains itself without the reader consulting the rule table.
	if !strings.Contains(got, "internal/cli/app must not import internal/daemon/store") {
		t.Errorf("String() = %q, missing the relative package/import phrasing", got)
	}
	if !strings.Contains(got, "over HTTP only") {
		t.Errorf("String() = %q, should include the rule's Why", got)
	}
	if strings.Contains(got, Module) {
		t.Errorf("String() = %q, should not contain the full module prefix", got)
	}
}

// TestLoadErrorsOnBadPattern covers Load's failure branch: a pattern that
// matches nothing makes `go list` exit non-zero, and Load must surface that
// as an error rather than silently returning an empty (vacuously-clean)
// graph — otherwise a broken invocation would look like "no violations".
func TestLoadErrorsOnBadPattern(t *testing.T) {
	_, err := Load(context.Background(), Module+"/internal/zzz_does_not_exist")
	if err == nil {
		t.Fatal("expected an error for a pattern that matches no package")
	}
}

// TestKramLayeringHolds is the enforcement itself: the real module's import
// graph must satisfy every DefaultRule. This is what turns the boundary
// from a convention into a gate — a future PR that breaches a layer fails
// here. It shells out to `go list`, so it needs the go toolchain (always
// present under `go test`).
func TestKramLayeringHolds(t *testing.T) {
	pkgs, err := Load(context.Background(), Module+"/...")
	if err != nil {
		t.Fatalf("loading import graph: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("go list returned no packages — the check would vacuously pass")
	}
	violations := Analyze(pkgs, DefaultRules())
	if len(violations) > 0 {
		var b strings.Builder
		b.WriteString("architectural layer boundary violation(s):\n")
		for _, v := range violations {
			b.WriteString("  - ")
			b.WriteString(v.String())
			b.WriteString("\n")
		}
		t.Fatal(b.String())
	}
}
