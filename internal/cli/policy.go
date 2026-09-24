package cli

import (
	"context"
	"encoding/json"

	"go.yaml.in/yaml/v3"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/policy"
)

const policyFlagUsage = "a Rego policy file, or a directory of them, to evaluate beside the manifest's policies/; may be repeated"

// judge evaluates gate's policies against what p would do, none when no
// policy is loaded.
func judge(ctx context.Context, set *policy.Set, gate policy.Gate, envName string, m *manifest.Manifest, p *plan.Plan) ([]string, error) {
	if set.Empty() {
		return nil, nil
	}
	input, err := policyInput(envName, m, p)
	if err != nil {
		return nil, err
	}
	return set.Evaluate(ctx, gate, input)
}

// policyInput is what a policy judges: the document kraai plan --json
// prints, each action with the config its resource is built from, the
// manifest's providers and services, and the environment overlay. Values
// are left out: --set and values files are where credentials end up, and a
// denial message prints to a CI log.
func policyInput(envName string, m *manifest.Manifest, p *plan.Plan) (map[string]any, error) {
	var input map[string]any
	if err := roundTrip(toPlanDocument(envName, p), json.Marshal, json.Unmarshal, &input); err != nil {
		return nil, err
	}
	actions, _ := input["actions"].([]any)
	for i, a := range p.Actions {
		if i >= len(actions) {
			break
		}
		var config any
		if err := roundTrip(a.Spec.Config, json.Marshal, json.Unmarshal, &config); err != nil {
			return nil, err
		}
		actions[i].(map[string]any)["config"] = config
	}
	var providers, services, overlay any
	for _, part := range []struct {
		from any
		into *any
	}{{m.Root.Providers, &providers}, {m.Services, &services}, {m.Environment, &overlay}} {
		if err := roundTrip(part.from, yaml.Marshal, yaml.Unmarshal, part.into); err != nil {
			return nil, err
		}
	}
	input["manifest"] = map[string]any{"providers": providers, "services": services}
	input["overlay"] = overlay
	return input, nil
}

// roundTrip encodes from and decodes the result into into, leaving plain
// maps, slices and scalars a policy can read.
func roundTrip(from any, encode func(any) ([]byte, error), decode func([]byte, any) error, into any) error {
	raw, err := encode(from)
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "building the policy input")
	}
	if err := decode(raw, into); err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "building the policy input")
	}
	return nil
}
