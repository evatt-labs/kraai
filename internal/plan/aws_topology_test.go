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
	apiBranch := findServiceAction(t, p, "api", neonresource.TypeBranch)
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
	// #119: the function reads DB's credential (envSecrets), so it must run
	// strictly after the branch that produces it. This is the ordering that
	// used to hold by accident — the function was pushed to wave 1 by its
	// bucket and role — and now holds because a declared read is an edge.
	if apiBranch.Wave >= apiFunction.Wave {
		t.Errorf("api: DB branch wave %d, function wave %d — the function reads the branch's "+
			"credential and must come strictly after it", apiBranch.Wave, apiFunction.Wave)
	}

	// Absolute waves. Every compute item sits one wave behind the DB branch,
	// because expandCompute gives every compute type the same ReadsBindings
	// (the service's full binding set) and a declared read is now an ordering
	// edge (#119). Only the function actually reads a credential; the bucket,
	// role and gateway are over-ordered by a wave. That is the cost of the
	// planner having no per-type way to say who reads what — #208 — and it is
	// a cost in latency, never in correctness.
	if apiBucket.Wave != 1 || apiRole.Wave != 1 || apiGateway.Wave != 1 {
		t.Errorf("api: bucket/role/gateway waves = %d/%d/%d, want all 1 (behind the DB branch)",
			apiBucket.Wave, apiRole.Wave, apiGateway.Wave)
	}
	if apiFunction.Wave != 2 {
		t.Errorf("api: function wave = %d, want 2", apiFunction.Wave)
	}
	if apiPermission.Wave != 3 {
		t.Errorf("api: permission wave = %d, want 3", apiPermission.Wave)
	}

	// "tick": schedule-triggered, no HTTP surface at all — must plan an
	// EventBridge rule and its permission, never an API Gateway or a
	// Lambda::Permission::APIGateway (applicability gating, unrelated
	// to this workstream but still load-bearing for the topology).
	tickBucket := findServiceAction(t, p, "tick", awsprovider.TypeArtifactBucket)
	tickRole := findServiceAction(t, p, "tick", awsprovider.TypeIAMRole)
	tickFunction := findServiceAction(t, p, "tick", awsprovider.TypeLambdaFunction)
	tickPermission := findServiceAction(t, p, "tick", awsprovider.TypePermissionEventsRule)

	if tickBucket.Wave != 1 || tickRole.Wave != 1 {
		t.Errorf("tick: bucket/role waves = %d/%d, want both 1", tickBucket.Wave, tickRole.Wave)
	}
	if tickFunction.Wave != 2 {
		t.Errorf("tick: function wave = %d, want 2", tickFunction.Wave)
	}
	if tickPermission.Wave != 3 {
		t.Errorf("tick: permission wave = %d, want 3", tickPermission.Wave)
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
	if maxWave != 3 {
		t.Errorf("plan spans %d wave(s) (0..%d), want exactly 4 (0..3)", maxWave+1, maxWave)
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

// TestPlan_CustomDomainRouteOrdersCertificateDomainMapping is #110's own
// acceptance criterion against the real AWS registrations: a route with a
// custom domain plans a domain name and a mapping, named by the hostname
// rather than the service, and they order behind the certificate and the API
// they need.
//
// The certificate lives in a different binding (tls, "CERT") from the domain
// name (compute, "api"), so nothing in DependsOn connects them. What orders
// the domain name after it is the read edge #119 added — this is the case
// that made that fix stop being latent.
func TestPlan_CustomDomainRouteOrdersCertificateDomainMapping(t *testing.T) {
	reg := awsAPITopologyFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityCompute: {Vendor: "aws"},
			manifest.CapabilityTLS:     {Vendor: "aws"},
		}},
		Services: map[string]manifest.Service{
			"api": {
				Dir:     ".",
				Compute: &manifest.Compute{Trigger: manifest.TriggerHTTP, Handler: "run.sh"},
				Bindings: manifest.Bindings{
					manifest.CapabilityTLS: {{"binding": "CERT", "domain": "api.example.com"}},
				},
			},
		},
		Environment: manifest.Environment{
			Kind: manifest.EnvironmentKindPersistent,
			Routes: map[string][]manifest.Route{
				"api": {{Pattern: "api.example.com", CustomDomain: true, Certificate: "CERT"}},
			},
		},
	}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	domain := findServiceAction(t, p, "api", awsprovider.TypeAPIGatewayV2DomainName)
	mapping := findServiceAction(t, p, "api", awsprovider.TypeAPIGatewayV2ApiMapping)
	api := findServiceAction(t, p, "api", awsprovider.TypeAPIGatewayV2API)
	cert := findServiceAction(t, p, "api", awsprovider.TypeCertificateManagerCertificate)

	// Named by the hostname: that is the identity Cloud Control addresses
	// these by, and the only name that would find them again.
	for _, a := range []Action{domain, mapping} {
		if a.Ref.Name != "api.example.com" {
			t.Errorf("%s named %q, want the route pattern", a.Type, a.Ref.Name)
		}
		// In the compute binding group, so DependsOn on the API resolves
		// and the tls binding is readable.
		if a.Binding != "api" {
			t.Errorf("%s in binding %q, want the service's compute group", a.Type, a.Binding)
		}
		if a.Spec.Config["route"] == nil {
			t.Errorf("%s has no route in its config", a.Type)
		}
	}

	if cert.Wave >= domain.Wave {
		t.Errorf("certificate wave %d, domain name wave %d — the domain presents the "+
			"certificate and must come strictly after it", cert.Wave, domain.Wave)
	}
	if api.Wave >= mapping.Wave || domain.Wave >= mapping.Wave {
		t.Errorf("api %d / domain %d / mapping %d — the mapping needs both first",
			api.Wave, domain.Wave, mapping.Wave)
	}

	// The API is told to close its generated hostname.
	if api.Spec.Config["customDomains"] == nil {
		t.Error("the API's config carries no customDomains, so it cannot close execute-api")
	}
}

// Without a custom-domain route, neither type is planned and the API keeps
// its generated hostname — the ordinary case, unchanged.
func TestPlan_NoCustomDomainPlansNoDomainName(t *testing.T) {
	reg := awsAPITopologyFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityCompute: {Vendor: "aws"}}},
		Services: map[string]manifest.Service{
			"api": {Dir: ".", Compute: &manifest.Compute{Trigger: manifest.TriggerHTTP}},
		},
		Environment: manifest.Environment{
			Kind:   manifest.EnvironmentKindPersistent,
			Routes: map[string][]manifest.Route{"api": {{Pattern: "api.example.com"}}},
		},
	}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, a := range p.Actions {
		if a.Type == awsprovider.TypeAPIGatewayV2DomainName || a.Type == awsprovider.TypeAPIGatewayV2ApiMapping {
			t.Errorf("%s was planned with no custom-domain route", a.Type)
		}
		if a.Type == awsprovider.TypeAPIGatewayV2API && a.Spec.Config["customDomains"] != nil {
			t.Error("the API's config carries customDomains without a custom domain")
		}
	}
}
