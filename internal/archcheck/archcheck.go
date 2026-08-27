// Package archcheck enforces Kram's architectural layer boundaries — the
// project's single most valuable and hardest-to-retrofit property — so a
// stray import can't silently breach a layer in a future PR and still
// compile and pass every other test.
//
// The boundaries it locks (all verified to hold when this was written):
//
//   - internal/cli/** never imports internal/daemon/**, internal/gateway,
//     or internal/router. The CLI talks to the daemon and gateway only over
//     HTTP, via the client packages under internal/cli — never by reaching
//     into server-side internals.
//   - internal/daemon/** never imports internal/gateway. The daemon reaches
//     the gateway only over HTTP, via internal/daemon/gatewayclient — the
//     gateway's own server/router internals stay off-limits.
//
// The rule-evaluation logic (Analyze) is a pure function, unit-tested with
// synthetic import graphs; Load feeds it the real graph via `go list`,
// swept across every platform Kram ships (so a breach hidden behind an
// OS build tag can't slip past a single-OS CI runner — see buildTargets),
// and the package's own integration test asserts the real module has zero
// violations. See DECISIONS.md.
package archcheck

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Module is Kram's module path — the prefix stripped to turn a full import
// path into the module-relative path the rules are written against.
const Module = "github.com/codexmark/kram"

// Rule forbids packages under From from importing packages under To. From,
// To, and each Except entry are module-relative package-path prefixes
// (e.g. "internal/cli"), matched on whole path segments: "internal/gateway"
// matches "internal/gateway" and "internal/gateway/x" but NOT
// "internal/gatewayclient" or "internal/gatewayconfig".
type Rule struct {
	From   string
	To     string
	Except []string // module-relative prefixes under To that are allowed anyway
	Why    string   // printed when the rule is violated, so the failure explains itself
}

// Violation is one forbidden edge: Package (full import path) imports
// Import (full import path) in breach of Rule.
type Violation struct {
	Package string
	Import  string
	Rule    Rule
}

func (v Violation) String() string {
	rel := func(p string) string { return strings.TrimPrefix(p, Module+"/") }
	return fmt.Sprintf("%s must not import %s (%s)", rel(v.Package), rel(v.Import), v.Rule.Why)
}

// PackageImports is one package's full import path and the full import
// paths it directly depends on (production and test imports both — a test
// file breaching a layer is still a breach).
type PackageImports struct {
	Path    string
	Imports []string
}

// DefaultRules is Kram's layer policy — the single source of truth shared
// by the integration test and any external runner.
func DefaultRules() []Rule {
	return []Rule{
		{From: "internal/cli", To: "internal/daemon", Why: "the CLI must reach the daemon only over HTTP via internal/cli/daemonclient, never by importing daemon internals"},
		{From: "internal/cli", To: "internal/gateway", Why: "the CLI must not import gateway server internals"},
		{From: "internal/cli", To: "internal/router", Why: "the CLI must not import the router directly"},
		{From: "internal/daemon", To: "internal/gateway", Why: "the daemon must reach the gateway only over HTTP via internal/daemon/gatewayclient, never by importing gateway internals"},
	}
}

// Analyze returns every rule violation in pkgs, deterministically ordered
// (by package, then import, then rule). It's pure: no I/O, no globals — the
// import graph is entirely supplied by the caller.
func Analyze(pkgs []PackageImports, rules []Rule) []Violation {
	var violations []Violation
	for _, pkg := range pkgs {
		relPkg := rel(pkg.Path)
		for _, imp := range pkg.Imports {
			relImp := rel(imp)
			for _, r := range rules {
				if !under(relPkg, r.From) {
					continue
				}
				if !under(relImp, r.To) {
					continue
				}
				if anyUnder(relImp, r.Except) {
					continue
				}
				violations = append(violations, Violation{Package: pkg.Path, Import: imp, Rule: r})
			}
		}
	}
	return violations
}

// rel turns a full import path into its module-relative form; a path
// outside the module (stdlib, third-party) is returned unchanged, which
// never matches an "internal/..." prefix.
func rel(importPath string) string {
	return strings.TrimPrefix(importPath, Module+"/")
}

// under reports whether the module-relative path p sits under prefix,
// matching only on whole path segments so "internal/gateway" does not
// match "internal/gatewayclient".
func under(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

func anyUnder(p string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if under(p, prefix) {
			return true
		}
	}
	return false
}

// buildTargets are the GOOS/GOARCH pairs Kram ships (see scripts/verify.sh's
// cross-builds). Load unions the import sets across all of them because
// `go list` on a single platform only sees the files selected for that
// platform — so a cross-layer import hidden behind a build tag (e.g. a
// *_windows.go file, or //go:build windows) would slip past a check run on
// a single-OS CI runner. All use CGO_ENABLED=0, matching how Kram is built
// (it is deliberately cgo-free — see internal/daemon/store), so this is the
// same file selection the shipped binaries see on each platform.
var buildTargets = []struct{ goos, goarch string }{
	{"linux", "amd64"},
	{"darwin", "amd64"},
	{"windows", "amd64"},
	{"android", "arm64"},
}

// Load builds the real import graph for pattern (e.g. Module+"/...") by
// shelling out to `go list` once per build target and unioning the results.
// It collects each package's production and test imports, since a boundary
// breach in a _test.go file is still a breach worth catching.
func Load(ctx context.Context, pattern string) ([]PackageImports, error) {
	merged := make(map[string]map[string]bool) // package path -> set of import paths
	var order []string                         // first-seen package order, for deterministic output

	for _, t := range buildTargets {
		pkgs, err := listForTarget(ctx, t.goos, t.goarch, pattern)
		if err != nil {
			return nil, fmt.Errorf("go list for %s/%s: %w", t.goos, t.goarch, err)
		}
		for _, pkg := range pkgs {
			set, ok := merged[pkg.Path]
			if !ok {
				set = make(map[string]bool)
				merged[pkg.Path] = set
				order = append(order, pkg.Path)
			}
			for _, imp := range pkg.Imports {
				set[imp] = true
			}
		}
	}

	out := make([]PackageImports, 0, len(order))
	for _, path := range order {
		imports := make([]string, 0, len(merged[path]))
		for imp := range merged[path] {
			imports = append(imports, imp)
		}
		sort.Strings(imports)
		out = append(out, PackageImports{Path: path, Imports: imports})
	}
	return out, nil
}

// listForTarget runs `go list` for one GOOS/GOARCH, returning each matched
// package's union of production, test, and external-test imports.
func listForTarget(ctx context.Context, goos, goarch, pattern string) ([]PackageImports, error) {
	// Tab-separated so the parse is unambiguous even though import paths
	// never contain tabs: path \t prod-imports \t test-imports \t
	// external-test-imports, each import list space-joined.
	const format = `{{.ImportPath}}` + "\t" +
		`{{join .Imports " "}}` + "\t" +
		`{{join .TestImports " "}}` + "\t" +
		`{{join .XTestImports " "}}`
	cmd := exec.CommandContext(ctx, "go", "list", "-f", format, pattern)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("go list failed: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("go list failed: %w", err)
	}

	var pkgs []PackageImports
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Split into path + up to three import groups (prod, test, xtest).
		// Trailing empty groups can be dropped by whitespace trimming, so
		// accept any field count >= 1 rather than demanding exactly four —
		// fields[1:] is simply "every import group that survived".
		fields := strings.Split(line, "\t")
		var imports []string
		for _, group := range fields[1:] {
			imports = append(imports, strings.Fields(group)...)
		}
		pkgs = append(pkgs, PackageImports{Path: fields[0], Imports: imports})
	}
	return pkgs, nil
}
