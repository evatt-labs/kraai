package aws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

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
