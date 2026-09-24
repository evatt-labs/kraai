package assemble

import (
	"context"
	"sort"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/provider/aws"
)

// SecretEntry locates one entry of one service's secrets binding: enough
// for `kraai secret set` to derive the exact parameter name a plan would
// and refuse to write a value the manifest does not expect it to.
type SecretEntry struct {
	ServiceKey string
	Binding    string
	Entry      string
	// Name is the derived SSM parameter name (internal/naming's
	// Namer.Entry), byte for byte what internal/plan's expandEntries would
	// derive for the same manifest and environment.
	Name string
	// External is true when the entry declares `source: external`, the
	// only shape kraai secret set may write a value for. A `generate`
	// entry's value is kraai's own, produced once at create time; kraai
	// secret set refuses it rather than silently replacing a value an
	// operator never asked it to manage.
	External bool
}

// LocateSecretEntry finds binding.entry among every service's secrets
// bindings declaring provider aws-ssm, deriving its parameter name for
// environmentName. serviceKey narrows the search to one service when a
// binding name is not unique across the manifest; empty searches every
// service.
//
// Exactly one match is required: none is a validation error naming what
// was searched, and more than one is a validation error asking the caller
// to narrow with serviceKey, since kraai has no other way to tell them
// apart from the arguments `kraai secret set` takes.
func LocateSecretEntry(m *manifest.Manifest, environmentName, serviceKey, binding, entry string) (SecretEntry, error) {
	var prefix string
	if m.Environment.Naming != nil {
		prefix = m.Environment.Naming.Prefix
	}
	namer := naming.NewNamer(prefix)

	var matches []SecretEntry
	for _, svcKey := range sortedServiceKeys(m.Services) {
		if serviceKey != "" && svcKey != serviceKey {
			continue
		}
		svc := m.Services[svcKey]
		for _, b := range svc.Bindings[manifest.CapabilitySecrets] {
			if b.Name() != binding {
				continue
			}
			config := b.Config()
			if provider, _ := config["provider"].(string); provider != aws.SecretsProviderSSM {
				continue
			}
			entries, _ := config["entries"].(map[string]any)
			raw, ok := entries[entry]
			if !ok {
				continue
			}
			shape, _ := raw.(map[string]any)
			_, external := shape["source"]
			matches = append(matches, SecretEntry{
				ServiceKey: svcKey, Binding: binding, Entry: entry,
				Name:     namer.Entry(environmentName, svcKey, binding, entry),
				External: external,
			})
		}
	}

	switch len(matches) {
	case 0:
		where := "any service"
		if serviceKey != "" {
			where = "service " + serviceKey
		}
		return SecretEntry{}, kerrors.Validation(
			"no secrets binding %q with entry %q was found in %s", binding, entry, where)
	case 1:
		return matches[0], nil
	default:
		services := make([]string, 0, len(matches))
		for _, mt := range matches {
			services = append(services, mt.ServiceKey)
		}
		return SecretEntry{}, kerrors.Validation(
			"secrets binding %q with entry %q exists in more than one service (%s); "+
				"narrow the search with --service", binding, entry, strings.Join(services, ", "))
	}
}

func sortedServiceKeys(services map[string]manifest.Service) []string {
	out := make([]string, 0, len(services))
	for k := range services {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AWSSetSecret writes value to the SSM parameter located.Name, overwriting
// whatever is there under the operator's own credentials. It never creates
// a parameter that does not already exist — `kraai apply` is what creates
// one, with the entry-identity tag Get depends on — so a `secret set`
// before the first `apply` fails with a clear message rather than leaving
// behind an untagged, unmanageable parameter.
func AWSSetSecret(ctx context.Context, m *manifest.Manifest, located SecretEntry, value string) error {
	client, ok, err := awsClientFor(ctx, m)
	if err != nil {
		return err
	}
	if !ok {
		return kerrors.Validation("the manifest configures no capability with vendor aws, so there is no secret to set")
	}
	return client.SetSecretParameter(ctx, located.Name, value)
}
