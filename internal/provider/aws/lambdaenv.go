package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
	"github.com/evatt-labs/kraai/internal/secretref"
)

// resolveEnv builds the function's environment variables: settings.Env
// verbatim, plus settings.EnvSecrets resolved at the point of use — through
// client's own backends for a secret reference, through spec.Secret for a
// binding key, exactly as every envSecrets entry resolved before references
// existed. Beside a value this version-diff feature can track (a
// reference, or a binding key naming a manifest.CapabilitySecrets entry),
// it also writes that value's marker variable (see secretVersionMarkerName)
// carrying the store's own version, so a later plan can tell whether the
// value is still current without ever reading it.
//
// Resolved in sorted order by environment variable name, rather than Go's
// randomized map order, so a manifest with more than one entry behaves the
// same on every run: which one fails first, and which ones were already
// fetched before it did, is otherwise not reproducible.
func resolveEnv(ctx context.Context, client *Client, spec resource.Spec, settings LambdaSettings) (map[string]any, error) {
	env := make(map[string]any, len(settings.Env)+2*len(settings.EnvSecrets))
	for k, v := range settings.Env {
		env[k] = v
	}

	envVars := make([]string, 0, len(settings.EnvSecrets))
	for envVar := range settings.EnvSecrets {
		envVars = append(envVars, envVar)
	}
	sort.Strings(envVars)

	for _, envVar := range envVars {
		value, version, err := resolveEnvSecret(ctx, client, spec, settings.EnvSecrets[envVar])
		if err != nil {
			return nil, kerrors.Wrap(err, codeOf(err), "environment variable %q", envVar)
		}
		env[envVar] = value
		if version != "" {
			env[secretVersionMarkerName(envVar)] = version
		}
	}
	return env, nil
}

// codeOf returns err's kerrors.Code, or CodeUnexpected when it carries
// none, so adding context to a secret reference failure never downgrades a
// transient one (CodeUnexpected: throttled, network) into a validation one.
func codeOf(err error) kerrors.Code {
	var kerr *kerrors.KError
	if errors.As(err, &kerr) {
		return kerr.Code()
	}
	return kerrors.CodeUnexpected
}

// resolveEnvSecret resolves one envSecrets value to its live value and, for
// a value this version-diff feature can track, the store's own version for
// it: a reference (resolved through client, exactly as before this feature
// existed) always carries one; a binding key naming a
// manifest.CapabilitySecrets entry carries one too, fetched through a
// separate metadata-only call and, deliberately, before the value itself —
// see the ordering note below. Any other binding key (a database's
// connection_uri, e.g.) is resolved through spec.Secret exactly as every
// envSecrets entry resolved before this feature or references existed, and
// returns an empty version: out of scope, nothing to compare.
//
// The returned error, wrapped by resolveEnv with only the environment
// variable's name, keeps whatever kerrors.Code the failure already carries:
// a missing reference is a validation error, but a throttled or otherwise
// failed provider call is not, and neither this function nor its caller
// should downgrade the second into the first.
//
// Ordering for a binding key: the version is fetched before the value.
// Under a rotation racing this apply, that means the marker can only
// under-state freshness — a value written after a newer version replaced
// the one the marker names — never over-state it. An under-stated marker
// makes the next plan see it as stale and redeploy, wasteful but correct; an
// over-stated one would mark an already-stale value as current and strand
// it. A reference has no such ordering to get right: its version comes from
// the very same call that returned its value, so the two can never
// disagree.
func resolveEnvSecret(ctx context.Context, client *Client, spec resource.Spec, raw string) (value, version string, err error) {
	ref, isRef, err := secretref.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if isRef {
		return resolveVersionedURIRef(ctx, client, ref)
	}

	binding, entry, hasEntry := strings.Cut(raw, ".")
	if hasEntry {
		name, found, err := entryParameterName(spec, binding, entry)
		if err != nil {
			return "", "", err
		}
		if found {
			if version, err = client.currentSSMParameterVersion(ctx, name); err != nil {
				return "", "", err
			}
		}
	}
	value, err = spec.Secret(ctx, raw)
	return value, version, err
}

// addBindingEnv publishes what each of the service's AWS bindings resolved
// to, so the function can reach it by its binding's name: a queues binding
// JOBS becomes JOBS_QUEUE_URL and JOBS_QUEUE_ARN, an objects binding ASSETS
// becomes ASSETS_BUCKET_NAME. A binding another vendor fulfils publishes a
// credential, which envSecrets maps by hand. A variable the manifest already
// set is a conflict to report, not to resolve quietly.
func addBindingEnv(ctx context.Context, spec resource.Spec, env map[string]any) error {
	variables, err := bindingVariables(spec)
	if err != nil {
		return err
	}
	for _, v := range variables {
		if _, taken := env[v.name]; taken {
			return kerrors.Validation(
				"binding %q: environment variable %q is set by settings and by a binding; rename one",
				spec.Binding, v.name)
		}
		value, err := v.value(ctx, spec)
		if err != nil {
			return err
		}
		env[v.name] = value
	}
	return nil
}

// bindingVariable is one environment variable a binding gives the function:
// its name, known from the manifest alone, and its value, which may need
// what the binding's resource published and so is resolved only at apply.
type bindingVariable struct {
	name  string
	value func(ctx context.Context, spec resource.Spec) (any, error)
}

// bindingVariables lists the variables the service's AWS bindings give the
// function. Names come from the binding and its capability, so a plan can
// know which variables a function should carry without resolving any.
func bindingVariables(spec resource.Spec) ([]bindingVariable, error) {
	bindings, err := decodeServiceBindings(spec)
	if err != nil {
		return nil, err
	}
	var out []bindingVariable
	for _, b := range bindings {
		if b.Vendor != Provider {
			continue
		}
		prefix := b.envPrefix()
		if b.Capability == manifest.CapabilityAWS {
			variables, err := nativeVariables(spec, b, prefix)
			if err != nil {
				return nil, err
			}
			out = append(out, variables...)
			continue
		}
		switch b.Capability {
		case manifest.CapabilityQueues:
			queueKey := b.attributeKey(spec, TypeSQSQueue)
			out = append(out,
				bindingVariable{name: prefix + "_QUEUE_URL", value: func(_ context.Context, spec resource.Spec) (any, error) {
					return spec.Attribute(queueKey, "QueueUrl")
				}},
				bindingVariable{name: prefix + "_QUEUE_ARN", value: func(_ context.Context, spec resource.Spec) (any, error) {
					return spec.Attribute(queueKey, "Arn")
				}})
		case manifest.CapabilityObjects:
			// The bucket's name is its identity, derived rather than
			// published.
			out = append(out, bindingVariable{name: prefix + "_BUCKET_NAME", value: func(context.Context, resource.Spec) (any, error) {
				return b.Name, nil
			}})
		case manifest.CapabilityKeyValue:
			// The cache's endpoint exists only once created. Reachable only
			// from inside the cache's network, which vpcConfigFor places the
			// function in.
			if driver, _ := b.Config["driver"].(string); driver != DriverRedis {
				continue
			}
			cacheKey := b.attributeKey(spec, TypeElastiCacheServerlessCache)
			out = append(out, bindingVariable{name: prefix + "_REDIS_URL", value: func(_ context.Context, spec resource.Spec) (any, error) {
				return cacheURL(spec, cacheKey)
			}})
		case manifest.CapabilityDatabase:
			switch driver, _ := b.Config["driver"].(string); driver {
			case DriverDynamoDB:
				out = append(out, bindingVariable{name: prefix + "_TABLE_NAME", value: func(context.Context, resource.Spec) (any, error) {
					return b.Name, nil
				}})
			case DriverPostgres:
				if engine, _ := b.Config["engine"].(string); engine == engineAurora {
					// The cluster's URL carries its master password, so it
					// is a credential, produced by aurora.go and resolved
					// here at the moment of use.
					secretKey := b.Binding + "." + SecretConnectionURI
					out = append(out, bindingVariable{name: prefix + "_DATABASE_URL", value: func(ctx context.Context, spec resource.Spec) (any, error) {
						return spec.Secret(ctx, secretKey)
					}})
					continue
				}
				// The DSQL URL carries no password: the function signs an
				// IAM token when it connects.
				clusterKey := b.attributeKey(spec, TypeDSQLCluster)
				out = append(out, bindingVariable{name: prefix + "_DATABASE_URL", value: func(_ context.Context, spec resource.Spec) (any, error) {
					return dsqlURL(spec, clusterKey)
				}})
			}
		}
	}
	return out, nil
}

// environmentMatches reports whether the live function's environment is the
// one the spec would produce, as far as a plan can tell: every literal
// variable carries its value; every secret or binding variable exists; a
// secret-backed variable this feature tracks a version for also carries a
// marker whose value matches the store's current one (expectedSecretVersion
// — the one live call this makes, metadata only, never a decrypt); and
// nothing else is present.
func environmentMatches(ctx context.Context, client *Client, spec resource.Spec, settings LambdaSettings, state *resource.State) (bool, error) {
	environment, _ := state.Attributes["Environment"].(map[string]any)
	live, _ := environment["Variables"].(map[string]any)

	expected := make(map[string]bool, len(settings.Env)+2*len(settings.EnvSecrets))
	for name, want := range settings.Env {
		expected[name] = true
		if got, _ := live[name].(string); got != want {
			return false, nil
		}
	}
	for envVar, raw := range settings.EnvSecrets {
		expected[envVar] = true
		version, found, err := expectedSecretVersion(ctx, client, spec, raw)
		if errors.Is(err, errSecretNotYetCreated) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		marker := secretVersionMarkerName(envVar)
		expected[marker] = true
		if got, _ := live[marker].(string); got != version {
			return false, nil
		}
	}
	variables, err := bindingVariables(spec)
	if err != nil {
		return false, err
	}
	for _, v := range variables {
		expected[v.name] = true
	}
	for name := range expected {
		if _, present := live[name]; !present {
			return false, nil
		}
	}
	for name := range live {
		if !expected[name] {
			return false, nil
		}
	}
	return true, nil
}

// nativeVariables are the environment variables a native binding gives the
// function: <BINDING>_<PROPERTY> for each property the type publishes, its
// name for a type the name identifies and every property the vendor
// assigns. A string is passed as it is, a number or boolean formatted, and
// anything else as JSON.
func nativeVariables(spec resource.Spec, b serviceBinding, prefix string) ([]bindingVariable, error) {
	typeName, _ := b.Config[nativeTypeKey].(string)
	facts, err := cfschema.Lookup(typeName)
	if err != nil {
		return nil, err
	}
	attributes := b.attributeKey(spec, resource.RoleType(typeName, nativeRole))
	var out []bindingVariable
	for _, property := range publishedProperties(facts) {
		out = append(out, bindingVariable{
			name: prefix + "_" + envName(property),
			value: func(_ context.Context, spec resource.Spec) (any, error) {
				value, ok := spec.Attributes[attributes][property]
				if !ok {
					return nil, kerrors.Validation("binding %q: %s published no %s", spec.Binding, b.Binding, property)
				}
				switch v := value.(type) {
				case string:
					return v, nil
				case bool, int, int64, float64:
					return fmt.Sprint(v), nil
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding %s of %s", property, b.Binding)
				}
				return string(encoded), nil
			},
		})
	}
	return out, nil
}

// envName spells a property name as an environment variable name: a word
// break before each capital that starts a word, acronyms kept whole, so
// QueueUrl is QUEUE_URL and DBClusterArn is DB_CLUSTER_ARN.
func envName(property string) string {
	runes := []rune(property)
	var sb strings.Builder
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prevLower := unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || (unicode.IsUpper(runes[i-1]) && nextLower) {
				sb.WriteByte('_')
			}
		}
		sb.WriteRune(unicode.ToUpper(r))
	}
	return sb.String()
}
