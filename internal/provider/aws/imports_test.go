package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// An adopted resource is addressed by the identity the manifest declared, not
// by the name kraai would have derived — which it never created and so cannot
// derive.
func TestResolveHonoursAnImportedID(t *testing.T) {
	fc := &fakeClient{byIdentifier: map[string]map[string]any{
		"i-abc123": {"BucketName": "someone-elses-bucket"},
	}}
	r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

	state, err := r.Get(context.Background(), resource.Ref{
		Provider: Provider, Type: TypeS3Bucket, Name: "env-api-assets",
		Import: &resource.Import{ID: "i-abc123"},
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil {
		t.Fatal("the adopted resource was not found by its declared id")
	}
	if state.ID != "i-abc123" {
		t.Errorf("state.ID = %q, want the imported id", state.ID)
	}
	// The derived name was never asked for: an import replaces the lookup,
	// rather than being tried after it.
	for _, got := range fc.getCalls {
		if got == "env-api-assets" {
			t.Errorf("the derived name was looked up despite an import: %v", fc.getCalls)
		}
	}
}

// An id that names nothing reads as absence, not as a resource that exists —
// the read-back is what confirms it, exactly as it does for a derived name.
func TestResolveReportsAnImportedIDThatIsNotThere(t *testing.T) {
	fc := &fakeClient{byIdentifier: map[string]map[string]any{}}
	r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

	state, err := r.Get(context.Background(), resource.Ref{
		Provider: Provider, Type: TypeS3Bucket, Name: "env-api-assets",
		Import: &resource.Import{ID: "i-missing"},
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state != nil {
		t.Errorf("a phantom resource was reported for an id that resolves to nothing: %+v", state)
	}
}

// Importing by name replaces the derived name in the list-and-match walk, so
// a byTag or byAttr type adopts by whatever its match function compares.
func TestResolveHonoursAnImportedName(t *testing.T) {
	fc := &fakeClient{
		list: []string{"cert-1", "cert-2"},
		byIdentifier: map[string]map[string]any{
			"cert-1": {"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "derived-name"}}},
			"cert-2": {"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "legacy-cert"}}},
		},
	}
	r := &resourceType{
		provider: Provider, typeName: TypeCertificateManagerCertificate,
		lookup: resource.LookupByTag, client: fc, match: certificateMatch, stampTag: certificateStampTag,
	}

	state, err := r.Get(context.Background(), resource.Ref{
		Provider: Provider, Type: TypeCertificateManagerCertificate, Name: "derived-name",
		Import: &resource.Import{Name: "legacy-cert"},
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil {
		t.Fatal("the adopted certificate was not matched by its declared name")
	}
	if state.ID != "cert-2" {
		t.Errorf("state.ID = %q, want the candidate matching the imported name", state.ID)
	}
}

// Without an import the derived name is still what identifies the resource,
// so adoption narrows to the manifests that ask for it.
func TestResolveWithoutAnImportUsesTheDerivedName(t *testing.T) {
	fc := &fakeClient{byIdentifier: map[string]map[string]any{
		"env-api-assets": {"BucketName": "env-api-assets"},
	}}
	r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

	state, err := r.Get(context.Background(), resource.Ref{
		Provider: Provider, Type: TypeS3Bucket, Name: "env-api-assets",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil || state.ID != "env-api-assets" {
		t.Fatalf("state = %+v, want the resource found by its derived name", state)
	}
}

// A provider with no adoption support says so, rather than searching by a
// derived name, finding nothing, and telling the author their resource does
// not exist while it sits there under the id they wrote down.
func TestRejectImportNamesTheRealReason(t *testing.T) {
	err := resource.RejectImport("cloudflare", "kv_namespace", resource.Ref{
		Name: "env-api-cache", Import: &resource.Import{ID: "kv-1"},
	})
	if err == nil {
		t.Fatal("an import was accepted by a provider that cannot adopt")
	}
	for _, want := range []string{"cloudflare", "kv_namespace", "no import support"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}

	if err := resource.RejectImport("cloudflare", "kv_namespace", resource.Ref{Name: "x"}); err != nil {
		t.Errorf("a Ref with no import was rejected: %v", err)
	}
}
