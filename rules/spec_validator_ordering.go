//go:build ignore

package rules

import "github.com/quasilyte/go-ruleguard/dsl"

// specValidatorAfterEarlyReturn flags a SpecValidator type assertion that
// appears after an "if $x == nil { ...; return ... }" guard in the same
// block.
//
// This is the shape AGENTS.md records as having shipped once already:
// internal/plan's decide used to reach SpecValidator only through the same
// branch as Differ, which runs after Get and after the early return for
// "resource does not exist yet." On a brand-new environment, where every
// action is ActionCreate, that early return fired before the validator
// ever ran — a manifest with an invalid spec planned clean. SpecValidator
// is declared in internal/plan (validate.go), not internal/resource: the
// brief that asked for this rule named internal/resource.SpecValidator,
// but that interface has no such package-qualified spelling in this
// codebase, only the unqualified plan.SpecValidator this rule matches.
//
// decide's current shape asserts SpecValidator first, unconditionally,
// before Get runs at all — so this rule does not fire against it today.
// It exists to catch a regression that reintroduces the old ordering, or
// a second decide-like function that copies it.
//
// Narrow by construction: it only catches a SpecValidator assertion that
// is textually preceded, in the same statement list, by a nil-guard with a
// return — not, for example, a SpecValidator assertion inside a nested
// helper the guard calls out to, or one gated by a condition spelled some
// other way than "$x == nil". Differ is deliberately not included: unlike
// SpecValidator, Differ's own contract requires live state to compare
// against, so running it only after Get has resolved something is
// correct, not a bug.
func specValidatorAfterEarlyReturn(m dsl.Matcher) {
	m.Match(`
		if $state == nil {
			$*_
			return $*_
		}
		$*_
		if $_, $ok := $x.(SpecValidator); $ok {
			$*_
		}
	`).Report(`SpecValidator is asserted after an "if $state == nil { ...; return }" guard — on a fresh environment, where Get always returns a nil state, this validator would never run. ValidateSpec must run unconditionally, before the existence check, exactly as internal/plan's decide now does — see SpecValidator's own doc comment (internal/plan/validate.go) for the bug this guards against.`)
}
