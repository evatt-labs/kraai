package plan

import (
	"fmt"
	"strings"
)

// Render turns a Plan into plain, human-readable text.
//
// A free function rather than a method so a second renderer — JSON, a TUI —
// can be added without touching Planner or Plan, and so no caller has to
// recompute the walk to present it differently.
func Render(p *Plan) string {
	if p == nil || len(p.Actions) == 0 {
		return "no resources declared\n"
	}

	var b strings.Builder
	wave := p.Actions[0].Wave
	fmt.Fprintf(&b, "wave %d:\n", wave)
	for _, a := range p.Actions {
		if a.Wave != wave {
			wave = a.Wave
			fmt.Fprintf(&b, "\nwave %d:\n", wave)
		}
		fmt.Fprintf(&b, "  %s %s (%s/%s, %s.%s)\n", symbol(a.Kind), describe(a), a.Provider, a.Type, a.ServiceKey, a.Binding)
	}
	return b.String()
}

// symbol is the one-character marker Render prefixes each line with.
func symbol(k ActionKind) string {
	switch k {
	case ActionCreate:
		return "+"
	case ActionReplace:
		return "~"
	case ActionFailed:
		return "!"
	case ActionNoChange:
		return "="
	default:
		return "?"
	}
}

// describe is the human-readable outcome Render prints for one Action.
func describe(a Action) string {
	switch a.Kind {
	case ActionCreate:
		return fmt.Sprintf("create %q", a.Ref.Name)
	case ActionNoChange:
		return fmt.Sprintf("%q unchanged", a.Ref.Name)
	case ActionReplace:
		return fmt.Sprintf("replace %q (immutable field differs)", a.Ref.Name)
	case ActionFailed:
		return fmt.Sprintf("could not read %q: %v", a.Ref.Name, a.Err)
	default:
		return fmt.Sprintf("%q: unknown action %s", a.Ref.Name, a.Kind)
	}
}
