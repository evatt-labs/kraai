package plan

import (
	"context"
	"sort"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

// secretsFixture is a minimal registry with one NameFromEntries
// registration under manifest.CapabilitySecrets, the shape
// internal/provider/aws's secrets capability actually registers.
type secretsFixture struct {
	reg    *resource.Registry
	secret *fakeResource
}

func newSecretsFixture(t *testing.T) *secretsFixture {
	t.Helper()
	f := &secretsFixture{reg: resource.NewRegistry(), secret: newFakeResource()}
	err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "secret", Capability: manifest.CapabilitySecrets,
		NameFrom: resource.NameFromEntries, NameKey: "entries",
		Lookup: resource.LookupByName, Resource: f.secret,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return f
}

func (f *secretsFixture) manifest(entries map[string]any) *manifest.Manifest {
	return &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilitySecrets: {Vendor: "aws"},
		}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{
				manifest.CapabilitySecrets: {{
					"binding": "SECRETS", "provider": "aws-ssm", "entries": entries,
				}},
			}},
		},
	}
}

func findActions(p *Plan, provider, typ string) []Action {
	var out []Action
	for _, a := range p.Actions {
		if a.Provider == provider && a.Type == typ {
			out = append(out, a)
		}
	}
	return out
}

// TestExpandEntries_OneItemPerEntry is the acceptance criterion for
// NameFromEntries: a single binding whose `entries:` map names two secrets
// expands to two resources, not one, each with its own derived name and its
// own entry's config, but sharing the binding's identity for everything
// apply's indexes key on.
func TestExpandEntries_OneItemPerEntry(t *testing.T) {
	f := newSecretsFixture(t)
	m := f.manifest(map[string]any{
		"pepper_key":           map[string]any{"generate": map[string]any{"bytes": 32, "encoding": "base64"}},
		"github_client_secret": map[string]any{"source": "external"},
	})

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	actions := findActions(p, "aws", "secret")
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2 (one per entry): %+v", len(actions), actions)
	}

	names := map[string]Action{}
	for _, a := range actions {
		names[a.Ref.Name] = a
		if a.ServiceKey != "api" || a.Binding != "SECRETS" || a.Capability != manifest.CapabilitySecrets {
			t.Errorf("action %+v does not carry the shared binding identity", a.Item)
		}
		if !reflectContains(a.ReadsBindings, "SECRETS") {
			t.Errorf("action %+v ReadsBindings = %v, want it to include the binding itself", a.Item, a.ReadsBindings)
		}
	}

	namer := naming.NewNamer("")
	wantPepper := namer.Entry(envName, "api", "SECRETS", "pepper_key")
	wantGitHub := namer.Entry(envName, "api", "SECRETS", "github_client_secret")

	pepper, ok := names[wantPepper]
	if !ok {
		t.Fatalf("no action named %q; got names %v", wantPepper, keysOf(names))
	}
	if entry, _ := pepper.Spec.Config["entry"].(string); entry != "pepper_key" {
		t.Errorf("pepper_key action's Spec.Config[entry] = %v, want %q", pepper.Spec.Config["entry"], "pepper_key")
	}
	if _, ok := pepper.Spec.Config["generate"]; !ok {
		t.Errorf("pepper_key action lost its own entry's config: %+v", pepper.Spec.Config)
	}

	github, ok := names[wantGitHub]
	if !ok {
		t.Fatalf("no action named %q; got names %v", wantGitHub, keysOf(names))
	}
	if source, _ := github.Spec.Config["source"].(string); source != "external" {
		t.Errorf("github_client_secret action's Spec.Config[source] = %v, want %q", github.Spec.Config["source"], "external")
	}

	if wantPepper == wantGitHub {
		t.Fatalf("two entries derived the same name %q", wantPepper)
	}
}

func reflectContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func keysOf(m map[string]Action) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestExpandEntries_MalformedEntriesIsAnError covers what expandEntries
// must refuse rather than silently plan as zero resources: a missing
// `entries` key, one that is not a map, and one that is empty.
func TestExpandEntries_MalformedEntriesIsAnError(t *testing.T) {
	f := newSecretsFixture(t)

	for _, c := range []struct {
		name    string
		entries any
	}{
		{"missing", nil},
		{"not a map", "oops"},
		{"empty map", map[string]any{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &manifest.Manifest{
				Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilitySecrets: {Vendor: "aws"}}},
				Services: map[string]manifest.Service{
					"api": {Bindings: manifest.Bindings{
						manifest.CapabilitySecrets: {{"binding": "SECRETS", "provider": "aws-ssm"}},
					}},
				},
			}
			if c.name != "missing" {
				m.Services["api"].Bindings[manifest.CapabilitySecrets][0]["entries"] = c.entries
			}
			_, err := New(f.reg).Plan(context.Background(), m, envName)
			if err == nil {
				t.Fatalf("Plan succeeded with %s entries, want an error", c.name)
			}
		})
	}
}

// TestExpandEntries_AdoptedImportIsRefused documents a real limitation
// rather than an undefined one: a resource.Import identifies exactly one
// resource, and a NameFromEntries binding expands to an author-chosen
// number of them, so an environment's `resources:` block cannot adopt a
// secrets binding as a whole. Out of scope per evatt-labs/kraai#331; this
// pins that the attempt fails loudly instead of adopting one entry
// arbitrarily or silently ignoring the import.
func TestExpandEntries_AdoptedImportIsRefused(t *testing.T) {
	f := newSecretsFixture(t)
	m := f.manifest(map[string]any{
		"pepper_key": map[string]any{"source": "external"},
	})
	m.Environment.Resources = map[string]manifest.ResourceImports{
		"api": {manifest.CapabilitySecrets: {"SECRETS": manifest.ImportRef{Name: "whichever"}}},
	}

	_, err := New(f.reg).Plan(context.Background(), m, envName)
	if err == nil {
		t.Fatal("Plan succeeded adopting a secrets binding, want an error")
	}
}

// TestServiceBindings_SecretsCarriesEntryNames is the third of the three
// callers that must derive an entry's parameter name identically (see
// internal/naming's Namer.Entry doc and internal/provider/aws/iamrole.go's
// bindingStatements): a compute registration's Spec.Config["bindings"]
// entry for a secrets binding carries entryNames, the per-entry derived
// names an execution role's grant scopes itself to, computed once here with
// the same namer expandEntries used for the resources themselves.
func TestServiceBindings_SecretsCarriesEntryNames(t *testing.T) {
	f := newSecretsFixture(t)
	compute := newFakeResource()
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::Lambda::Function", Capability: manifest.CapabilityCompute,
		Lookup: resource.LookupByName, Resource: compute, Reads: resource.ReadsServiceBindings,
	}); err != nil {
		t.Fatal(err)
	}

	m := f.manifest(map[string]any{
		"pepper_key": map[string]any{"source": "external"},
	})
	m.Root.Providers[manifest.CapabilityCompute] = &manifest.Provider{Vendor: "aws"}

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	fn := findAction(t, p, "aws", "AWS::Lambda::Function")
	bindings, _ := fn.Spec.Config["bindings"].([]any)
	var secretsDesc map[string]any
	for _, b := range bindings {
		desc, _ := b.(map[string]any)
		if desc["capability"] == manifest.CapabilitySecrets {
			secretsDesc = desc
		}
	}
	if secretsDesc == nil {
		t.Fatalf("no secrets binding in compute's Spec.Config[bindings]: %+v", bindings)
	}

	entryNames, _ := secretsDesc["entryNames"].(map[string]string)
	want := naming.NewNamer("").Entry(envName, "api", "SECRETS", "pepper_key")
	if got := entryNames["pepper_key"]; got != want {
		t.Errorf("entryNames[pepper_key] = %q, want %q (entryNames: %+v)", got, want, entryNames)
	}
}
