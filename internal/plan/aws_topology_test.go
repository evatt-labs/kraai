package plan

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	awsprovider "github.com/evatt-labs/kraai/internal/provider/aws"
	"github.com/evatt-labs/kraai/internal/provider/neon"
	"github.com/evatt-labs/kraai/internal/provider/neonresource"
	"github.com/evatt-labs/kraai/internal/resource"
)

// awsAPITopologyFixture registers this package's real, exported
// registrations — internal/provider/aws.Registrations and
// internal/provider/neonresource.Registrations, unmodified — swapping only
// each Registration.Resource for a fakeResource so Get never reaches a real
// cloud API. Capability, DependsOn, Applies, Lookup and every
// other field come straight from production: this is what "design it
// against the real registrations" (the task's own instruction) means in a
// test that must run offline, with no credentials, in CI.
//
// This is the acceptance criterion for the whole workstream: a manifest
// shaped like ~/Code/kraai-api (not modified — this fixture lives entirely
// in this repo) must order its compute resources correctly in one pass.
func awsAPITopologyFixture(t *testing.T) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry()

	for _, r := range awsprovider.Registrations(&awsprovider.Client{}) {
		r.Resource = newFakeResource()
		if err := reg.Register(r); err != nil {
			t.Fatalf("Register(%s): %v", r.Key(), err)
		}
	}

	// cfClient is nil: kraai-api's compute vendor is aws, not cloudflare, so
	// the Hyperdrive companion's own When condition
	// (RequiresCapabilityVendor(CapabilityCompute, HyperdriveProvider))
	// would exclude it even if it were registered — passing nil here
	// mirrors what internal/assemble actually does when no manifest names
	// Cloudflare at all (see neonresource.Registrations's own doc comment).
	for _, r := range neonresource.Registrations(neon.New("test-key"), nil, neonresource.BranchSettings{
		Project: "kraai", Database: "neondb", Role: "app_user",
	}) {
		r.Resource = newFakeResource()
		if err := reg.Register(r); err != nil {
			t.Fatalf("Register(%s): %v", r.Key(), err)
		}
	}
	return reg
}

// kraaiAPIManifest builds a manifest matching ~/Code/kraai-api's real
// shape (services/api.yaml, providers.compute=aws, providers.database=neon):
// two services, "api" (HTTP-triggered, default apigateway front door) and
// "tick" (schedule-triggered), each with one Postgres binding named "DB".
func kraaiAPIManifest() *manifest.Manifest {
	return &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityCompute:  {Vendor: "aws"},
			manifest.CapabilityDatabase: {Vendor: "neon"},
		}},
		Services: map[string]manifest.Service{
			"api": {
				Dir:      ".",
				Compute:  &manifest.Compute{Trigger: manifest.TriggerHTTP, Handler: "run.sh"},
				Bindings: manifest.Bindings{manifest.CapabilityDatabase: {{"binding": "DB", "driver": "postgres"}}},
			},
			"tick": {
				Dir:      ".",
				Compute:  &manifest.Compute{Trigger: manifest.TriggerSchedule, Handler: "app.tasks.tick.handler", Schedule: "rate(5 minutes)"},
				Bindings: manifest.Bindings{manifest.CapabilityDatabase: {{"binding": "DB", "driver": "postgres"}}},
			},
		},
	}
}

// findServiceAction finds the Action for svcKey's own registration of typ,
// distinguishing services the way findAction (planner_test.go) cannot —
// two services here both plan a Lambda function, an artifact bucket, etc,
// and this topology test needs to assert on each service's own instance,
// not "whichever one findAction happens to see first."
func findServiceAction(t *testing.T, p *Plan, svcKey, typ string) Action {
	t.Helper()
	for _, a := range p.Actions {
		if a.ServiceKey == svcKey && a.Type == typ {
			return a
		}
	}
	t.Fatalf("no action for service %q type %q in %+v", svcKey, typ, p.Actions)
	return Action{}
}

// TestPlan_AWSAPITopology_WaveAssignment is the acceptance criterion this
// entire workstream exists to satisfy, proven directly rather than by
// hand-waving: a fresh apply of a manifest shaped like kraai-api must order
// artifact bucket and IAM role before the function, the function before its
// permission, and the API Gateway before the API Gateway permission — all
// in one pass, with no manual phase assignment anywhere in sight.
//
// The real, live-account evidence this fixes (see
// resource.Registration.DependsOn's doc comment): both Lambda::Permission
// registrations failed on a second `kraai apply` run because they needed
// their function (and, for the API Gateway one, the gateway) to exist
// first, and every compute type sat in the same PhaseCompute with no
// ordering between them.
func TestPlan_AWSAPITopology_WaveAssignment(t *testing.T) {
	reg := awsAPITopologyFixture(t)
	m := kraaiAPIManifest()

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// "api": HTTP-triggered, default front door (apigateway).
	apiBucket := findServiceAction(t, p, "api", awsprovider.TypeArtifactBucket)
	apiRole := findServiceAction(t, p, "api", awsprovider.TypeIAMRole)
	apiFunction := findServiceAction(t, p, "api", awsprovider.TypeLambdaFunction)
	apiGateway := findServiceAction(t, p, "api", awsprovider.TypeAPIGatewayV2API)
	apiPermission := findServiceAction(t, p, "api", awsprovider.TypePermissionAPIGateway)

	if apiBucket.Wave >= apiFunction.Wave {
		t.Errorf("api: artifact bucket wave %d, function wave %d — bucket must come strictly before the function",
			apiBucket.Wave, apiFunction.Wave)
	}
	if apiRole.Wave >= apiFunction.Wave {
		t.Errorf("api: IAM role wave %d, function wave %d — role must come strictly before the function",
			apiRole.Wave, apiFunction.Wave)
	}
	if apiFunction.Wave >= apiPermission.Wave {
		t.Errorf("api: function wave %d, permission wave %d — function must come strictly before its permission",
			apiFunction.Wave, apiPermission.Wave)
	}
	if apiGateway.Wave >= apiPermission.Wave {
		t.Errorf("api: gateway wave %d, permission wave %d — the API Gateway must come strictly before the "+
			"permission that authorizes it to invoke the function", apiGateway.Wave, apiPermission.Wave)
	}
	// Exact values, not just relative order — the specific topology this
	// workstream's own PR body documents.
	if apiBucket.Wave != 0 || apiRole.Wave != 0 || apiGateway.Wave != 0 {
		t.Errorf("api: bucket/role/gateway waves = %d/%d/%d, want all 0",
			apiBucket.Wave, apiRole.Wave, apiGateway.Wave)
	}
	if apiFunction.Wave != 1 {
		t.Errorf("api: function wave = %d, want 1", apiFunction.Wave)
	}
	if apiPermission.Wave != 2 {
		t.Errorf("api: permission wave = %d, want 2", apiPermission.Wave)
	}

	// "tick": schedule-triggered, no HTTP surface at all — must plan an
	// EventBridge rule and its permission, never an API Gateway or a
	// Lambda::Permission::APIGateway (applicability gating, unrelated
	// to this workstream but still load-bearing for the topology).
	tickBucket := findServiceAction(t, p, "tick", awsprovider.TypeArtifactBucket)
	tickRole := findServiceAction(t, p, "tick", awsprovider.TypeIAMRole)
	tickFunction := findServiceAction(t, p, "tick", awsprovider.TypeLambdaFunction)
	tickPermission := findServiceAction(t, p, "tick", awsprovider.TypePermissionEventsRule)

	if tickBucket.Wave != 0 || tickRole.Wave != 0 {
		t.Errorf("tick: bucket/role waves = %d/%d, want both 0", tickBucket.Wave, tickRole.Wave)
	}
	if tickFunction.Wave != 1 {
		t.Errorf("tick: function wave = %d, want 1", tickFunction.Wave)
	}
	if tickPermission.Wave != 2 {
		t.Errorf("tick: permission wave = %d, want 2", tickPermission.Wave)
	}

	for _, a := range p.Actions {
		if a.ServiceKey == "api" && (a.Type == awsprovider.TypeLambdaURL || a.Type == awsprovider.TypeEventsRule ||
			a.Type == awsprovider.TypePermissionEventsRule) {
			t.Errorf("api service unexpectedly planned %s (schedule/url-only type)", a.Type)
		}
		if a.ServiceKey == "tick" && (a.Type == awsprovider.TypeAPIGatewayV2API || a.Type == awsprovider.TypeLambdaURL ||
			a.Type == awsprovider.TypePermissionAPIGateway) {
			t.Errorf("tick service unexpectedly planned %s (http-only type)", a.Type)
		}
	}

	// The whole plan converges in three waves — the direct proof of the
	// workstream's headline claim: a fresh apply now orders this topology
	// correctly in one pass, where the old phase model took three runs to
	// converge on a live account.
	maxWave := 0
	for _, a := range p.Actions {
		if a.Wave > maxWave {
			maxWave = a.Wave
		}
	}
	if maxWave != 2 {
		t.Errorf("plan spans %d wave(s) (0..%d), want exactly 3 (0..2)", maxWave+1, maxWave)
	}
}

// TestPlan_AWSAPITopology_Deterministic pins that this exact real-registration
// topology produces the same wave assignment on every run, not just in the
// abstract (graph_test.go's TestComputeWaves_Deterministic) but against the
// concrete registrations this workstream's PR body reports waves for.
func TestPlan_AWSAPITopology_Deterministic(t *testing.T) {
	m := kraaiAPIManifest()

	var first *Plan
	for i := 0; i < 10; i++ {
		p, err := New(awsAPITopologyFixture(t)).Plan(context.Background(), m, envName)
		if err != nil {
			t.Fatalf("Plan (run %d): %v", i, err)
		}
		if first == nil {
			first = p
			continue
		}
		if len(p.Actions) != len(first.Actions) {
			t.Fatalf("run %d: %d actions, want %d", i, len(p.Actions), len(first.Actions))
		}
		for j := range p.Actions {
			a, b := p.Actions[j], first.Actions[j]
			if a.ServiceKey != b.ServiceKey || a.Type != b.Type || a.Wave != b.Wave {
				t.Fatalf("run %d: action[%d] = %+v, want %+v (first run)", i, j, a, b)
			}
		}
	}
}

// TestPlan_StaticSiteOrdersAcrossCapabilities is the decomposition's own
// acceptance criterion: the five types that used to share the `objects`
// capability now sit under four, and must still order correctly.
//
// They do because groupKey is (service, binding) and carries no capability,
// so four entries sharing one binding name are one expansion group and their
// DependsOn edges resolve across capabilities exactly as they did when all
// five were one binding. That is load-bearing and invisible — hence this
// test, and its companion below for what happens when the names differ.
func TestPlan_StaticSiteOrdersAcrossCapabilities(t *testing.T) {
	reg := awsAPITopologyFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityObjects: {Vendor: "aws"},
			manifest.CapabilityDNS:     {Vendor: "aws"},
			manifest.CapabilityTLS:     {Vendor: "aws"},
			manifest.CapabilityCDN:     {Vendor: "aws"},
		}},
		Services: map[string]manifest.Service{
			"site": {Dir: ".", Bindings: manifest.Bindings{
				manifest.CapabilityObjects: {{"binding": "SITE"}},
				manifest.CapabilityTLS:     {{"binding": "SITE", "domain": "acme.example"}},
				manifest.CapabilityCDN:     {{"binding": "SITE"}},
				manifest.CapabilityDNS:     {{"binding": "SITE", "zone": "acme.example"}},
			}},
		},
	}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	waves := map[string]int{}
	for _, a := range p.Actions {
		waves[a.Type] = a.Wave
	}

	// The distribution needs its origin bucket and its certificate; the
	// record needs its zone and the distribution it aliases.
	for _, c := range []struct{ earlier, later string }{
		{"AWS::S3::Bucket", "AWS::CloudFront::Distribution"},
		{"AWS::CertificateManager::Certificate", "AWS::CloudFront::Distribution"},
		{"AWS::Route53::HostedZone", "AWS::Route53::RecordSet"},
		{"AWS::CloudFront::Distribution", "AWS::Route53::RecordSet"},
	} {
		e, ok := waves[c.earlier]
		if !ok {
			t.Fatalf("%s was not planned (waves: %v)", c.earlier, waves)
		}
		l, ok := waves[c.later]
		if !ok {
			t.Fatalf("%s was not planned (waves: %v)", c.later, waves)
		}
		if e >= l {
			t.Errorf("%s (wave %d) must come before %s (wave %d)", c.earlier, e, c.later, l)
		}
	}
}

// The companion to the test above, pinning the hazard rather than the
// guarantee: the same four entries under different binding names are four
// groups, no DependsOn resolves across them, and the distribution lands in
// the same wave as the bucket it is supposed to front.
//
// Asserted rather than left undiscovered, because this is what the
// decomposition made reachable — one capability meant one binding meant one
// group, and there was nothing to get wrong. Not currently a live bug: none
// of these four types can be created at all yet (evatt-labs/kraai#117). When
// evatt-labs/kraai#197 decides what a cross-capability edge should mean, this
// test is what has to change, and it should fail loudly when it does rather
// than quietly keep passing.
func TestPlan_StaticSiteLosesOrderingWhenBindingNamesDiffer(t *testing.T) {
	reg := awsAPITopologyFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityObjects: {Vendor: "aws"},
			manifest.CapabilityCDN:     {Vendor: "aws"},
		}},
		Services: map[string]manifest.Service{
			"site": {Dir: ".", Bindings: manifest.Bindings{
				manifest.CapabilityObjects: {{"binding": "ASSETS"}},
				manifest.CapabilityCDN:     {{"binding": "EDGE"}},
			}},
		},
	}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	waves := map[string]int{}
	for _, a := range p.Actions {
		waves[a.Type] = a.Wave
	}
	if waves["AWS::CloudFront::Distribution"] != waves["AWS::S3::Bucket"] {
		t.Fatalf("expected the documented hazard — distribution and bucket in one wave — got %v. "+
			"If this now orders correctly, evatt-labs/kraai#197 has been fixed and both this test "+
			"and groupKey's doc comment need updating.", waves)
	}
}
