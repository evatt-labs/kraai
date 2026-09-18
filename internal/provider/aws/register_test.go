package aws

import (
	"context"
	"reflect"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

func TestRegisterWiresEveryType(t *testing.T) {
	reg := resource.NewRegistry()
	client := &Client{}
	if err := Register(reg, client); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cases := []struct {
		key        string
		capability string
		dependsOn  []string
		lookup     resource.LookupStrategy
	}{
		{Provider + "/" + TypeS3Bucket, manifest.CapabilityObjects, nil, resource.LookupByName},
		{Provider + "/" + TypeCloudFrontDistribution, manifest.CapabilityObjects,
			[]string{key(TypeS3Bucket), key(TypeCertificateManagerCertificate)}, resource.LookupByAttr},
		{Provider + "/" + TypeCertificateManagerCertificate, manifest.CapabilityObjects, nil, resource.LookupByTag},
		{Provider + "/" + TypeRoute53HostedZone, manifest.CapabilityObjects, nil, resource.LookupByAPI},
		{Provider + "/" + TypeRoute53RecordSet, manifest.CapabilityObjects,
			[]string{key(TypeRoute53HostedZone), key(TypeCloudFrontDistribution)}, resource.LookupByAttr},
		{Provider + "/" + TypeLambdaFunction, manifest.CapabilityCompute,
			[]string{key(TypeArtifactBucket), key(TypeIAMRole)}, resource.LookupByName},
		{Provider + "/" + TypeAPIGatewayV2API, manifest.CapabilityCompute, nil, resource.LookupByTag},
		{Provider + "/" + TypeArtifactBucket, manifest.CapabilityCompute, nil, resource.LookupByName},
		{Provider + "/" + TypeIAMRole, manifest.CapabilityCompute, nil, resource.LookupByName},
		{Provider + "/" + TypeLambdaURL, manifest.CapabilityCompute, []string{key(TypeLambdaFunction)}, resource.LookupByAttr},
		{Provider + "/" + TypeEventsRule, manifest.CapabilityCompute, nil, resource.LookupByName},
		{Provider + "/" + TypePermissionEventsRule, manifest.CapabilityCompute,
			[]string{key(TypeLambdaFunction)}, resource.LookupByAttr},
		{Provider + "/" + TypePermissionAPIGateway, manifest.CapabilityCompute,
			[]string{key(TypeLambdaFunction), key(TypeAPIGatewayV2API)}, resource.LookupByAttr},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			got, ok := reg.Lookup(tc.key)
			if !ok {
				t.Fatalf("Lookup(%q): not registered", tc.key)
			}
			if got.Capability != tc.capability || got.Lookup != tc.lookup {
				t.Fatalf("registration = %+v, want capability=%s lookup=%s", got, tc.capability, tc.lookup)
			}
			if !reflect.DeepEqual(got.DependsOn, tc.dependsOn) {
				t.Fatalf("DependsOn = %v, want %v", got.DependsOn, tc.dependsOn)
			}
		})
	}
}

// TestComputeRegistrationsTriggerGating is the per-service-compute fix
// pinned at this package's own boundary: the API Gateway registration only
// applies to an HTTP-triggered service, while the Lambda function
// registration applies regardless — the exact shape that stops a
// schedule-invoked service like kraai-api's `tick` from planning an API
// Gateway nothing will ever call.
func TestComputeRegistrationsTriggerGating(t *testing.T) {
	regs := Registrations(&Client{})

	byType := map[string]resource.Registration{}
	for _, r := range regs {
		byType[r.Type] = r
	}
	function, httpAPI := byType[TypeLambdaFunction], byType[TypeAPIGatewayV2API]
	bucket, role := byType[TypeArtifactBucket], byType[TypeIAMRole]
	url, rule := byType[TypeLambdaURL], byType[TypeEventsRule]

	// Registrations are now selected at one point, against one context, so
	// these assertions name both dimensions rather than calling a
	// trigger-only check the planner no longer has. Where a registration has
	// no settings condition, the settings passed here are irrelevant and left
	// nil; where it has one, the settings that select it are supplied, so
	// this test measures the trigger dimension rather than tripping over the
	// other one.
	appliesTo := func(reg resource.Registration, trigger string, settings map[string]any) bool {
		return reg.Matches(resource.ApplicabilityContext{Trigger: trigger, Settings: settings})
	}
	frontDoor := func(v string) map[string]any { return map[string]any{"httpFrontDoor": v} }

	if len(function.Applies) != 0 {
		t.Fatalf("TypeLambdaFunction has %d condition(s), want none: every service gets a function regardless of trigger",
			len(function.Applies))
	}
	if !appliesTo(function, manifest.TriggerHTTP, nil) ||
		!appliesTo(function, manifest.TriggerSchedule, nil) ||
		!appliesTo(function, "", nil) {
		t.Fatal("TypeLambdaFunction must apply to every trigger, including none declared")
	}

	if !appliesTo(httpAPI, manifest.TriggerHTTP, frontDoor(httpFrontDoorAPIGateway)) {
		t.Error("TypeAPIGatewayV2API must apply to an HTTP-triggered service")
	}
	if appliesTo(httpAPI, manifest.TriggerSchedule, frontDoor(httpFrontDoorAPIGateway)) {
		t.Error("TypeAPIGatewayV2API must not apply to a schedule-triggered service — this is the bug this workstream fixes")
	}
	if !appliesTo(httpAPI, "", nil) {
		t.Error("TypeAPIGatewayV2API must still apply to a service declaring no compute: block, unchanged from before triggers existed")
	}

	// The artifact bucket and execution role apply to every compute
	// service unconditionally — every Lambda needs a package and a role
	// regardless of how it's invoked.
	for name, reg := range map[string]resource.Registration{"bucket": bucket, "role": role} {
		if len(reg.Applies) != 0 {
			t.Errorf("%s has %d condition(s), want none", name, len(reg.Applies))
		}
		if !appliesTo(reg, manifest.TriggerHTTP, nil) ||
			!appliesTo(reg, manifest.TriggerSchedule, nil) ||
			!appliesTo(reg, "", nil) {
			t.Errorf("%s must apply to every trigger", name)
		}
	}

	if !appliesTo(url, manifest.TriggerHTTP, frontDoor(httpFrontDoorURL)) {
		t.Error("TypeLambdaURL must apply to an HTTP-triggered service")
	}
	if appliesTo(url, manifest.TriggerSchedule, frontDoor(httpFrontDoorURL)) {
		t.Error("TypeLambdaURL must not apply to a schedule-triggered service")
	}

	if !appliesTo(rule, manifest.TriggerSchedule, nil) {
		t.Error("TypeEventsRule must apply to a schedule-triggered service")
	}
	if appliesTo(rule, manifest.TriggerHTTP, nil) {
		t.Error("TypeEventsRule must not apply to an HTTP-triggered service")
	}

	rulePermission, apiPermission := byType[TypePermissionEventsRule], byType[TypePermissionAPIGateway]
	if !appliesTo(rulePermission, manifest.TriggerSchedule, nil) {
		t.Error("TypePermissionEventsRule must apply to a schedule-triggered service")
	}
	if appliesTo(rulePermission, manifest.TriggerHTTP, nil) {
		t.Error("TypePermissionEventsRule must not apply to an HTTP-triggered service")
	}
	if !appliesTo(apiPermission, manifest.TriggerHTTP, frontDoor(httpFrontDoorAPIGateway)) {
		t.Error("TypePermissionAPIGateway must apply to an HTTP-triggered service")
	}
	if appliesTo(apiPermission, manifest.TriggerSchedule, frontDoor(httpFrontDoorAPIGateway)) {
		t.Error("TypePermissionAPIGateway must not apply to a schedule-triggered service")
	}
}

// TestHTTPFrontDoorRegistrationsAreMutuallyExclusive is PR #80's review
// fix at this package's own boundary: AWS::Lambda::Url and
// AWS::ApiGatewayV2::Api both apply to TriggerHTTP, so a trigger condition
// alone cannot stop a service planning both. A settings condition must
// select exactly one, and its own accompanying permission
// (TypePermissionAPIGateway) must track ApiGatewayV2::Api's choice exactly —
// creating that permission for a service that has no API Gateway would name
// a SourceArn Cloud Control could never resolve.
func TestHTTPFrontDoorRegistrationsAreMutuallyExclusive(t *testing.T) {
	regs := Registrations(&Client{})
	byType := map[string]resource.Registration{}
	for _, r := range regs {
		byType[r.Type] = r
	}
	url, httpAPI, apiPermission := byType[TypeLambdaURL], byType[TypeAPIGatewayV2API], byType[TypePermissionAPIGateway]

	cases := []struct {
		name         string
		settings     map[string]any
		wantURL      bool
		wantAPI      bool
		wantAPIPerms bool
	}{
		{"unset settings default to apigateway", nil, false, true, true},
		{"empty settings default to apigateway", map[string]any{}, false, true, true},
		{"explicit apigateway", map[string]any{"httpFrontDoor": "apigateway"}, false, true, true},
		{"explicit url", map[string]any{"httpFrontDoor": "url"}, true, false, false},
	}
	// Every case fixes the trigger at HTTP, so what varies is the settings
	// alone — the dimension this test is about. The nil case is what proves
	// a settings condition is consulted rather than waved through when the
	// map is nil: waving it through would make both front doors apply at
	// once, which is precisely what this test exists to forbid.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := resource.ApplicabilityContext{
				Trigger:  manifest.TriggerHTTP,
				Settings: c.settings,
			}
			if got := url.Matches(ctx); got != c.wantURL {
				t.Errorf("TypeLambdaURL.Matches(settings=%v) = %v, want %v", c.settings, got, c.wantURL)
			}
			if got := httpAPI.Matches(ctx); got != c.wantAPI {
				t.Errorf("TypeAPIGatewayV2API.Matches(settings=%v) = %v, want %v", c.settings, got, c.wantAPI)
			}
			if got := apiPermission.Matches(ctx); got != c.wantAPIPerms {
				t.Errorf("TypePermissionAPIGateway.Matches(settings=%v) = %v, want %v", c.settings, got, c.wantAPIPerms)
			}
			if url.Matches(ctx) && httpAPI.Matches(ctx) {
				t.Fatalf("both TypeLambdaURL and TypeAPIGatewayV2API select for settings=%v — a service would get two HTTP front doors", c.settings)
			}
		})
	}
}

func TestRegisterPropagatesADuplicateRegistrationError(t *testing.T) {
	// Registry.Register already rejects a duplicate provider/type key
	// (tested in internal/resource); this proves Register's own loop
	// surfaces that failure to its caller instead of swallowing it after
	// registering some but not all four types.
	reg := resource.NewRegistry()
	if err := reg.Register(Registrations(&Client{})[0]); err != nil {
		t.Fatalf("seeding a conflicting registration: %v", err)
	}

	if err := Register(reg, &Client{}); err == nil {
		t.Fatal("expected the duplicate S3 bucket registration to be reported")
	}
}

// TestRegisterExpandsCapabilitiesToEveryType checks that both capabilities
// this package registers under resolve to the complete, expected set of
// types. Execution order is no longer Resolve's concern — Resolve returns
// registration order (see Registry.Resolve's own doc comment) and real
// ordering is computed from DependsOn by internal/plan/graph.go, which has
// its own dedicated wave-assignment tests against this exact topology
// (internal/plan/aws_topology_test.go).
func TestRegisterExpandsCapabilitiesToEveryType(t *testing.T) {
	// Mirrors neonresource's own capability-to-multiple-types pairing: one
	// capability, several AWS types.
	reg := resource.NewRegistry()
	if err := Register(reg, &Client{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	objects, err := reg.Resolve(manifest.CapabilityObjects, resource.ApplicabilityContext{Vendors: map[string]string{manifest.CapabilityObjects: Provider}})
	if err != nil {
		t.Fatalf("Resolve objects: %v", err)
	}
	wantObjects := map[string]bool{
		TypeRoute53HostedZone: true, TypeCertificateManagerCertificate: true, TypeS3Bucket: true,
		TypeCloudFrontDistribution: true, TypeRoute53RecordSet: true,
	}
	if len(objects) != len(wantObjects) {
		t.Fatalf("objects = %+v, want %d entries", objects, len(wantObjects))
	}
	for _, r := range objects {
		if !wantObjects[r.Type] {
			t.Fatalf("objects contains unexpected type %q (full: %+v)", r.Type, objects)
		}
	}

	// Compute takes two resolves, not one, because Resolve answers "what
	// applies to this service" rather than "what is registered": the two HTTP
	// front doors are mutually exclusive, so no single service ever sees both
	// and no single context can return all eight. The union across the only
	// two front-door choices there are is what "every type is reachable"
	// means — every registered type is reachable from some real manifest,
	// which is the property this test is actually for.
	//
	// The trigger is left unset, which satisfies every trigger condition, so
	// what varies here is the front door alone.
	computeVendors := map[string]string{manifest.CapabilityCompute: Provider}
	got := map[string]bool{}
	for _, frontDoor := range []string{httpFrontDoorAPIGateway, httpFrontDoorURL} {
		compute, err := reg.Resolve(manifest.CapabilityCompute, resource.ApplicabilityContext{
			Vendors:  computeVendors,
			Settings: map[string]any{"httpFrontDoor": frontDoor},
		})
		if err != nil {
			t.Fatalf("Resolve compute (httpFrontDoor=%s): %v", frontDoor, err)
		}
		for _, r := range compute {
			got[r.Type] = true
		}
	}

	wantCompute := map[string]bool{
		TypeArtifactBucket: true, TypeIAMRole: true, TypeLambdaFunction: true, TypeLambdaURL: true,
		TypeEventsRule: true, TypePermissionEventsRule: true, TypeAPIGatewayV2API: true, TypePermissionAPIGateway: true,
	}
	if !reflect.DeepEqual(got, wantCompute) {
		t.Fatalf("compute types reachable across both front doors = %v, want %v", got, wantCompute)
	}
}

// TestRegistrationsGetThroughTheRegistry is a light end-to-end check that the
// registry's Resource is actually this package's resourceType wired to the
// client passed to Register — not just metadata.
func TestRegistrationsGetThroughTheRegistry(t *testing.T) {
	fc := &fakeClient{byIdentifier: map[string]map[string]any{
		"my-function": {"FunctionName": "my-function"},
	}}
	regs := Registrations(nil)
	for i := range regs {
		if regs[i].Type == TypeLambdaFunction {
			regs[i].Resource = &resourceType{provider: Provider, typeName: TypeLambdaFunction, lookup: resource.LookupByName, client: fc}
		}
	}
	reg := resource.NewRegistry()
	for _, r := range regs {
		if err := reg.Register(r); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	got, ok := reg.Lookup(Provider + "/" + TypeLambdaFunction)
	if !ok {
		t.Fatal("lambda function not registered")
	}
	state, err := got.Resource.Get(context.Background(), resource.Ref{Name: "my-function"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil || state.ID != "my-function" {
		t.Fatalf("state = %+v", state)
	}
}

// TestPermissionRegistrationsDeclareAListScope pins the fix for a real
// `kraai plan` failure against a live account: both TypePermissionEventsRule
// and TypePermissionAPIGateway drive AWS::Lambda::Permission, whose Cloud
// Control list handler is parent-scoped and rejects an unscoped
// ListResources call outright (see Client.ListResources's own doc comment
// for the real API error). Both registrations must wire a listScope that
// resolves to the invoking function's own derived name, not leave
// resourceType's engine to send the unscoped request that originally
// failed.
func TestPermissionRegistrationsDeclareAListScope(t *testing.T) {
	regs := Registrations(&Client{})
	for _, typeName := range []string{TypePermissionEventsRule, TypePermissionAPIGateway} {
		t.Run(typeName, func(t *testing.T) {
			var reg *resource.Registration
			for i := range regs {
				if regs[i].Type == typeName {
					reg = &regs[i]
				}
			}
			if reg == nil {
				t.Fatalf("no registration found for %s", typeName)
			}
			perm, ok := reg.Resource.(*lambdaPermissionResource)
			if !ok {
				t.Fatalf("Resource = %T, want *lambdaPermissionResource", reg.Resource)
			}
			if perm.inner.listScope == nil {
				t.Fatal("listScope is nil — this registration would send an unscoped ListResources request, exactly the failure this fix closes")
			}
			model, err := perm.inner.listScope("myenv-api")
			if err != nil {
				t.Fatalf("listScope: %v", err)
			}
			want := map[string]any{"FunctionName": "myenv-api"}
			if !reflect.DeepEqual(model, want) {
				t.Fatalf("listScope(%q) = %+v, want %+v", "myenv-api", model, want)
			}
		})
	}
}
