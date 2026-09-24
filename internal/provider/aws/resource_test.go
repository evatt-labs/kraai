package aws

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// fakeClient is ccAPI, hand-rolled — no AWS account, no network needed.
type fakeClient struct {
	// byIdentifier answers GetResource. A missing key means "not found",
	// distinct from an entry mapping to an error.
	byIdentifier map[string]map[string]any
	getErr       map[string]error
	list         []string
	// listByType, when set for a type, answers ListResources for it instead
	// of list: Cloud Control lists per type, and a test resolving two types
	// through one fake needs each to see only its own identifiers.
	listByType map[string][]string
	listErr    error
	getCalls   []string
	listCalls  int
	// listModels records the resourceModel passed to every ListResources
	// call, in order, so a test can assert a parent-scoped type's request
	// actually carried the right scope (or that a non-parent-scoped type's
	// request carried none at all).
	listModels []map[string]any

	createID    string
	createProps map[string]any
	createErr   error
	createCalls []map[string]any
	// createOrder records the typeName of every CreateResource call, in
	// order, so a test can assert two types were created in the right
	// sequence (cloudfront_test.go's OAC-before-distribution).
	createOrder []string
	// createResults, keyed by typeName, overrides createID/createProps/
	// createErr for a call against that specific type — needed once a
	// single fakeClient creates more than one type in the same test (the
	// CloudFront composite resource creates both an OriginAccessControl
	// and a Distribution through the same client). A type with no entry
	// here falls back to createID/createProps/createErr above, which is
	// what every existing, single-type test in this file already relies
	// on.
	createResults map[string]struct {
		id    string
		props map[string]any
		err   error
	}

	updateProps   map[string]any
	updateErr     error
	updateCalls   []string
	updatePatches [][]byte

	deleteErr   error
	deleteCalls []string

	schema      cfschema.Facts
	schemaErr   error
	schemaCalls int

	// tagged answers TaggedResources by tag value; taggedErr fails it.
	tagged      map[string][]string
	taggedErr   error
	taggedCalls int
	taggedTypes []string
}

func (f *fakeClient) TaggedResources(_ context.Context, name, tagType string) ([]string, error) {
	f.taggedCalls++
	f.taggedTypes = append(f.taggedTypes, tagType)
	if f.taggedErr != nil {
		return nil, f.taggedErr
	}
	return f.tagged[name], nil
}

func (f *fakeClient) GetResource(_ context.Context, _ string, identifier string) (map[string]any, bool, error) {
	f.getCalls = append(f.getCalls, identifier)
	if err, ok := f.getErr[identifier]; ok {
		return nil, false, err
	}
	props, ok := f.byIdentifier[identifier]
	if !ok {
		return nil, false, nil
	}
	return props, true, nil
}

func (f *fakeClient) ListResources(_ context.Context, typeName string, resourceModel map[string]any) ([]string, error) {
	f.listCalls++
	f.listModels = append(f.listModels, resourceModel)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if ids, ok := f.listByType[typeName]; ok {
		return ids, nil
	}
	return f.list, nil
}

func (f *fakeClient) CreateResource(_ context.Context, typeName string, desiredState map[string]any) (string, map[string]any, error) {
	f.createCalls = append(f.createCalls, desiredState)
	f.createOrder = append(f.createOrder, typeName)
	if result, ok := f.createResults[typeName]; ok {
		if result.err != nil {
			return "", nil, result.err
		}
		return result.id, result.props, nil
	}
	if f.createErr != nil {
		return "", nil, f.createErr
	}
	return f.createID, f.createProps, nil
}

func (f *fakeClient) UpdateResource(_ context.Context, _ string, identifier string, patch []byte) (map[string]any, error) {
	f.updateCalls = append(f.updateCalls, identifier)
	f.updatePatches = append(f.updatePatches, patch)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updateProps, nil
}

func (f *fakeClient) DeleteResource(_ context.Context, _ string, identifier string) error {
	f.deleteCalls = append(f.deleteCalls, identifier)
	return f.deleteErr
}

func (f *fakeClient) DescribeType(context.Context, string) (cfschema.Facts, error) {
	f.schemaCalls++
	if f.schemaErr != nil {
		return cfschema.Facts{}, f.schemaErr
	}
	return f.schema, nil
}

func matchNameField(properties map[string]any, name string) bool {
	v, _ := properties["Name"].(string)
	return v == name
}

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

func TestResourceTypeCreate(t *testing.T) {
	t.Run("submits Config as desired state and returns the created identity", func(t *testing.T) {
		fc := &fakeClient{
			createID: "my-bucket", createProps: map[string]any{"BucketName": "my-bucket"},
			schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/BucketName"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		state, err := r.Create(context.Background(), resource.Spec{Binding: "objects", Name: "my-bucket", Config: map[string]any{"BucketName": "my-bucket"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if state.ID != "my-bucket" || state.Attributes["BucketName"] != "my-bucket" || state.Ref.Name != "my-bucket" {
			t.Fatalf("state = %+v", state)
		}
		if len(fc.createCalls) != 1 || fc.createCalls[0]["BucketName"] != "my-bucket" {
			t.Fatalf("createCalls = %+v", fc.createCalls)
		}
	})

	t.Run("no derived name is a validation error", func(t *testing.T) {
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: &fakeClient{}}

		_, err := r.Create(context.Background(), resource.Spec{Binding: "objects"})
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
			t.Fatalf("err = %v, want a CodeValidation KError", err)
		}
	})

	t.Run("a byTag type stamps its identity tag into the create call itself", func(t *testing.T) {
		// The tag must ride in CreateResource's own desired state, never a
		// follow-up write, since a crash between the two would orphan the
		// resource unfindably.
		fc := &fakeClient{createID: "arn:aws:acm:...", createProps: map[string]any{}}
		r := &resourceType{
			provider: Provider, typeName: TypeCertificateManagerCertificate, lookup: resource.LookupByTag,
			client: fc, match: certificateMatch, stampTag: certificateStampTag,
		}

		_, err := r.Create(context.Background(), resource.Spec{
			Binding: "objects", Name: "my-cert",
			Config: map[string]any{"DomainName": "example.com"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if len(fc.createCalls) != 1 {
			t.Fatalf("createCalls = %+v", fc.createCalls)
		}
		tags, _ := fc.createCalls[0]["Tags"].([]any)
		if len(tags) != 1 {
			t.Fatalf("Tags = %+v, want exactly one entry", tags)
		}
		tag, _ := tags[0].(map[string]any)
		if tag["Key"] != identityTagKey || tag["Value"] != "my-cert" {
			t.Fatalf("tag = %+v", tag)
		}
	})

	t.Run("a byTag type with no stampTag function fails loudly rather than orphaning silently", func(t *testing.T) {
		r := &resourceType{provider: Provider, typeName: TypeCertificateManagerCertificate, lookup: resource.LookupByTag, client: &fakeClient{}}

		_, err := r.Create(context.Background(), resource.Spec{Binding: "objects", Name: "my-cert"})
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
			t.Fatalf("err = %v, want a CodeValidation KError", err)
		}
	})

	t.Run("a CreateResource failure is reported, not swallowed", func(t *testing.T) {
		fc := &fakeClient{
			createErr: errors.New("throttled"),
			schema:    cfschema.Facts{PrimaryIdentifier: []string{"/properties/BucketName"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		_, err := r.Create(context.Background(), resource.Spec{Binding: "objects", Name: "x"})
		if err == nil {
			t.Fatal("expected the failure to be reported")
		}
		// Confirms CreateResource's own failure is what propagates, not
		// injectDerivedName rejecting an incomplete desired state first —
		// the schema above gives it a resolvable BucketName to populate.
		if !strings.Contains(err.Error(), "throttled") {
			t.Fatalf("err = %v, want it to wrap the CreateResource failure", err)
		}
	})
}

// TestResourceTypeCreateInjectsDerivedName covers injectDerivedName: the
// generic fix for the S3 bucket bug (a LookupByName Create whose Config
// carries no identifying property submits an empty-enough desired state
// that Cloud Control silently reinterprets rather than rejects — see
// injectDerivedName's own doc comment for AWS::S3::Bucket's documented
// "generates a unique ID" behavior).
func TestResourceTypeCreateInjectsDerivedName(t *testing.T) {
	t.Run("byName with a Config that omits the identifying property gets it injected from the schema's primary identifier", func(t *testing.T) {
		// This is the regression case: an "objects" capability binding's
		// Spec.Config is nil (internal/plan's expandBinding, out of this
		// workstream's scope) exactly as it is for AWS::S3::Bucket's own
		// bare registration in register.go — see this test file's sibling
		// PR description for the revert-and-fail proof against this exact
		// case.
		fc := &fakeClient{
			createID: "my-bucket", createProps: map[string]any{"BucketName": "my-bucket"},
			schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/BucketName"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		_, err := r.Create(context.Background(), resource.Spec{Binding: "objects", Name: "my-bucket", Config: nil})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if len(fc.createCalls) != 1 {
			t.Fatalf("createCalls = %+v, want exactly 1", fc.createCalls)
		}
		if fc.createCalls[0]["BucketName"] != "my-bucket" {
			t.Fatalf("desired state = %+v, want BucketName injected from the derived name", fc.createCalls[0])
		}
	})

	t.Run("the property name comes from the schema, not a hardcoded table", func(t *testing.T) {
		// Same mechanism, a type whose identifying property is not
		// "BucketName" at all — proves injectDerivedName reads
		// PrimaryIdentifier rather than assuming one fixed key.
		fc := &fakeClient{
			createID: "my-role",
			schema:   cfschema.Facts{PrimaryIdentifier: []string{"/properties/RoleName"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeIAMRole, lookup: resource.LookupByName, client: fc}

		_, err := r.Create(context.Background(), resource.Spec{Binding: "compute", Name: "my-role", Config: nil})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if fc.createCalls[0]["RoleName"] != "my-role" {
			t.Fatalf("desired state = %+v, want RoleName injected", fc.createCalls[0])
		}
	})

	t.Run("a value the caller already set for the identifying property is never overwritten", func(t *testing.T) {
		// artifactbucket.go's own Create rewrites Config to a real bucket
		// name that difference from spec.Name (the derived service name plus
		// an "-artifacts" suffix) — injectDerivedName must respect that
		// deliberate choice rather than clobbering it with spec.Name.
		fc := &fakeClient{
			createID: "custom-name",
			schema:   cfschema.Facts{PrimaryIdentifier: []string{"/properties/BucketName"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		_, err := r.Create(context.Background(), resource.Spec{
			Binding: "objects", Name: "derived-name",
			Config: map[string]any{"BucketName": "custom-name"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if fc.createCalls[0]["BucketName"] != "custom-name" {
			t.Fatalf("BucketName = %v, want the caller-set value preserved, not spec.Name", fc.createCalls[0]["BucketName"])
		}
	})

	t.Run("a compound primary identifier is a loud error, never a guess", func(t *testing.T) {
		// AWS::Route53::RecordSet's real shape: no single property is "the
		// name" — see this method's own doc comment on why guessing one
		// would just relocate the bug rather than close it.
		fc := &fakeClient{
			schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/HostedZoneId", "/properties/Name", "/properties/Type"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeRoute53RecordSet, lookup: resource.LookupByName, client: fc}

		_, err := r.Create(context.Background(), resource.Spec{Binding: "objects", Name: "www.example.com"})
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
			t.Fatalf("err = %v, want a CodeValidation KError", err)
		}
		if !strings.Contains(err.Error(), TypeRoute53RecordSet) {
			t.Fatalf("err = %v, want it to name the offending type", err)
		}
		if len(fc.createCalls) != 0 {
			t.Fatal("expected CreateResource to never be called for an unresolvable identifier")
		}
	})

	t.Run("an empty primary identifier is a loud error, never an empty desired state", func(t *testing.T) {
		// The exact shape a schema with no decoded PrimaryIdentifier at all
		// takes (a genuinely unknown identifier, or a DescribeType response
		// this package cannot interpret) — must refuse just as loudly as
		// the compound case, not fall through to submitting Config as-is.
		fc := &fakeClient{schema: cfschema.Facts{}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		_, err := r.Create(context.Background(), resource.Spec{Binding: "objects", Name: "my-bucket"})
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
			t.Fatalf("err = %v, want a CodeValidation KError", err)
		}
		if len(fc.createCalls) != 0 {
			t.Fatal("expected CreateResource to never be called for an unresolvable identifier")
		}
	})

	t.Run("a DescribeType failure during Create is reported, not swallowed", func(t *testing.T) {
		fc := &fakeClient{schemaErr: errors.New("throttled")}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		if _, err := r.Create(context.Background(), resource.Spec{Binding: "objects", Name: "x"}); err == nil {
			t.Fatal("expected the schema-fetch failure to be reported")
		}
		if len(fc.createCalls) != 0 {
			t.Fatal("expected CreateResource to never be called when the schema fetch fails")
		}
	})

	t.Run("byTag never fetches a schema for identity: stampTag already covers it", func(t *testing.T) {
		fc := &fakeClient{createID: "arn:aws:acm:...", createProps: map[string]any{}}
		r := &resourceType{
			provider: Provider, typeName: TypeCertificateManagerCertificate, lookup: resource.LookupByTag,
			client: fc, match: certificateMatch, stampTag: certificateStampTag,
		}

		_, err := r.Create(context.Background(), resource.Spec{
			Binding: "objects", Name: "my-cert",
			Config: map[string]any{"DomainName": "example.com"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if fc.schemaCalls != 0 {
			t.Fatalf("schemaCalls = %d, want 0: a byTag type has no reason to consult the schema for identity", fc.schemaCalls)
		}
	})

	t.Run("byAttr never fetches a schema for identity: the provider assigns the identifier", func(t *testing.T) {
		fc := &fakeClient{createID: "E123", createProps: map[string]any{}}
		r := &resourceType{
			provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr,
			client: fc, match: matchNameField,
		}

		_, err := r.Create(context.Background(), resource.Spec{
			Binding: "objects", Name: "my-distribution",
			Config: map[string]any{"DistributionConfig": map[string]any{}},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if fc.schemaCalls != 0 {
			t.Fatalf("schemaCalls = %d, want 0", fc.schemaCalls)
		}
	})
}

func TestResourceTypeUpdate(t *testing.T) {
	t.Run("diffs current against desired and submits a patch", func(t *testing.T) {
		fc := &fakeClient{
			schema:       cfschema.Facts{HasUpdate: true},
			byIdentifier: map[string]map[string]any{"my-bucket": {"BucketName": "my-bucket", "VersioningConfiguration": map[string]any{"Status": "Suspended"}}},
			updateProps:  map[string]any{"BucketName": "my-bucket", "VersioningConfiguration": map[string]any{"Status": "Enabled"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		state, err := r.Update(context.Background(), resource.Ref{Name: "my-bucket"}, resource.Spec{
			Binding: "objects", Name: "my-bucket",
			Config: map[string]any{"BucketName": "my-bucket", "VersioningConfiguration": map[string]any{"Status": "Enabled"}},
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if state.Attributes["VersioningConfiguration"].(map[string]any)["Status"] != "Enabled" {
			t.Fatalf("state = %+v", state)
		}
		if len(fc.updateCalls) != 1 || fc.updateCalls[0] != "my-bucket" {
			t.Fatalf("updateCalls = %v", fc.updateCalls)
		}
		var ops []patchOp
		if err := json.Unmarshal(fc.updatePatches[0], &ops); err != nil {
			t.Fatalf("decoding submitted patch: %v", err)
		}
		if len(ops) != 1 || ops[0].Op != "replace" || ops[0].Path != "/VersioningConfiguration" {
			t.Fatalf("ops = %+v", ops)
		}
	})

	t.Run("no schema update handler refuses with ErrImmutable rather than attempting the call", func(t *testing.T) {
		fc := &fakeClient{
			schema:       cfschema.Facts{},
			byIdentifier: map[string]map[string]any{"my-cert": {"DomainName": "example.com"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeCertificateManagerCertificate, lookup: resource.LookupByName, client: fc}

		_, err := r.Update(context.Background(), resource.Ref{Name: "my-cert"}, resource.Spec{Config: map[string]any{"DomainName": "other.example.com"}})
		if !errors.Is(err, resource.ErrImmutable) {
			t.Fatalf("err = %v, want it to wrap resource.ErrImmutable", err)
		}
		if len(fc.updateCalls) != 0 {
			t.Fatal("expected no UpdateResource call to have been attempted")
		}
	})

	t.Run("no differing properties is a no-op that still returns fresh state", func(t *testing.T) {
		fc := &fakeClient{
			schema:       cfschema.Facts{HasUpdate: true},
			byIdentifier: map[string]map[string]any{"my-bucket": {"BucketName": "my-bucket"}},
		}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		state, err := r.Update(context.Background(), resource.Ref{Name: "my-bucket"}, resource.Spec{Config: map[string]any{"BucketName": "my-bucket"}})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if state.ID != "my-bucket" {
			t.Fatalf("state = %+v", state)
		}
		if len(fc.updateCalls) != 0 {
			t.Fatal("expected no UpdateResource call when nothing difference")
		}
	})

	t.Run("updating a resource that does not exist is a validation error", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{HasUpdate: true}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		_, err := r.Update(context.Background(), resource.Ref{Name: "missing"}, resource.Spec{Config: map[string]any{}})
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
			t.Fatalf("err = %v, want a CodeValidation KError", err)
		}
	})

	t.Run("a DescribeType failure is reported, not swallowed", func(t *testing.T) {
		fc := &fakeClient{schemaErr: errors.New("throttled")}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		if _, err := r.Update(context.Background(), resource.Ref{Name: "x"}, resource.Spec{}); err == nil {
			t.Fatal("expected the schema-fetch failure to be reported")
		}
	})

	t.Run("byAttr resolves the candidate before fetching properties for the diff", func(t *testing.T) {
		fc := &fakeClient{
			schema: cfschema.Facts{HasUpdate: true},
			list:   []string{"cand-1"},
			byIdentifier: map[string]map[string]any{
				"cand-1": {"Name": "target", "Comment": "old"},
			},
			updateProps: map[string]any{"Name": "target", "Comment": "new"},
		}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc, match: matchNameField}

		state, err := r.Update(context.Background(), resource.Ref{Name: "target"}, resource.Spec{Config: map[string]any{"Name": "target", "Comment": "new"}})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if state.ID != "cand-1" {
			t.Fatalf("state = %+v", state)
		}
	})
}

func TestResourceTypeDelete(t *testing.T) {
	t.Run("resolves then deletes", func(t *testing.T) {
		fc := &fakeClient{byIdentifier: map[string]map[string]any{"my-bucket": {}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		if err := r.Delete(context.Background(), resource.Ref{Name: "my-bucket"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(fc.deleteCalls) != 1 || fc.deleteCalls[0] != "my-bucket" {
			t.Fatalf("deleteCalls = %v", fc.deleteCalls)
		}
	})

	t.Run("byName: nothing to resolve against is success without calling DeleteResource", func(t *testing.T) {
		// LookupByName has no existence check of its own (resolve returns
		// found=true unconditionally for a derived identifier) — this
		// documents that Delete relies on the client's own absence-is-success
		// contract in that case, exercised by the byAttr case below instead.
		fc := &fakeClient{}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		if err := r.Delete(context.Background(), resource.Ref{Name: "never-existed"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(fc.deleteCalls) != 1 {
			t.Fatalf("deleteCalls = %v, want the byName fast path to still call DeleteResource", fc.deleteCalls)
		}
	})

	t.Run("byAttr: no candidate found is success without calling DeleteResource", func(t *testing.T) {
		fc := &fakeClient{list: []string{}}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc, match: matchNameField}

		if err := r.Delete(context.Background(), resource.Ref{Name: "never-existed"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(fc.deleteCalls) != 0 {
			t.Fatalf("deleteCalls = %v, want no DeleteResource call for a never-created resource", fc.deleteCalls)
		}
	})

	t.Run("a resolve failure is reported, not treated as absence", func(t *testing.T) {
		fc := &fakeClient{listErr: errors.New("throttled")}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc, match: matchNameField}

		if err := r.Delete(context.Background(), resource.Ref{Name: "x"}); err == nil {
			t.Fatal("expected the resolve failure to be reported")
		}
	})

	t.Run("a DeleteResource failure is reported, not swallowed", func(t *testing.T) {
		fc := &fakeClient{byIdentifier: map[string]map[string]any{"my-bucket": {}}, deleteErr: errors.New("access denied")}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		if err := r.Delete(context.Background(), resource.Ref{Name: "my-bucket"}); err == nil {
			t.Fatal("expected the failure to be reported")
		}
	})
}

func TestResourceTypeDiff(t *testing.T) {
	t.Run("a createOnlyProperty that difference is a replacement", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/BucketName"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		difference, err := r.Diff(
			resource.Spec{Config: map[string]any{"BucketName": "new-name"}},
			&resource.State{Attributes: map[string]any{"BucketName": "old-name"}},
		)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Immutable {
			t.Fatal("expected a createOnlyProperty change to be reported as differing")
		}
	})

	t.Run("a nested createOnlyProperty is compared at its own path", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/DistributionConfig/CallerReference"}}}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc}

		difference, err := r.Diff(
			resource.Spec{Config: map[string]any{"DistributionConfig": map[string]any{"CallerReference": "b", "Comment": "whatever"}}},
			&resource.State{Attributes: map[string]any{"DistributionConfig": map[string]any{"CallerReference": "a", "Comment": "different but not create-only"}}},
		)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Immutable {
			t.Fatal("expected the nested createOnlyProperty change to be reported as differing")
		}
	})

	t.Run("a matching createOnlyProperty is not a replacement", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/BucketName"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		difference, err := r.Diff(
			resource.Spec{Config: map[string]any{"BucketName": "same-name"}},
			&resource.State{Attributes: map[string]any{"BucketName": "same-name"}},
		)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Same {
			t.Fatal("expected an identical createOnlyProperty to report no difference")
		}
	})

	t.Run("a createOnlyProperty not declared in the manifest is never a source of difference", func(t *testing.T) {
		// A property the manifest never mentions is never touched — the
		// manifest is kraai's only source of truth — so its absence from
		// Spec.Config must not itself trigger a replacement.
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/BucketName"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		difference, err := r.Diff(
			resource.Spec{Config: map[string]any{}},
			&resource.State{Attributes: map[string]any{"BucketName": "whatever"}},
		)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Same {
			t.Fatal("expected no difference when the manifest never declares the property at all")
		}
	})

	t.Run("declared in the manifest but absent from current state is a replacement, not a silent match", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/BucketName"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		difference, err := r.Diff(
			resource.Spec{Config: map[string]any{"BucketName": "new-name"}},
			&resource.State{Attributes: map[string]any{}},
		)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Immutable {
			t.Fatal("expected a manifest-declared property Cloud Control never reported to be treated as differing")
		}
	})

	t.Run("no createOnlyProperties at all means never a replacement", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		difference, err := r.Diff(resource.Spec{Config: map[string]any{"Anything": "goes"}}, &resource.State{Attributes: map[string]any{}})
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Same {
			t.Fatal("expected no createOnlyProperties to mean no replacement")
		}
	})

	t.Run("the schema is fetched once and cached across calls", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/BucketName"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		for range 3 {
			if _, err := r.Diff(resource.Spec{Config: map[string]any{}}, &resource.State{}); err != nil {
				t.Fatalf("Diff: %v", err)
			}
		}
		if fc.schemaCalls != 1 {
			t.Fatalf("schemaCalls = %d, want exactly 1 (cached)", fc.schemaCalls)
		}
	})

	t.Run("a DescribeType failure is reported, not swallowed", func(t *testing.T) {
		fc := &fakeClient{schemaErr: errors.New("throttled")}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		if _, err := r.Diff(resource.Spec{}, &resource.State{}); err == nil {
			t.Fatal("expected the schema-fetch failure to be reported")
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
