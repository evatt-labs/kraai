package aws

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

func TestResourceTypeGetByName(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		fc := &fakeClient{byIdentifier: map[string]map[string]any{
			"my-bucket": {"BucketName": "my-bucket"},
		}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		state, err := r.Get(context.Background(), resource.Ref{Name: "my-bucket"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if state == nil || state.ID != "my-bucket" || state.Attributes["BucketName"] != "my-bucket" {
			t.Fatalf("state = %+v", state)
		}
		// No ListResources call for a byName type: the derived name already
		// is the primary identifier.
		if fc.listCalls != 0 {
			t.Fatalf("listCalls = %d, want 0", fc.listCalls)
		}
	})

	t.Run("absent is (nil, nil), never an error", func(t *testing.T) {
		fc := &fakeClient{byIdentifier: map[string]map[string]any{}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		state, err := r.Get(context.Background(), resource.Ref{Name: "missing"})
		if err != nil || state != nil {
			t.Fatalf("state=%v err=%v, want (nil, nil)", state, err)
		}
	})

	t.Run("an API failure is never mistaken for absence", func(t *testing.T) {
		// This is the non-negotiable bar: teardown reads Get's (nil, nil) as
		// "already deleted, keep going". Misreading an outage this way would
		// orphan a real resource.
		fc := &fakeClient{getErr: map[string]error{"x": errors.New("throttled")}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		state, err := r.Get(context.Background(), resource.Ref{Name: "x"})
		if err == nil {
			t.Fatal("expected the failure to be reported")
		}
		if state != nil {
			t.Fatalf("state = %v, want nil alongside the error", state)
		}
	})
}

func TestResourceTypeGetByAttr(t *testing.T) {
	t.Run("finds the matching candidate and reuses its properties", func(t *testing.T) {
		fc := &fakeClient{
			list: []string{"cand-1", "cand-2"},
			byIdentifier: map[string]map[string]any{
				"cand-1": {"Name": "other"},
				"cand-2": {"Name": "target"},
			},
		}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc, match: matchNameField}

		state, err := r.Get(context.Background(), resource.Ref{Name: "target"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if state == nil || state.ID != "cand-2" {
			t.Fatalf("state = %+v", state)
		}
		// Exactly one GetResource per candidate up to and including the
		// match — never a second call for the identifier Get already has
		// properties for.
		if len(fc.getCalls) != 2 {
			t.Fatalf("getCalls = %v, want 2 (one per candidate, no re-fetch)", fc.getCalls)
		}
	})

	t.Run("no candidate matches is absence, not an error", func(t *testing.T) {
		fc := &fakeClient{
			list:         []string{"cand-1"},
			byIdentifier: map[string]map[string]any{"cand-1": {"Name": "other"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc, match: matchNameField}

		state, err := r.Get(context.Background(), resource.Ref{Name: "target"})
		if err != nil || state != nil {
			t.Fatalf("state=%v err=%v, want (nil, nil)", state, err)
		}
	})

	t.Run("a candidate deleted between list and get is skipped, not fatal", func(t *testing.T) {
		fc := &fakeClient{
			list:         []string{"vanished", "cand-2"},
			byIdentifier: map[string]map[string]any{"cand-2": {"Name": "target"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc, match: matchNameField}

		state, err := r.Get(context.Background(), resource.Ref{Name: "target"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if state == nil || state.ID != "cand-2" {
			t.Fatalf("state = %+v, want the surviving candidate", state)
		}
	})

	t.Run("ListResources failing is reported, not treated as no candidates", func(t *testing.T) {
		fc := &fakeClient{listErr: errors.New("throttled")}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc, match: matchNameField}

		if _, err := r.Get(context.Background(), resource.Ref{Name: "target"}); err == nil {
			t.Fatal("expected the ListResources failure to be reported")
		}
	})

	t.Run("GetResource failing on a candidate is reported, not skipped", func(t *testing.T) {
		fc := &fakeClient{
			list:   []string{"cand-1"},
			getErr: map[string]error{"cand-1": errors.New("throttled")},
		}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc, match: matchNameField}

		if _, err := r.Get(context.Background(), resource.Ref{Name: "target"}); err == nil {
			t.Fatal("expected the GetResource failure to be reported")
		}
	})
}

// TestResourceTypeResolveListScope covers resolve's parent-scoped list
// path (resourceType.listScope): a type that declares one must carry the
// scope's resource model into ListResources; a type that does not must
// keep sending an unscoped request exactly as before this mechanism
// existed; and a listScope that cannot populate itself must refuse loudly
// rather than fall back to an unscoped call.
func TestResourceTypeResolveListScope(t *testing.T) {
	t.Run("a parent-scoped type's list request carries the declared resource model", func(t *testing.T) {
		fc := &fakeClient{
			list:         []string{"perm1"},
			byIdentifier: map[string]map[string]any{"perm1": {"FunctionName": "myenv-api"}},
		}
		r := &resourceType{
			provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: fc, match: lambdaPermissionMatch, listScope: lambdaPermissionListScope,
		}

		state, err := r.Get(context.Background(), resource.Ref{Name: "myenv-api"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if state == nil || state.ID != "perm1" {
			t.Fatalf("state = %+v", state)
		}
		if len(fc.listModels) != 1 {
			t.Fatalf("listModels = %+v, want exactly one ListResources call", fc.listModels)
		}
		want := map[string]any{"FunctionName": "myenv-api"}
		if !reflect.DeepEqual(fc.listModels[0], want) {
			t.Fatalf("resourceModel = %+v, want %+v", fc.listModels[0], want)
		}
	})

	t.Run("a non-parent-scoped type's list request carries no resource model", func(t *testing.T) {
		fc := &fakeClient{
			list:         []string{"cand-1"},
			byIdentifier: map[string]map[string]any{"cand-1": {"Name": "target"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc, match: matchNameField}

		if _, err := r.Get(context.Background(), resource.Ref{Name: "target"}); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(fc.listModels) != 1 || fc.listModels[0] != nil {
			t.Fatalf("listModels = %+v, want a single nil entry — no extra API call shape for a type with no listScope", fc.listModels)
		}
	})

	t.Run("a listScope that errors is reported, never silently sent unscoped", func(t *testing.T) {
		fc := &fakeClient{list: []string{"perm1"}}
		wantErr := errors.New("cannot resolve parent")
		r := &resourceType{
			provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: fc, match: lambdaPermissionMatch,
			listScope: func(string) (map[string]any, error) { return nil, wantErr },
		}

		if _, err := r.Get(context.Background(), resource.Ref{Name: "myenv-api"}); !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
		if fc.listCalls != 0 {
			t.Fatalf("listCalls = %d, want 0 — a failed listScope must never reach ListResources", fc.listCalls)
		}
	})

	t.Run("a listScope that produces an empty model refuses rather than falling back to unscoped", func(t *testing.T) {
		fc := &fakeClient{list: []string{"perm1"}}
		r := &resourceType{
			provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: fc, match: lambdaPermissionMatch,
			listScope: func(string) (map[string]any, error) { return map[string]any{}, nil },
		}

		_, err := r.Get(context.Background(), resource.Ref{Name: "myenv-api"})
		if err == nil {
			t.Fatal("expected an empty resource model from a declared listScope to be refused")
		}
		if fc.listCalls != 0 {
			t.Fatalf("listCalls = %d, want 0 — refusing must happen before ListResources is ever called", fc.listCalls)
		}
	})

	t.Run("lambdaPermissionListScope itself refuses an empty name", func(t *testing.T) {
		if _, err := lambdaPermissionListScope(""); err == nil {
			t.Fatal("expected an empty derived name to be refused")
		}
	})
}

// The engine's ownership hook: what resolve finds is not necessarily ours.
// Set by the two registrations that need it (the objects bucket, the hosted
// zone); nil for every type whose lookup is already account-scoped.
func TestResourceTypeOwnership(t *testing.T) {
	deny := func(context.Context, string, map[string]any) (bool, error) { return false, nil }
	allow := func(context.Context, string, map[string]any) (bool, error) { return true, nil }
	broken := func(context.Context, string, map[string]any) (bool, error) { return false, errors.New("list denied") }

	t.Run("Get reports an unowned instance as absent", func(t *testing.T) {
		fc := &fakeClient{byIdentifier: map[string]map[string]any{"my-bucket": {"BucketName": "my-bucket"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc, owns: deny}

		state, err := r.Get(context.Background(), resource.Ref{Name: "my-bucket"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if state != nil {
			t.Fatalf("Get = %+v, want nil for an instance this account does not own", state)
		}
	})

	t.Run("Get reports an owned instance", func(t *testing.T) {
		fc := &fakeClient{byIdentifier: map[string]map[string]any{"my-bucket": {"BucketName": "my-bucket"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc, owns: allow}

		state, err := r.Get(context.Background(), resource.Ref{Name: "my-bucket"})
		if err != nil || state == nil {
			t.Fatalf("Get = %+v, %v, want the owned instance", state, err)
		}
	})

	t.Run("a hook failure is an error, never silently absent", func(t *testing.T) {
		fc := &fakeClient{byIdentifier: map[string]map[string]any{"my-bucket": {}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc, owns: broken}

		if _, err := r.Get(context.Background(), resource.Ref{Name: "my-bucket"}); err == nil {
			t.Fatal("a check that could not run answered 'absent'")
		}
		if err := r.Delete(context.Background(), resource.Ref{Name: "my-bucket"}); err == nil {
			t.Fatal("a check that could not run permitted a delete")
		}
	})

	t.Run("an import is never asked", func(t *testing.T) {
		// Adoption is the manifest asserting ownership by hand; asking the
		// hook would refuse exactly the resource the import exists to reach.
		fc := &fakeClient{byIdentifier: map[string]map[string]any{"Z123": {"Name": "acme.example."}}}
		r := &resourceType{provider: Provider, typeName: TypeRoute53HostedZone, lookup: resource.LookupByAPI, client: fc,
			match: hostedZoneMatch, owns: broken}

		state, err := r.Get(context.Background(), resource.Ref{Name: "acme.example", Import: &resource.Import{ID: "Z123"}})
		if err != nil || state == nil {
			t.Fatalf("Get = %+v, %v, want the imported instance without consulting the hook", state, err)
		}
	})

	t.Run("Delete skips an unowned instance without calling DeleteResource", func(t *testing.T) {
		fc := &fakeClient{byIdentifier: map[string]map[string]any{"my-bucket": {"BucketName": "my-bucket"}}}
		var sawProps map[string]any
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc,
			owns: func(_ context.Context, _ string, props map[string]any) (bool, error) {
				sawProps = props
				return false, nil
			}}

		if err := r.Delete(context.Background(), resource.Ref{Name: "my-bucket"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(fc.deleteCalls) != 0 {
			t.Fatalf("deleteCalls = %v, want none for an instance this account does not own", fc.deleteCalls)
		}
		// byName resolves without reading; Delete reads before asking, so a
		// hook that needs the properties (the hosted zone's tags) has them.
		if sawProps == nil {
			t.Fatal("the hook was asked without the instance's properties")
		}
	})

	t.Run("Delete proceeds for an owned instance", func(t *testing.T) {
		fc := &fakeClient{byIdentifier: map[string]map[string]any{"my-bucket": {}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc, owns: allow}

		if err := r.Delete(context.Background(), resource.Ref{Name: "my-bucket"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(fc.deleteCalls) != 1 {
			t.Fatalf("deleteCalls = %v, want one", fc.deleteCalls)
		}
	})

	t.Run("Create stamps a tag for a type found another way", func(t *testing.T) {
		// Not byTag — the lookup is by name — but the tag is how owns later
		// tells what kraai made from what it did not.
		fc := &fakeClient{createID: "Z1", createProps: map[string]any{}}
		r := &resourceType{provider: Provider, typeName: TypeRoute53HostedZone, lookup: resource.LookupByAPI, client: fc,
			match: hostedZoneMatch, stampTag: hostedZoneStampTag}

		if _, err := r.Create(context.Background(), resource.Spec{Name: "acme.example", Config: map[string]any{"Name": "acme.example"}}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if !arrayTagsMatchIn(fc.createCalls[0], hostedZoneTagsProperty, "acme.example") {
			t.Fatalf("desired = %v, want the identity tag under %s", fc.createCalls[0], hostedZoneTagsProperty)
		}
	})
}

// emptySchemaCF is a CloudFormation client whose every DescribeType answers
// with a schema declaring nothing — for a test that drives a real *Client
// through a lookup, which consults the type's schema before listing.
func emptySchemaCF() *fakeCF {
	return &fakeCF{out: &cloudformation.DescribeTypeOutput{Schema: aws.String("{}")}}
}

// The check that would have prevented the Lambda::Permission outage
// (evatt-labs/kraai#132): a list request is held to what the type's own
// schema says its list handler requires, before it is sent.
func TestResourceTypeListScopeIsCheckedAgainstTheSchema(t *testing.T) {
	permission := cfschema.Facts{ListScope: [][]string{{"FunctionName"}}}
	record := cfschema.Facts{ListScope: [][]string{{"HostedZoneId"}, {"HostedZoneName"}}}

	t.Run("no listScope on a type whose handler requires one is refused before the call", func(t *testing.T) {
		fc := &fakeClient{schema: permission, list: []string{"perm1"}}
		r := &resourceType{provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: fc, match: lambdaPermissionMatch}

		_, err := r.Get(context.Background(), resource.Ref{Name: "myenv-api"})
		if err == nil {
			t.Fatal("an unscoped list of a parent-scoped type was sent")
		}
		for _, want := range []string{"FunctionName", "declares no listScope"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q: %v", want, err)
			}
		}
		if fc.listCalls != 0 {
			t.Fatalf("listCalls = %d, want 0", fc.listCalls)
		}
	})

	t.Run("a listScope supplying what the handler requires proceeds", func(t *testing.T) {
		fc := &fakeClient{schema: permission, list: []string{"perm1"},
			byIdentifier: map[string]map[string]any{"perm1": {"FunctionName": "myenv-api"}}}
		r := &resourceType{provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: fc, match: lambdaPermissionMatch, listScope: lambdaPermissionListScope}

		if state, err := r.Get(context.Background(), resource.Ref{Name: "myenv-api"}); err != nil || state == nil {
			t.Fatalf("Get = %+v, %v", state, err)
		}
	})

	t.Run("a listScope supplying the wrong property is refused, naming both", func(t *testing.T) {
		fc := &fakeClient{schema: permission, list: []string{"perm1"}}
		r := &resourceType{provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: fc, match: lambdaPermissionMatch,
			listScope: func(string) (map[string]any, error) { return map[string]any{"Function": "x"}, nil }}

		_, err := r.Get(context.Background(), resource.Ref{Name: "myenv-api"})
		if err == nil || !strings.Contains(err.Error(), "FunctionName") || !strings.Contains(err.Error(), "[Function]") {
			t.Fatalf("err = %v, want the required and the supplied properties named", err)
		}
		if fc.listCalls != 0 {
			t.Fatalf("listCalls = %d, want 0", fc.listCalls)
		}
	})

	t.Run("any one alternative satisfies a oneOf", func(t *testing.T) {
		fc := &fakeClient{schema: record, list: []string{}}
		r := &resourceType{provider: Provider, typeName: TypeRoute53RecordSet, lookup: resource.LookupByAttr,
			client: fc, match: recordSetMatch, listScope: recordSetListScope}

		if _, err := r.Get(context.Background(), resource.Ref{Name: "acme.example"}); err != nil {
			t.Fatalf("Get: %v — HostedZoneName is one of the accepted alternatives", err)
		}
	})

	t.Run("a handler that requires nothing needs no scope", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{}, list: []string{}}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByTag,
			client: fc, match: arrayTagsMatch}

		if _, err := r.Get(context.Background(), resource.Ref{Name: "x"}); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if fc.listCalls != 1 {
			t.Fatalf("listCalls = %d, want 1", fc.listCalls)
		}
	})

	t.Run("a schema that cannot be fetched fails the lookup, never sends unscoped", func(t *testing.T) {
		fc := &fakeClient{schemaErr: errors.New("throttled"), list: []string{}}
		r := &resourceType{provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: fc, match: lambdaPermissionMatch, listScope: lambdaPermissionListScope}

		if _, err := r.Get(context.Background(), resource.Ref{Name: "myenv-api"}); err == nil {
			t.Fatal("a lookup proceeded with no schema to check against")
		}
		if fc.listCalls != 0 {
			t.Fatalf("listCalls = %d, want 0", fc.listCalls)
		}
	})
}
