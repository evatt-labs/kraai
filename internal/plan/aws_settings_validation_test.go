package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	awsprovider "github.com/evatt-labs/kraai/internal/provider/aws"
	"github.com/evatt-labs/kraai/internal/resource"
)

// specValidatorOnly wraps a real provider resource, forwarding ValidateSpec
// to it (via the same structural type-assertion internal/resource/otel.go
// itself uses to reach an optional interface) while answering Get with a
// canned "nothing exists" — so a test can drive a real provider's real
// ValidateSpec through the real planner without that provider's real Get
// making a live API call once validation passes.
//
// Registered in place of the real resource before Instrument decorates it
// (see awsComputeRegistryWithRealLambda), so what production's decorator
// wraps is this forwarder, and what it forwards ValidateSpec to is still
// internal/provider/aws's own unmodified lambdaFunctionResource — nothing
// about the real settings-validation path is faked, only the live network
// call an already-valid spec would otherwise make.
type specValidatorOnly struct {
	resource.Resource
}

func (s specValidatorOnly) ValidateSpec(spec resource.Spec) error {
	validator, ok := s.Resource.(interface{ ValidateSpec(resource.Spec) error })
	if !ok {
		return nil
	}
	return validator.ValidateSpec(spec)
}

func (s specValidatorOnly) Get(context.Context, resource.Ref) (*resource.State, error) {
	return nil, nil
}

// awsComputeRegistryWithRealLambda registers internal/provider/aws's real,
// exported registrations — unmodified, exactly as awsAPITopologyFixture
// does — except AWS::Lambda::Function keeps its real ValidateSpec (wrapped
// in specValidatorOnly, so a spec that passes validation does not go on to
// make a live Get call) instead of being swapped for a fake with no
// opinion on settings at all.
//
// This is deliberate, not an oversight: the whole point of this file's
// tests is proving that internal/provider/aws's real ValidateSpec — which
// calls decodeLambdaSettings, which calls computeSettingsSchema.Validate
// (settings_schema.go) — is what decide (planner.go) reaches, through the
// real resource.Instrument decorator this registry applies to every
// registration, exactly as internal/assemble.Registry wires a live run. A
// fake resource, or a direct unit test against decodeLambdaSettings alone,
// would not prove that wiring; the bug this whole mechanism exists to
// close — settings validation used to run only inside DiffersFromState,
// which never executes on a fresh environment's first plan because
// DiffersFromState only runs once Get has already found an existing
// resource, so a typo'd or invalid setting reached nothing at all —
// passed every unit test the pre-fix code had.
//
// Every other compute registration keeps a fakeResource, exactly as
// awsAPITopologyFixture's own doc comment explains: this keeps the test
// offline and credential-free while still exercising real registration
// data (Capability, DependsOn, Triggers, SelectedBy) for the one
// registration under test.
func awsComputeRegistryWithRealLambda(t *testing.T) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry(resource.WithDecorator(resource.Instrument(nil, nil)))

	client := &awsprovider.Client{}
	for _, r := range awsprovider.Registrations(client) {
		if r.Type == awsprovider.TypeLambdaFunction {
			r.Resource = specValidatorOnly{Resource: r.Resource}
		} else {
			r.Resource = newFakeResource()
		}
		if err := reg.Register(r); err != nil {
			t.Fatalf("Register(%s): %v", r.Key(), err)
		}
	}
	return reg
}

// awsComputeManifest builds a one-service, HTTP-triggered manifest whose
// providers.compute.settings is settings — the same shape a real
// kraai.yaml.j2 carries.
func awsComputeManifest(settings map[string]any) *manifest.Manifest {
	return &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			Compute: &manifest.Provider{Vendor: "aws", Settings: settings},
		}},
		Services: map[string]manifest.Service{
			"api": {
				Dir:     ".",
				Compute: &manifest.Compute{Trigger: manifest.TriggerHTTP, Handler: "run.sh"},
			},
		},
	}
}

func validComputeSettings() map[string]any {
	return map[string]any{"runtime": "python3.13", "architecture": "arm64"}
}

// TestAWSComputeSettingsValidation_FailsOnFreshEnvironment is this
// workstream's own version of the proof its brief demands: "prove that
// validation runs on a fresh environment where nothing exists yet." Every
// fakeResource in this plan starts with no recorded state, i.e. Get
// returns (nil, nil) for everything — a brand-new environment, the exact
// condition under which the pre-fix bug (settings_validate.go's
// validateKnownSettings, reachable only via DiffersFromState) planned
// clean. reservedConcurency (missing the second "r") is the proposal's own
// named regression fixture.
func TestAWSComputeSettingsValidation_FailsOnFreshEnvironment(t *testing.T) {
	reg := awsComputeRegistryWithRealLambda(t)
	settings := validComputeSettings()
	settings["reservedConcurency"] = 5 // typo of reservedConcurrency

	p, err := New(reg).Plan(context.Background(), awsComputeManifest(settings), envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	action := findAction(t, p, awsprovider.Provider, awsprovider.TypeLambdaFunction)
	if action.Kind != ActionFailed {
		t.Fatalf("Lambda function action.Kind = %v, want ActionFailed — a typo'd "+
			"reservedConcurrency must not plan clean against a fresh environment", action.Kind)
	}
	msg := action.Err.Error()
	for _, want := range []string{"reservedConcurency", "did you mean reservedConcurrency"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}

	// Every other compute registration for this service must still have
	// been planned as an ordinary ActionCreate: this environment is
	// genuinely fresh (fakeResource.Get finds nothing for anything), and
	// one resource's invalid settings must not corrupt the rest of the
	// plan.
	otherCreated := false
	for _, a := range p.Actions {
		if a.Provider == awsprovider.Provider && a.Type != awsprovider.TypeLambdaFunction && a.Kind == ActionCreate {
			otherCreated = true
		}
	}
	if !otherCreated {
		t.Fatal("expected at least one other aws compute registration to plan as ActionCreate")
	}
}

// TestAWSComputeSettingsValidation_NamingPrefixRegression is the proposal's
// second named regression fixture (naming.prefix), reproduced against the
// real AWS compute schema: a manifest key no reader of the merged compute
// settings map recognizes must fail loudly, on a fresh environment, the
// same as reservedConcurrency's typo above.
func TestAWSComputeSettingsValidation_NamingPrefixRegression(t *testing.T) {
	reg := awsComputeRegistryWithRealLambda(t)
	settings := validComputeSettings()
	settings["prefix"] = "kraaiapi-"

	p, err := New(reg).Plan(context.Background(), awsComputeManifest(settings), envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	action := findAction(t, p, awsprovider.Provider, awsprovider.TypeLambdaFunction)
	if action.Kind != ActionFailed {
		t.Fatalf("action.Kind = %v, want ActionFailed for an unrecognized %q key", action.Kind, "prefix")
	}
	if !strings.Contains(action.Err.Error(), "prefix") {
		t.Fatalf("error %q does not name the offending key", action.Err.Error())
	}
}

// TestAWSComputeSettingsValidation_ValidSettingsPlanCleanly is the
// necessary negative control: the two tests above only mean something if a
// correctly-spelled manifest still plans without error.
func TestAWSComputeSettingsValidation_ValidSettingsPlanCleanly(t *testing.T) {
	reg := awsComputeRegistryWithRealLambda(t)
	settings := validComputeSettings()
	settings["reservedConcurrency"] = 5
	settings["httpFrontDoor"] = "apigateway"

	p, err := New(reg).Plan(context.Background(), awsComputeManifest(settings), envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	action := findAction(t, p, awsprovider.Provider, awsprovider.TypeLambdaFunction)
	if action.Kind != ActionCreate {
		t.Fatalf("action = %+v, want ActionCreate for valid settings on a fresh environment", action)
	}
}

// TestAWSComputeSettingsValidation_WrongTypeRejectedNotCoerced proves the
// same "reject, don't coerce" contract the brief asks for at the schema
// level (internal/resource/schema_test.go) also holds through the real
// call site: a wrong-typed value must fail loudly here too, not be
// silently dropped by decodeLambdaSettings' own settingStr-style type
// assertions and then reported as merely "missing".
func TestAWSComputeSettingsValidation_WrongTypeRejectedNotCoerced(t *testing.T) {
	reg := awsComputeRegistryWithRealLambda(t)
	settings := map[string]any{
		"runtime":      12345, // wrong type: not a string
		"architecture": "arm64",
	}

	p, err := New(reg).Plan(context.Background(), awsComputeManifest(settings), envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	action := findAction(t, p, awsprovider.Provider, awsprovider.TypeLambdaFunction)
	if action.Kind != ActionFailed {
		t.Fatalf("action.Kind = %v, want ActionFailed for a wrong-typed runtime", action.Kind)
	}
	if strings.Contains(action.Err.Error(), "missing") {
		t.Fatalf("wrong-typed runtime was reported as missing (coerced away), not rejected: %q", action.Err.Error())
	}
}
