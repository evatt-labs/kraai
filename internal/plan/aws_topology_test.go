package plan

import (
	"context"
	"reflect"
	"slices"
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

	// Absolute waves. The bucket, role and gateway share wave 0 with the DB
	// branch: none of them reads a credential, and each registration says so
	// by leaving Reads at its own binding (#208). Only the function reads the
	// whole service, and it is the only compute item a declared read (#119)
	// orders behind the branch — where its bucket and role already put it.
	if apiBucket.Wave != 0 || apiRole.Wave != 0 || apiGateway.Wave != 0 {
		t.Errorf("api: bucket/role/gateway waves = %d/%d/%d, want all 0 (alongside the DB branch)",
			apiBucket.Wave, apiRole.Wave, apiGateway.Wave)
	}
	if apiBranch.Wave != 0 {
		t.Errorf("api: DB branch wave = %d, want 0", apiBranch.Wave)
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
	for i := range 10 {
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
// capability now sit under four bindings with four different names, and
// still order correctly — because each entry names the binding it needs
// (cdn's origin and certificate, dns's alias), and a reference is a read
// the graph orders (evatt-labs/kraai#197). The manifest is built by hand,
// so References is set the way the loader would have resolved it.
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
			"site": {
				Dir: ".",
				Bindings: manifest.Bindings{
					manifest.CapabilityObjects: {{"binding": "ASSETS"}},
					manifest.CapabilityTLS:     {{"binding": "CERT", "domain": "acme.example"}},
					manifest.CapabilityCDN:     {{"binding": "EDGE", "origin": "ASSETS", "certificate": "CERT"}},
					manifest.CapabilityDNS:     {{"binding": "ZONE", "zone": "acme.example", "alias": "EDGE"}},
				},
				References: map[string]map[string]string{
					"EDGE": {"origin": "ASSETS", "certificate": "CERT"},
					"ZONE": {"alias": "EDGE"},
				},
			},
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

// TestPlan_StaticSiteWithAValidatedCertificateIsNotACycle is the live
// failure that made reference reads per type: a certificate validated
// through the zone (tls → dns), a distribution presenting it (cdn → tls),
// and the zone's record pointing at the distribution (dns → cdn). As
// binding-level edges that is a cycle — the zone waited on the distribution
// its sibling record points at — and `kraai plan` refused the manifest.
// Only the record reads the alias, only the certificate reads the zone, and
// the plan is four waves.
func TestPlan_StaticSiteWithAValidatedCertificateIsNotACycle(t *testing.T) {
	reg := awsAPITopologyFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityObjects: {Vendor: "aws"},
			manifest.CapabilityDNS:     {Vendor: "aws"},
			manifest.CapabilityTLS:     {Vendor: "aws"},
			manifest.CapabilityCDN:     {Vendor: "aws"},
		}},
		Services: map[string]manifest.Service{
			"site": {
				Dir: ".",
				Bindings: manifest.Bindings{
					manifest.CapabilityObjects: {{"binding": "ASSETS"}},
					manifest.CapabilityDNS:     {{"binding": "ZONE", "zone": "acme.example", "alias": "EDGE"}},
					manifest.CapabilityTLS:     {{"binding": "CERT", "domain": "acme.example", "zone": "ZONE"}},
					manifest.CapabilityCDN:     {{"binding": "EDGE", "origin": "ASSETS", "certificate": "CERT", "aliases": []any{"acme.example"}}},
				},
				References: map[string]map[string]string{
					"ZONE": {"alias": "EDGE"},
					"CERT": {"zone": "ZONE"},
					"EDGE": {"origin": "ASSETS", "certificate": "CERT"},
				},
			},
		},
	}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v — the static site's own shape planned as a cycle", err)
	}

	waves := map[string]int{}
	for _, a := range p.Actions {
		waves[a.Type] = a.Wave
	}
	want := map[string]int{
		"AWS::S3::Bucket":                      0,
		"AWS::Route53::HostedZone":             0,
		"AWS::CertificateManager::Certificate": 1,
		"AWS::CloudFront::Distribution":        2,
		"AWS::Route53::RecordSet":              3,
	}
	for typ, wave := range want {
		if waves[typ] != wave {
			t.Errorf("%s in wave %d, want %d (waves: %v)", typ, waves[typ], wave, waves)
		}
	}
}

// The companion, pinning what is no longer load-bearing: four entries that
// share a binding name but reference nothing are not related by the name.
// The distribution lands in the same wave as the bucket, and that is
// correct — a name coincidence was the silent trap evatt-labs/kraai#197
// closed, and if it ever orders again something is inferring a
// relationship the manifest never wrote. (A real cdn entry cannot reach
// here without an origin; the schema requires one.)
func TestPlan_StaticSiteSharedNamesAreNotARelationship(t *testing.T) {
	reg := awsAPITopologyFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityObjects: {Vendor: "aws"},
			manifest.CapabilityCDN:     {Vendor: "aws"},
		}},
		Services: map[string]manifest.Service{
			"site": {Dir: ".", Bindings: manifest.Bindings{
				manifest.CapabilityObjects: {{"binding": "SITE"}},
				manifest.CapabilityCDN:     {{"binding": "SITE"}},
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
		t.Fatalf("distribution and bucket were ordered by their shared name alone: %v", waves)
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

	// What each reads is exactly what it needs (#208): the domain name its
	// route's certificate binding, the mapping only its own — which must be
	// present, or the mapping could not read the id its API published.
	if got, want := domain.ReadsBindings, []string{"CERT", "api"}; !reflect.DeepEqual(got, want) {
		t.Errorf("domain name reads %v, want %v", got, want)
	}
	if got, want := mapping.ReadsBindings, []string{"api"}; !reflect.DeepEqual(got, want) {
		t.Errorf("api mapping reads %v, want %v", got, want)
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

// TestPlan_AWSQueueOrdersBeforeTheFunctionNotTheRole pins how a queues
// binding fits the compute graph: the function reads every binding on its
// service and so waits for the queue, while the role, which grants the
// queue by an ARN it builds locally, stays in the first wave beside it.
func TestPlan_AWSQueueOrdersBeforeTheFunctionNotTheRole(t *testing.T) {
	reg := awsAPITopologyFixture(t)
	m := kraaiAPIManifest()
	m.Root.Providers[manifest.CapabilityQueues] = &manifest.Provider{Vendor: "aws"}
	api := m.Services["api"]
	api.Bindings[manifest.CapabilityQueues] = []manifest.Binding{{"binding": "JOBS"}}
	m.Services["api"] = api

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	queue := findServiceAction(t, p, "api", awsprovider.TypeSQSQueue)
	role := findServiceAction(t, p, "api", awsprovider.TypeIAMRole)
	function := findServiceAction(t, p, "api", awsprovider.TypeLambdaFunction)
	if queue.Wave >= function.Wave {
		t.Errorf("queue wave %d, function wave %d — the function reads the queue's URL, so the queue must come first",
			queue.Wave, function.Wave)
	}
	if role.Wave != queue.Wave {
		t.Errorf("role wave %d, queue wave %d — the role grants the queue by a locally built ARN and must not wait for it",
			role.Wave, queue.Wave)
	}
}

// TestPlan_AWSCacheOrdersAfterItsNetwork pins how a redis keyvalue binding
// fits the graph through its network reference: the security group waits
// for the VPC, the cache waits for the group and the subnet, and the
// function, which receives the cache's URL, waits for the cache.
func TestPlan_AWSCacheOrdersAfterItsNetwork(t *testing.T) {
	reg := awsAPITopologyFixture(t)
	m := kraaiAPIManifest()
	m.Root.Providers[manifest.CapabilityNetwork] = &manifest.Provider{Vendor: "aws"}
	m.Root.Providers[manifest.CapabilityKeyValue] = &manifest.Provider{Vendor: "aws"}
	api := m.Services["api"]
	api.Bindings[manifest.CapabilityNetwork] = []manifest.Binding{{"binding": "NET", "cidr": "10.90.0.0/16", "subnet": "10.90.1.0/24"}}
	api.Bindings[manifest.CapabilityKeyValue] = []manifest.Binding{{"binding": "CACHE", "driver": "redis", "network": "NET"}}
	api.References = map[string]map[string]string{"CACHE": {"network": "NET"}}
	m.Services["api"] = api

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	waves := map[string]int{}
	for _, a := range p.Actions {
		if a.ServiceKey == "api" {
			waves[a.Type] = a.Wave
		}
	}
	// Declared reads, not just wave numbers: the subnet happens to share a
	// wave with the security group, so the cache would land after it even
	// with the read edge missing.
	for _, typ := range []string{awsprovider.TypeCacheSecurityGroup, awsprovider.TypeElastiCacheServerlessCache} {
		if reads := findServiceAction(t, p, "api", typ).ReadsBindings; !slices.Contains(reads, "NET") {
			t.Errorf("%s reads %v, want the NET network binding it references", typ, reads)
		}
	}
	for _, c := range []struct{ earlier, later string }{
		{awsprovider.TypeVPC, awsprovider.TypeCacheSecurityGroup},
		{awsprovider.TypeCacheSecurityGroup, awsprovider.TypeElastiCacheServerlessCache},
		{awsprovider.TypeSubnet, awsprovider.TypeElastiCacheServerlessCache},
		{awsprovider.TypeElastiCacheServerlessCache, awsprovider.TypeLambdaFunction},
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

// TestPlan_AWSNetworkEgressOrdersTheFunctionBehindTheNAT pins the private
// half of a network: the NAT gateway waits for the EIP, the public subnet
// and the gateway attachment; the private route waits for the NAT and the
// private table; the function, placed in the private subnet, waits for it.
func TestPlan_AWSNetworkEgressOrdersTheFunctionBehindTheNAT(t *testing.T) {
	reg := awsAPITopologyFixture(t)
	m := kraaiAPIManifest()
	m.Root.Providers[manifest.CapabilityNetwork] = &manifest.Provider{Vendor: "aws"}
	api := m.Services["api"]
	api.Bindings[manifest.CapabilityNetwork] = []manifest.Binding{
		{"binding": "NET", "cidr": "10.90.0.0/16", "subnet": "10.90.1.0/24", "private": "10.90.2.0/24"},
	}
	m.Services["api"] = api

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	waves := map[string]int{}
	for _, a := range p.Actions {
		if a.ServiceKey == "api" {
			waves[a.Type] = a.Wave
		}
	}
	for _, c := range []struct{ earlier, later string }{
		{awsprovider.TypeEIP, awsprovider.TypeNatGateway},
		{awsprovider.TypeSubnet, awsprovider.TypeNatGateway},
		{awsprovider.TypeVPCGatewayAttachment, awsprovider.TypeNatGateway},
		{awsprovider.TypeNatGateway, awsprovider.TypePrivateRoute},
		{awsprovider.TypePrivateRouteTable, awsprovider.TypePrivateRoute},
		{awsprovider.TypePrivateSubnet, awsprovider.TypePrivateSubnetRouteTableAssociation},
		{awsprovider.TypePrivateSubnet, awsprovider.TypeLambdaFunction},
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
