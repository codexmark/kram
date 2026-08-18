package permission

// Evaluator resolves a Decision for a tool call against a fixed rule set —
// policy rules plus any persisted "always" grants, evaluated together as
// one list. It's immutable once built: a grant earned mid-run takes effect
// on the *next* run (Registry is rebuilt per daemon process, not per turn),
// which avoids the same prompt-cache-hostile problem Kram's memory
// snapshotting already solved once — see DECISIONS.md.
type Evaluator struct {
	rules []Rule
	def   Decision
}

// NewEvaluator builds an Evaluator from a policy file and any grants
// earned via a prior "always" approval. Grants are appended after the
// policy's own rules: at equal specificity, later rules win the tie (see
// Evaluate), so a grant naturally overrides a same-specificity "ask" rule
// from the policy — approving something "always" is supposed to stop it
// asking again.
func NewEvaluator(policy PolicyFile, grants []Rule) *Evaluator {
	def := policy.Default
	if !def.valid() {
		def = Allow
	}
	rules := make([]Rule, 0, len(policy.Rules)+len(grants))
	rules = append(rules, policy.Rules...)
	rules = append(rules, grants...)
	return &Evaluator{rules: rules, def: def}
}

// Evaluate returns the decision for one call to tool, whose policy subject
// (the command, path, or raw args — see policySubject) is subject.
func (e *Evaluator) Evaluate(tool, subject string) Decision {
	decision := e.def
	best := -1
	for _, r := range e.rules {
		if !r.matchesTool(tool) || !r.matchesSubject(subject) {
			continue
		}
		sp := r.specificity()
		if sp >= best {
			best = sp
			decision = r.Decision
		}
	}
	return decision
}

// FullyDenied reports whether every rule mentioning tool says Deny (and
// there's at least one, or the compatibility default itself is Deny) —
// meaning no call to this tool could ever be anything but denied. A tool in
// that state is removed from what the model is offered entirely (see
// Registry.Definitions), the same treatment a disabled tool gets: a
// capability the model can never use shouldn't cost tokens in the schema.
// A *partial* deny (some patterns denied, others allowed/asked) leaves the
// tool visible, since it's still sometimes usable.
func (e *Evaluator) FullyDenied(tool string) bool {
	matched := false
	for _, r := range e.rules {
		if !r.matchesTool(tool) {
			continue
		}
		matched = true
		if r.Decision != Deny {
			return false
		}
	}
	if matched {
		return true
	}
	return e.def == Deny
}
