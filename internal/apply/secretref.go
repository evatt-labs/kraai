package apply

import (
	"context"

	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// resolveSecretRefs resolves every secret reference the plan's actions
// declare, exactly once each, before any action runs. A reference that
// fails to resolve — missing, access denied, or any other error — fails
// the whole run here, before a single mutation.
//
// internal/plan never resolves a live credential (see plan.Action.Spec's
// doc comment: "a plan never resolves a live credential"); resolving here
// instead, once, after preflight and before the first wave, keeps that
// guarantee intact while still failing a bad reference before it can reach
// Create or Update. Each reference is resolved a second time, separately,
// by whichever resource actually consumes it — the same "call twice, never
// cache" contract resource.Outputs.Secret already documents — so this pass
// is a validation read, not the one that produces the value a resource
// ends up using.
func (a *Applier) resolveSecretRefs(ctx context.Context, p *plan.Plan) error {
	seen := map[string]bool{}
	for _, act := range p.Actions {
		reg, ok := a.registry.Lookup(act.Ref.Key())
		if !ok {
			// An unregistered type fails later, in execute, with a message
			// naming the drift between the plan and the registry; this pass
			// only resolves what it can reach.
			continue
		}
		resolver, ok := reg.Resource.(resource.SecretRefResolver)
		if !ok {
			continue
		}
		refs, err := resolver.SecretRefs(act.Spec)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			key := ref.String()
			if seen[key] {
				continue
			}
			seen[key] = true

			producer, err := resolver.ResolveSecretRef(ctx, ref)
			if err != nil {
				return err
			}
			if _, err := producer(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
