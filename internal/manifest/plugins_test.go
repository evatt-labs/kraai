package manifest

import (
	"strings"
	"testing"
)

func validPlugin() Plugin {
	return Plugin{
		Name:     "cost-guard",
		Path:     "./plugins/cost-guard.wasm",
		Provides: []PluginProvision{{Key: "policy.cost", Export: "kraai_export_check_cost"}},
	}
}

// Everything loading a plugin actually needs is required here, so a
// half-written entry is a named error against the manifest rather than a
// module that fails to instantiate several steps later.
func TestValidatePluginsRequiresWhatLoadingNeeds(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Plugin)
		wantErr string
	}{
		{
			name:    "a plugin with no name",
			mutate:  func(p *Plugin) { p.Name = "" },
			wantErr: "name is required",
		},
		{
			name:    "a plugin with no path",
			mutate:  func(p *Plugin) { p.Path = "" },
			wantErr: "path is required",
		},
		{
			name:    "a plugin that provides nothing",
			mutate:  func(p *Plugin) { p.Provides = nil },
			wantErr: "provides is required",
		},
		{
			name:    "a provision with no key",
			mutate:  func(p *Plugin) { p.Provides[0].Key = "" },
			wantErr: "key is required",
		},
		{
			name:    "a provision with no export",
			mutate:  func(p *Plugin) { p.Provides[0].Export = "" },
			wantErr: "export is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validPlugin()
			c.mutate(&p)
			err := validatePlugins([]Plugin{p})
			if err == nil {
				t.Fatalf("accepted %+v", p)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error should mention %q: %v", c.wantErr, err)
			}
			// Every message locates the entry, since a manifest may declare
			// several and an error naming none of them is a hunt.
			if !strings.Contains(err.Error(), "plugins[0]") {
				t.Errorf("error should locate the entry: %v", err)
			}
		})
	}
}

// A name is how a plugin is referred to in its own load error, in an
// override warning and in `kraai plugins`, so two sharing one is ambiguous
// exactly where it matters.
func TestValidatePluginsRejectsADuplicateName(t *testing.T) {
	first, second := validPlugin(), validPlugin()
	second.Path = "./plugins/other.wasm"
	second.Provides[0].Key = "policy.other"

	err := validatePlugins([]Plugin{first, second})
	if err == nil {
		t.Fatal("two plugins sharing a name were accepted")
	}
	for _, want := range []string{"plugins[1]", "cost-guard", "more than once"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// One plugin naming a key twice gives no way to choose between its exports.
func TestValidatePluginsRejectsADuplicateKeyWithinOnePlugin(t *testing.T) {
	p := validPlugin()
	p.Provides = append(p.Provides, PluginProvision{
		Key: "policy.cost", Export: "kraai_export_other",
	})

	err := validatePlugins([]Plugin{p})
	if err == nil {
		t.Fatal("one plugin providing a key twice was accepted")
	}
	if !strings.Contains(err.Error(), "provided more than once") {
		t.Errorf("error should say the key repeats: %v", err)
	}
}

// Two plugins naming one key is an override, which internal/plugin's
// registry records as a warning. Rejecting it here would make a deliberate
// replacement impossible to express.
func TestValidatePluginsAllowsTheSameKeyAcrossPlugins(t *testing.T) {
	first, second := validPlugin(), validPlugin()
	second.Name = "cost-guard-override"
	second.Path = "./plugins/override.wasm"

	if err := validatePlugins([]Plugin{first, second}); err != nil {
		t.Fatalf("an override was rejected: %v", err)
	}
}

// Grants are checked by internal/plugin's Host, which owns the list of
// capabilities that exist. Keeping a second copy here would be free to
// drift from the one that actually decides.
func TestValidatePluginsDoesNotJudgeGrants(t *testing.T) {
	p := validPlugin()
	p.Grants = []string{"something_the_host_may_or_may_not_offer"}

	if err := validatePlugins([]Plugin{p}); err != nil {
		t.Fatalf("validation second-guessed a grant: %v", err)
	}
}

// No plugins at all is the overwhelmingly common manifest.
func TestValidatePluginsAcceptsNone(t *testing.T) {
	if err := validatePlugins(nil); err != nil {
		t.Fatalf("a manifest declaring no plugins was rejected: %v", err)
	}
}
