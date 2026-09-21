package apply

import (
	"fmt"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/plan"
)

// preflight walks the whole plan before Apply touches anything and refuses
// the run on an unreadable resource, or on a replacement the caller did not
// permit. Refusing before a single call keeps a refusal cheap: nothing has
// to be rolled back.
func preflight(p *plan.Plan, allowReplace bool) error {
	var failed, blockedReplace []string

	for _, a := range p.Actions {
		switch a.Kind { //nolint:exhaustive // deliberately partial: preflight only refuses on ActionFailed and unpermitted ActionReplace, every other Kind is fine to proceed on and needs no branch here
		case plan.ActionFailed:
			failed = append(failed, describeAction(a))
		case plan.ActionReplace:
			if !allowReplace {
				blockedReplace = append(blockedReplace, describeAction(a))
			}
		}
	}

	if len(failed) == 0 && len(blockedReplace) == 0 {
		return nil
	}

	var reasons []string
	if len(failed) > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"%d resource(s) could not be read, so kraai does not know whether they exist "+
				"and cannot safely create, replace, or skip them: %s",
			len(failed), strings.Join(failed, ", ")))
	}
	if len(blockedReplace) > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"%d resource(s) require replacement (delete then create) but --replace was not "+
				"passed: %s", len(blockedReplace), strings.Join(blockedReplace, ", ")))
	}

	return kerrors.Validation("apply refused before making any changes: %s", strings.Join(reasons, "; "))
}

// describeAction names one action the way an operator would look it up in
// the manifest and in the provider's console: service and binding, then
// provider/type and the derived name.
func describeAction(a plan.Action) string {
	return fmt.Sprintf("%s.%s (%s/%s %q)", a.ServiceKey, a.Binding, a.Provider, a.Type, a.Ref.Name)
}
