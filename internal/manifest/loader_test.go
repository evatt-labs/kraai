package manifest_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// testVocabulary is a real resource.Catalog built from hand-written
// declarations, standing in for the one internal/assemble builds from the
// provider packages. Real rather than a fake that accepts everything,
// because what these tests have to prove is that a manifest is held to the
// vendor's declared shape — a stub returning nil would let every binding
// entry through and still pass.
//
// It declares every capability testdata/ names and nothing else, and carries
// a binding schema for the vendors testdata/ configures, so a manifest
// naming an undeclared capability or writing a mis-shaped entry fails here
// the way it would in production. No provider package is imported.
func testVocabulary(t *testing.T) manifest.Vocabulary {
	t.Helper()

	// Mirrors internal/provider/cfresource's and neonresource's own
	// databaseBindingSchema: a driver, and a caching block closed to
	// anything it does not name.
	databaseBinding := resource.NewSchema("database binding", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"binding": map[string]any{"type": "string"},
			"driver":  map[string]any{"type": "string"},
			"caching": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"disabled": map[string]any{"type": "boolean"},
					"maxAge":   map[string]any{"type": "integer"},
				},
				"additionalProperties": false,
			},
		},
		"required":             []any{"binding"},
		"additionalProperties": false,
	})
	nameOnly := resource.NewSchema("binding", map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"binding": map[string]any{"type": "string"}},
		"required":             []any{"binding"},
		"additionalProperties": false,
	})

	queuesBinding := resource.NewSchema("queues binding", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"binding":  map[string]any{"type": "string"},
			"consumer": map[string]any{"type": "boolean"},
		},
		"required":             []any{"binding"},
		"additionalProperties": false,
	})

	defs := []resource.CapabilityDef{
		{Name: manifest.CapabilityCompute, Summary: "a service's own deployable unit"},
		{Name: manifest.CapabilityDatabase, Summary: "a database", Binding: databaseBinding},
		{Name: manifest.CapabilityKeyValue, Summary: "a key-value store", Binding: nameOnly},
		{Name: manifest.CapabilityNetwork, Summary: "a private network"},
		{Name: manifest.CapabilityObjects, Summary: "an object store", Binding: nameOnly},
		{Name: manifest.CapabilityQueues, Summary: "a queue", Binding: queuesBinding},
	}

	// Every vendor testdata/ names declares the same set, so a fixture can
	// configure any of them without this table growing a per-vendor case.
	var providers []resource.Provider
	for _, vendor := range []string{"aws", "cloudflare", "neon", "default-compute", "from-values", "from-cli"} {
		providers = append(providers, resource.FuncProvider{
			ProviderName:     vendor,
			CapabilitiesFunc: func() []resource.CapabilityDef { return defs },
		})
	}

	catalog, err := resource.NewCatalog(providers...)
	if err != nil {
		t.Fatalf("building the test catalog: %v", err)
	}
	return catalog
}

func newLoader(t *testing.T, fsys manifest.FS, engine manifest.TemplateEngine) *manifest.Loader {
	t.Helper()
	return manifest.NewLoader(fsys, engine, testVocabulary(t))
}

// configuredProvider returns the provider a manifest configured for a
// capability, failing the test if it configured none.
func configuredProvider(t *testing.T, p manifest.Providers, capability string) *manifest.Provider {
	t.Helper()
	configured, ok := p.For(capability)
	if !ok {
		t.Fatalf("providers.%s is not configured", capability)
	}
	return configured
}

func vendorOf(t *testing.T, p manifest.Providers, capability string) string {
	t.Helper()
	return configuredProvider(t, p, capability).Vendor
}

func newRealLoader(t *testing.T, root string) *manifest.Loader {
	t.Helper()
	fsys := mustNewFS(t, root)
	return newLoader(t, fsys, manifest.NewTemplateEngine(fsys))
}

// TestLoad_BlueprintExamplesParse pins that the canonical kraai.yaml /
// services / environments manifest examples, copied verbatim into
// testdata/blueprint/, parse into a fully resolved Manifest.
func TestLoad_BlueprintExamplesParse(t *testing.T) {
	loader := newRealLoader(t, "testdata/blueprint")

	got, err := loader.Load("prod", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Root.Version != 1 {
		t.Errorf("Root.Version = %d, want 1", got.Root.Version)
	}
	if vendor := vendorOf(t, got.Root.Providers, manifest.CapabilityCompute); vendor != "cloudflare" {
		t.Errorf("providers.compute vendor = %q", vendor)
	}
	database := configuredProvider(t, got.Root.Providers, manifest.CapabilityDatabase)
	if database.Vendor != "neon" {
		t.Errorf("providers.database vendor = %q", database.Vendor)
	}
	// A vendor's own settings are carried through uninterpreted.
	if got := database.Settings["project"]; got != "kraai-control-plane" {
		t.Errorf("Postgres.Settings[project] = %v", got)
	}
	if len(got.Root.Plugins) != 2 {
		t.Fatalf("Root.Plugins = %+v, want 2", got.Root.Plugins)
	}
	costGuard := got.Root.Plugins[1]
	if costGuard.Name != "cost-guard" || costGuard.Path != "./plugins/cost-guard.wasm" {
		t.Errorf("Root.Plugins[1] = %+v", costGuard)
	}
	// Grants and provides are the operator's declaration, not the module's,
	// so they have to survive the load intact — see manifest.Plugin.
	if len(costGuard.Grants) != 1 || costGuard.Grants[0] != "http_fetch" {
		t.Errorf("Root.Plugins[1].Grants = %v", costGuard.Grants)
	}
	if len(costGuard.Provides) != 1 ||
		costGuard.Provides[0].Key != "policy.cost" ||
		costGuard.Provides[0].Export != "kraai_export_check_cost" {
		t.Errorf("Root.Plugins[1].Provides = %+v", costGuard.Provides)
	}

	api, ok := got.Services["api"]
	if !ok {
		t.Fatalf("services = %v, want an %q entry", got.Services, "api")
	}
	if api.Dir != "packages/api" {
		t.Errorf("api.Dir = %q", api.Dir)
	}
	// The blueprint writes `databases:`, which names the `database`
	// capability — the one manifest key whose spelling differs from the
	// capability it selects. Everything downstream sees the capability name.
	databases := api.Bindings[manifest.CapabilityDatabase]
	if len(databases) != 2 {
		t.Fatalf("api database bindings = %+v", databases)
	}
	if databases[0].Name() != "DB" || databases[0]["driver"] != "sqlite" {
		t.Errorf("database[0] = %+v", databases[0])
	}
	pg := databases[1]
	if pg.Name() != "PG" || pg["driver"] != "postgres" {
		t.Errorf("database[1] = %+v", pg)
	}
	// Entry shape past the binding name is the vendor's vocabulary, so it
	// arrives as what the YAML said rather than as a type this package owns.
	caching, ok := pg["caching"].(map[string]any)
	if !ok || caching["disabled"] != false || caching["maxAge"] != 60 {
		t.Errorf("database[1].caching = %+v", pg["caching"])
	}
	if kv := api.Bindings[manifest.CapabilityKeyValue]; len(kv) != 1 || kv[0].Name() != "CACHE" {
		t.Errorf("api keyvalue bindings = %+v", kv)
	}
	if objects := api.Bindings[manifest.CapabilityObjects]; len(objects) != 1 ||
		objects[0].Name() != "ASSETS" {
		t.Errorf("api objects bindings = %+v", api.Bindings[manifest.CapabilityObjects])
	}
	queues := api.Bindings[manifest.CapabilityQueues]
	if len(queues) != 1 || queues[0].Name() != "JOBS" || queues[0]["consumer"] != true {
		t.Errorf("api queues bindings = %+v", queues)
	}

	env := got.Environment
	if env.Kind != manifest.EnvironmentKindPersistent {
		t.Errorf("Environment.Kind = %q", env.Kind)
	}
	if !env.Protected {
		t.Errorf("Environment.Protected = false, want true")
	}
	if env.Naming == nil || env.Naming.Prefix != "" {
		t.Errorf("Environment.Naming = %+v", env.Naming)
	}
	routes, ok := env.Routes["api"]
	if !ok || len(routes) != 1 || routes[0].Pattern != "api.acme.com" || !routes[0].CustomDomain {
		t.Errorf("Environment.Routes = %+v", env.Routes)
	}
	imports, ok := env.Resources["api"]
	if !ok {
		t.Fatalf("Environment.Resources = %+v, want an %q entry", env.Resources, "api")
	}
	// Keyed by capability, so an import for a capability the fixed struct
	// never had a field for — a DNS zone, a VPC — is expressible too.
	if got := imports[manifest.CapabilityDatabase]["DB"].ID; got != "0e1f...-uuid" {
		t.Errorf("imported DB ref = %+v", imports[manifest.CapabilityDatabase])
	}

	if got.Values["region"] != "enam" || got.Values["tier"] != "production" {
		t.Errorf("Values = %v", got.Values)
	}
}

func requireCode(t *testing.T, err error, code kerrors.Code) *kerrors.KError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error")
	}
	kerr, ok := err.(*kerrors.KError) //nolint:errorlint // asserting the concrete constructor return type is the point
	if !ok {
		t.Fatalf("expected *kerrors.KError, got %T (%v)", err, err)
	}
	if kerr.Code() != code {
		t.Fatalf("expected code %v, got %v", code, kerr.Code())
	}
	return kerr
}

// TestLoad_TemplateRenderErrorFailsLoudly is the first half of acceptance
// criterion 2: a .j2 file with a template error must fail at render time,
// not silently produce broken YAML that then fails (or worse, passes)
// schema validation.
func TestLoad_TemplateRenderErrorFailsLoudly(t *testing.T) {
	loader := newRealLoader(t, "testdata/render-error")

	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "kraai.yaml.j2") {
		t.Errorf("error %q does not name the failing template", kerr.Error())
	}
}

// TestLoad_RenderedButSchemaInvalidFailsValidation is the second half of
// acceptance criterion 2: a template that renders successfully but
// produces a document with an unknown field must fail with the same
// path-based error a hand-written file would produce.
func TestLoad_RenderedButSchemaInvalidFailsValidation(t *testing.T) {
	loader := newRealLoader(t, "testdata/schema-invalid-after-render")

	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "services.api: unknown field \"bogus\"") {
		t.Errorf("error %q does not contain the expected path-based message", kerr.Error())
	}
}

// TestLoad_SetOverridesValuesOverridesTemplateDefault is acceptance
// criterion 3: --set beats the values file, which beats a template's own
// default, in that order.
func TestLoad_SetOverridesValuesOverridesTemplateDefault(t *testing.T) {
	loader := newRealLoader(t, "testdata/precedence")

	t.Run("template default when neither values nor --set supply it", func(t *testing.T) {
		got, err := loader.Load("nodev", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if vendor := vendorOf(t, got.Root.Providers, manifest.CapabilityCompute); vendor != "default-compute" {
			t.Errorf("providers.compute vendor = %q, want the template default", vendor)
		}
	})

	t.Run("values file overrides the template default", func(t *testing.T) {
		got, err := loader.Load("dev", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if vendor := vendorOf(t, got.Root.Providers, manifest.CapabilityCompute); vendor != "from-values" {
			t.Errorf("providers.compute vendor = %q, want the values-file value", vendor)
		}
	})

	t.Run("--set overrides the values file", func(t *testing.T) {
		got, err := loader.Load("dev", []string{"compute=from-cli"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if vendor := vendorOf(t, got.Root.Providers, manifest.CapabilityCompute); vendor != "from-cli" {
			t.Errorf("providers.compute vendor = %q, want the --set value", vendor)
		}
	})
}

// TestLoad_RootTemplateRendersButFailsSchema is the root-file analogue of
// TestLoad_RenderedButSchemaInvalidFailsValidation: kraai.yaml.j2 itself
// (not a services file) renders successfully but the result has an
// unknown field.
func TestLoad_RootTemplateRendersButFailsSchema(t *testing.T) {
	loader := newRealLoader(t, "testdata/root-template-schema-invalid")
	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "unknown field \"bogus\"") {
		t.Errorf("error %q does not name the unknown field", kerr.Error())
	}
}

// TestLoad_TemplatedServiceRendersSuccessfully proves a services/*.yaml.j2
// file that renders to valid, schema-conforming YAML loads normally —
// the success path alongside the render/schema failure paths covered
// elsewhere.
func TestLoad_TemplatedServiceRendersSuccessfully(t *testing.T) {
	loader := newRealLoader(t, "testdata/templated-service")
	got, err := loader.Load("dev", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	api, ok := got.Services["api"]
	if !ok || api.Dir != "packages/api" {
		t.Fatalf("services = %+v", got.Services)
	}
}

func TestLoad_MissingRootIsValidationError(t *testing.T) {
	loader := newRealLoader(t, "testdata/missing-root")
	_, err := loader.Load("dev", nil)
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestLoad_BothRootFilesIsValidationError(t *testing.T) {
	loader := newRealLoader(t, "testdata/both-root")
	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "kraai.yaml") || !strings.Contains(kerr.Error(), "kraai.yaml.j2") {
		t.Errorf("error %q does not name both root files", kerr.Error())
	}
}

func TestLoad_UnknownTopLevelKeyRejected(t *testing.T) {
	loader := newRealLoader(t, "testdata/unknown-key")
	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "unknown field \"bogus\"") {
		t.Errorf("error %q does not name the unknown field", kerr.Error())
	}
}

// A typo nested inside a binding entry is still rejected, but by the
// vendor's declared schema rather than by strict decoding: what a binding
// entry may carry is the vendor's vocabulary, and this package no longer has
// a Go struct to check `maxage` against. The error still names the entry by
// its manifest path, which is what made the strict-decode version worth
// having.
func TestLoad_UnknownNestedKeyRejectedWithPath(t *testing.T) {
	loader := newRealLoader(t, "testdata/unknown-nested-key")
	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	for _, want := range []string{"services.api.database[1]", "maxage"} {
		if !strings.Contains(kerr.Error(), want) {
			t.Errorf("error %q does not mention %q", kerr.Error(), want)
		}
	}
}

func TestLoad_DuplicateServiceAcrossFilesIsValidationError(t *testing.T) {
	loader := newRealLoader(t, "testdata/duplicate-service")
	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "api") {
		t.Errorf("error %q does not name the duplicate service", kerr.Error())
	}
}

func TestLoad_MissingEnvironmentIsValidationError(t *testing.T) {
	loader := newRealLoader(t, "testdata/missing-environment")
	_, err := loader.Load("nope", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "nope") {
		t.Errorf("error %q does not name the missing environment", kerr.Error())
	}
}

func TestLoad_BadKindIsValidationError(t *testing.T) {
	loader := newRealLoader(t, "testdata/bad-kind")
	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "kind") {
		t.Errorf("error %q does not mention kind", kerr.Error())
	}
}

// TestLoad_InvalidNamingPrefixIsValidationError is the "validate the
// prefix... enforce it at manifest load with a clear error" requirement:
// a naming.prefix missing its mandatory trailing hyphen is rejected at
// Load, with manifest.ErrInvalidPrefix identifiable in the error chain
// via errors.Is — not just a message a caller has to substring-match.
func TestLoad_InvalidNamingPrefixIsValidationError(t *testing.T) {
	loader := newRealLoader(t, "testdata/bad-naming-prefix")
	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !errors.Is(kerr, manifest.ErrInvalidPrefix) {
		t.Errorf("error %v does not wrap manifest.ErrInvalidPrefix", kerr)
	}
	if !strings.Contains(kerr.Error(), "naming.prefix") {
		t.Errorf("error %q does not mention naming.prefix", kerr.Error())
	}
}

// TestLoad_NamingPrefixTooLongIsValidationError is the "a prefix so long
// it leaves no room for the derived name is a manifest error, not a
// silent truncation to nothing" requirement: a naming.prefix over
// internal/manifest's length ceiling is rejected at Load, with
// manifest.ErrPrefixTooLong identifiable via errors.Is, rather than
// silently accepted and truncated away later inside internal/naming.
func TestLoad_NamingPrefixTooLongIsValidationError(t *testing.T) {
	loader := newRealLoader(t, "testdata/naming-prefix-too-long")
	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !errors.Is(kerr, manifest.ErrPrefixTooLong) {
		t.Errorf("error %v does not wrap manifest.ErrPrefixTooLong", kerr)
	}
}

func TestLoad_WrongVersionIsValidationError(t *testing.T) {
	ctrl := gomock.NewController(t)
	fsys := manifest.NewMockFS(ctrl)
	fsys.EXPECT().ReadFile("environments/dev.values.yaml").Return(nil, fsNotExistErr("environments/dev.values.yaml"))
	fsys.EXPECT().ReadFile("kraai.yaml").Return([]byte("version: 2\n"), nil)
	fsys.EXPECT().ReadFile("kraai.yaml.j2").Return(nil, fsNotExistErr("kraai.yaml.j2"))

	loader := newLoader(t, fsys, manifest.NewTemplateEngine(fsys))
	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "version") {
		t.Errorf("error %q does not mention version", kerr.Error())
	}
}

func TestLoad_TemplateRootReadErrorIsWrapped(t *testing.T) {
	ctrl := gomock.NewController(t)
	fsys := manifest.NewMockFS(ctrl)
	fsys.EXPECT().ReadFile("environments/dev.values.yaml").Return(nil, fsNotExistErr("environments/dev.values.yaml"))
	fsys.EXPECT().ReadFile("kraai.yaml").Return(nil, fsNotExistErr("kraai.yaml"))
	fsys.EXPECT().ReadFile("kraai.yaml.j2").Return(nil, errors.New("disk on fire"))

	loader := newLoader(t, fsys, manifest.NewTemplateEngine(fsys))
	_, err := loader.Load("dev", nil)
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestLoad_ServicesTemplateGlobErrorIsWrapped(t *testing.T) {
	ctrl := gomock.NewController(t)
	fsys := manifest.NewMockFS(ctrl)
	fsys.EXPECT().ReadFile("environments/dev.values.yaml").Return(nil, fsNotExistErr("environments/dev.values.yaml"))
	fsys.EXPECT().ReadFile("kraai.yaml").Return([]byte("version: 1\n"), nil)
	fsys.EXPECT().ReadFile("kraai.yaml.j2").Return(nil, fsNotExistErr("kraai.yaml.j2"))
	fsys.EXPECT().Glob("services/*.yaml").Return(nil, nil)
	fsys.EXPECT().Glob("services/*.yaml.j2").Return(nil, errors.New("glob exploded"))

	loader := newLoader(t, fsys, manifest.NewTemplateEngine(fsys))
	_, err := loader.Load("dev", nil)
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestLoad_BadSetArgPropagates(t *testing.T) {
	loader := newRealLoader(t, "testdata/blueprint")
	_, err := loader.Load("prod", []string{"nopequals"})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

// The remaining tests use MockFS/MockTemplateEngine to reach loader.go
// branches a real fixture directory can't provoke on demand: a raw
// filesystem error (not "file doesn't exist") from Glob or ReadFile.

func TestLoad_ServicesGlobErrorIsWrapped(t *testing.T) {
	ctrl := gomock.NewController(t)
	fsys := manifest.NewMockFS(ctrl)
	fsys.EXPECT().ReadFile("environments/dev.values.yaml").Return(nil, fsNotExistErr("environments/dev.values.yaml"))
	fsys.EXPECT().ReadFile("kraai.yaml").Return([]byte("version: 1\n"), nil)
	fsys.EXPECT().ReadFile("kraai.yaml.j2").Return(nil, fsNotExistErr("kraai.yaml.j2"))
	fsys.EXPECT().Glob("services/*.yaml").Return(nil, errors.New("glob exploded"))

	loader := newLoader(t, fsys, manifest.NewTemplateEngine(fsys))
	_, err := loader.Load("dev", nil)
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestLoad_ServicesReadFileErrorIsWrapped(t *testing.T) {
	ctrl := gomock.NewController(t)
	fsys := manifest.NewMockFS(ctrl)
	fsys.EXPECT().ReadFile("environments/dev.values.yaml").Return(nil, fsNotExistErr("environments/dev.values.yaml"))
	fsys.EXPECT().ReadFile("kraai.yaml").Return([]byte("version: 1\n"), nil)
	fsys.EXPECT().ReadFile("kraai.yaml.j2").Return(nil, fsNotExistErr("kraai.yaml.j2"))
	fsys.EXPECT().Glob("services/*.yaml").Return([]string{"services/api.yaml"}, nil)
	fsys.EXPECT().Glob("services/*.yaml.j2").Return(nil, nil)
	fsys.EXPECT().ReadFile("services/api.yaml").Return(nil, errors.New("disk on fire"))

	loader := newLoader(t, fsys, manifest.NewTemplateEngine(fsys))
	_, err := loader.Load("dev", nil)
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestLoad_ServicesTemplateReadFileErrorIsWrapped(t *testing.T) {
	ctrl := gomock.NewController(t)
	fsys := manifest.NewMockFS(ctrl)
	fsys.EXPECT().ReadFile("environments/dev.values.yaml").Return(nil, fsNotExistErr("environments/dev.values.yaml"))
	fsys.EXPECT().ReadFile("kraai.yaml").Return([]byte("version: 1\n"), nil)
	fsys.EXPECT().ReadFile("kraai.yaml.j2").Return(nil, fsNotExistErr("kraai.yaml.j2"))
	fsys.EXPECT().Glob("services/*.yaml").Return(nil, nil)
	fsys.EXPECT().Glob("services/*.yaml.j2").Return([]string{"services/api.yaml.j2"}, nil)
	fsys.EXPECT().ReadFile("services/api.yaml.j2").Return(nil, errors.New("disk on fire"))

	loader := newLoader(t, fsys, manifest.NewTemplateEngine(fsys))
	_, err := loader.Load("dev", nil)
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestLoad_ServiceTemplateRenderErrorPropagates(t *testing.T) {
	ctrl := gomock.NewController(t)
	fsys := manifest.NewMockFS(ctrl)
	tpl := manifest.NewMockTemplateEngine(ctrl)

	fsys.EXPECT().ReadFile("environments/dev.values.yaml").Return(nil, fsNotExistErr("environments/dev.values.yaml"))
	fsys.EXPECT().ReadFile("kraai.yaml").Return([]byte("version: 1\n"), nil)
	fsys.EXPECT().ReadFile("kraai.yaml.j2").Return(nil, fsNotExistErr("kraai.yaml.j2"))
	fsys.EXPECT().Glob("services/*.yaml").Return(nil, nil)
	fsys.EXPECT().Glob("services/*.yaml.j2").Return([]string{"services/api.yaml.j2"}, nil)
	fsys.EXPECT().ReadFile("services/api.yaml.j2").Return([]byte("services: {}"), nil)
	tpl.EXPECT().Render("services/api.yaml.j2", gomock.Any(), gomock.Any()).
		Return(nil, kerrors.Validation("services/api.yaml.j2: boom"))

	loader := newLoader(t, fsys, tpl)
	_, err := loader.Load("dev", nil)
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestLoad_EnvironmentReadErrorIsWrapped(t *testing.T) {
	ctrl := gomock.NewController(t)
	fsys := manifest.NewMockFS(ctrl)
	fsys.EXPECT().ReadFile("environments/dev.values.yaml").Return(nil, fsNotExistErr("environments/dev.values.yaml"))
	fsys.EXPECT().ReadFile("kraai.yaml").Return([]byte("version: 1\n"), nil)
	fsys.EXPECT().ReadFile("kraai.yaml.j2").Return(nil, fsNotExistErr("kraai.yaml.j2"))
	fsys.EXPECT().Glob("services/*.yaml").Return(nil, nil)
	fsys.EXPECT().Glob("services/*.yaml.j2").Return(nil, nil)
	fsys.EXPECT().ReadFile("environments/dev.yaml").Return(nil, errors.New("disk on fire"))

	loader := newLoader(t, fsys, manifest.NewTemplateEngine(fsys))
	_, err := loader.Load("dev", nil)
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestLoad_RootReadErrorIsWrapped(t *testing.T) {
	ctrl := gomock.NewController(t)
	fsys := manifest.NewMockFS(ctrl)
	fsys.EXPECT().ReadFile("environments/dev.values.yaml").Return(nil, fsNotExistErr("environments/dev.values.yaml"))
	fsys.EXPECT().ReadFile("kraai.yaml").Return(nil, errors.New("disk on fire"))

	loader := newLoader(t, fsys, manifest.NewTemplateEngine(fsys))
	_, err := loader.Load("dev", nil)
	_ = requireCode(t, err, kerrors.CodeValidation)
}

// TestLoad_PerServiceComputeParses is the schema half of the
// per-service-compute workstream's acceptance criterion: a manifest shaped
// like kraai-api's real one (services/api.yaml.j2's `tick` workaround
// replaced with a proper compute: block) loads with each service's own
// trigger, handler, schedule and settings intact, and a service's compute
// settings distinct from the provider's.
func TestLoad_PerServiceComputeParses(t *testing.T) {
	loader := newRealLoader(t, "testdata/per-service-compute")

	got, err := loader.Load("dev", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	api := got.Services["api"]
	if api.Compute == nil {
		t.Fatalf("api.Compute is nil")
	}
	if api.Compute.Trigger != manifest.TriggerHTTP {
		t.Errorf("api.Compute.Trigger = %q, want %q", api.Compute.Trigger, manifest.TriggerHTTP)
	}
	if api.Compute.Handler != "app.main.handler" {
		t.Errorf("api.Compute.Handler = %q", api.Compute.Handler)
	}
	if api.Compute.Schedule != "" {
		t.Errorf("api.Compute.Schedule = %q, want empty: api is HTTP-triggered", api.Compute.Schedule)
	}

	tick := got.Services["tick"]
	if tick.Compute == nil {
		t.Fatalf("tick.Compute is nil")
	}
	if tick.Compute.Trigger != manifest.TriggerSchedule {
		t.Errorf("tick.Compute.Trigger = %q, want %q", tick.Compute.Trigger, manifest.TriggerSchedule)
	}
	if tick.Compute.Schedule != "rate(5 minutes)" {
		t.Errorf("tick.Compute.Schedule = %q", tick.Compute.Schedule)
	}
	if tick.Compute.Settings["reservedConcurrency"] != 1 {
		t.Errorf("tick.Compute.Settings[reservedConcurrency] = %v, want the service's own override",
			tick.Compute.Settings["reservedConcurrency"])
	}

	// The provider-level block is untouched by any service's override —
	// merging (internal/plan's job) happens later, against a copy.
	compute := configuredProvider(t, got.Root.Providers, manifest.CapabilityCompute)
	if compute.Settings["runtime"] != "python3.14" {
		t.Errorf("provider settings = %+v", compute.Settings)
	}
}

// TestLoad_UnknownTriggerIsValidationError covers the trigger enum
// rejecting a value outside {http, schedule}, naming the offending service
// and value the same way every other schema violation in this package does.
func TestLoad_UnknownTriggerIsValidationError(t *testing.T) {
	loader := newRealLoader(t, "testdata/bad-trigger")
	_, err := loader.Load("dev", nil)
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "services.tick.compute.trigger") || !strings.Contains(kerr.Error(), `"cron"`) {
		t.Errorf("error %q does not name the bad trigger", kerr.Error())
	}
}

// fsNotExistErr builds the same *fs.PathError shape a real FS
// implementation returns for a missing file, so mock-based tests exercise
// the errors.Is(err, fs.ErrNotExist) branch exactly as production code
// would.
func fsNotExistErr(name string) error {
	return &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}
