package tools

import (
	"fmt"
	"sort"
)

// ToolOrderRest is the reserved marker in a configured tool order naming
// where every tool NOT explicitly listed gets inserted, alphabetically —
// the same position and ordering the generated Tools overview already
// uses when no order is configured at all. A configured order must
// contain this marker exactly once; there is no implicit "everything
// else goes last" behavior, so a deployment can't accidentally bury
// unlisted tools without saying so.
const ToolOrderRest = "<unlisted-tools>"

// ValidateToolOrder checks a configured order's shape — no duplicate
// entries, exactly one ToolOrderRest marker — without needing a live
// Registry. It deliberately does NOT check that listed names correspond
// to real registered tools: only the registry that will actually render
// with this order knows its full tool universe, so that check happens
// separately, once, when a Registry using this order is constructed (see
// Registry's own tool-order validation) — failing there instead of here
// is what makes an unregistered name fail loudly instead of just
// vanishing from the overview.
func ValidateToolOrder(order []string) error {
	if order == nil {
		return nil
	}
	seen := make(map[string]bool, len(order))
	hasRest := false
	for _, name := range order {
		if seen[name] {
			return fmt.Errorf("tool order lists %q more than once", name)
		}
		seen[name] = true
		if name == ToolOrderRest {
			hasRest = true
		}
	}
	if !hasRest {
		return fmt.Errorf("tool order must contain the %q rest entry (marks where unlisted tools are inserted)", ToolOrderRest)
	}
	return nil
}

// UnknownToolOrderNames returns every name in order (other than the rest
// marker) that isn't present in known — the "typo in config" case
// ValidateToolOrder alone can't catch, since it has no registry to check
// against. Called once at Registry construction against the full
// registered-tool universe (every tool that exists, not just the ones
// currently visible — a name valid at startup but disabled or denied for
// one particular assembly must not become a startup error just because
// this workspace's policy currently hides it).
func UnknownToolOrderNames(order []string, known map[string]bool) []string {
	var unknown []string
	for _, name := range order {
		if name == ToolOrderRest {
			continue
		}
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	return unknown
}

// OrderToolNames arranges visible (already alphabetically sorted by the
// caller — see Registry.VisibleTools) according to order: names listed
// explicitly appear in that order, and every other visible name is
// inserted, still alphabetical, at the ToolOrderRest position. order ==
// nil returns visible unchanged — today's plain alphabetical behavior,
// byte-for-byte. A listed name that isn't currently visible (disabled,
// or hidden by permission policy for this assembly) is simply absent
// from the result, the same as it would be from an unordered overview —
// this function only arranges what VisibleTools() already decided to
// show, never adds to it.
func OrderToolNames(visible []string, order []string) []string {
	if order == nil {
		return visible
	}
	visibleSet := make(map[string]bool, len(visible))
	for _, name := range visible {
		visibleSet[name] = true
	}
	listed := make(map[string]bool, len(order))
	for _, name := range order {
		if name != ToolOrderRest {
			listed[name] = true
		}
	}
	rest := make([]string, 0, len(visible))
	for _, name := range visible {
		if !listed[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)

	out := make([]string, 0, len(visible))
	for _, name := range order {
		if name == ToolOrderRest {
			out = append(out, rest...)
			continue
		}
		if visibleSet[name] {
			out = append(out, name)
		}
	}
	return out
}
