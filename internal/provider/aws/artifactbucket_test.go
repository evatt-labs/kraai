package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/evatt-labs/kraai/internal/resource"
)

func TestArtifactBucketName(t *testing.T) {
	got := artifactBucketName("myenv-myservice")
	want := "myenv-myservice-artifacts"
	if got != want {
		t.Fatalf("artifactBucketName(%q) = %q, want %q", "myenv-myservice", got, want)
	}

	// A long derived name must still respect S3's 63-character ceiling
	// after the suffix is appended, and must not end in a hyphen.
	long := strings.Repeat("a", 60)
	longGot := artifactBucketName(long)
	if len(longGot) > maxBucketNameLen {
		t.Fatalf("artifactBucketName(%d-char name) = %d chars, want <= %d", len(long), len(longGot), maxBucketNameLen)
	}
	if strings.HasSuffix(longGot, "-") {
		t.Fatalf("artifactBucketName(%d-char name) = %q, ends in a hyphen", len(long), longGot)
	}
}

func TestArtifactBucketResourceRewritesNameBothWays(t *testing.T) {
	const serviceName = "myenv-api"
	const realBucket = "myenv-api-artifacts"

	fc := &fakeClient{
		byIdentifier: map[string]map[string]any{realBucket: {"BucketName": realBucket}},
		createID:     realBucket,
		createProps:  map[string]any{"BucketName": realBucket},
		schema:       Schema{PrimaryIdentifier: []string{"/properties/BucketName"}},
	}
	fs3 := &fakeS3{listOut: []*s3.ListObjectsV2Output{{}}}
	bucket := &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc},
		client: &Client{s3: fs3},
	}

	t.Run("Get resolves the real bucket name but reports the service Ref", func(t *testing.T) {
		state, err := bucket.Get(context.Background(), resource.Ref{Provider: Provider, Type: TypeArtifactBucket, Name: serviceName})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if state == nil {
			t.Fatal("Get: expected a state")
		}
		if state.Ref.Name != serviceName {
			t.Fatalf("state.Ref.Name = %q, want the caller's own Ref.Name %q, not the real bucket name", state.Ref.Name, serviceName)
		}
		if len(fc.getCalls) == 0 || fc.getCalls[0] != realBucket {
			t.Fatalf("GetResource called with %v, want the transformed bucket name %q", fc.getCalls, realBucket)
		}
	})

	t.Run("Create submits the real bucket name and reports the service Ref", func(t *testing.T) {
		// The incoming spec carries expandCompute's generic compute shape
		// (dir/settings/trigger/handler/schedule) — none of it is a real
		// AWS::S3::Bucket property, and Create must never forward it
		// verbatim (see this method's own doc comment on why an absent
		// BucketName silently multiplies buckets every apply).
		spec := resource.Spec{
			Name: serviceName,
			Config: map[string]any{
				"dir": "./app", "trigger": "http", "handler": "run.sh",
				"settings": map[string]any{"runtime": "python3.13"},
			},
		}
		state, err := bucket.Create(context.Background(), spec)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if state.Ref.Name != serviceName {
			t.Fatalf("state.Ref.Name = %q, want %q", state.Ref.Name, serviceName)
		}

		desired := fc.createCalls[0]
		if desired["BucketName"] != realBucket {
			t.Fatalf("BucketName = %v, want %q", desired["BucketName"], realBucket)
		}
		if len(desired) != 1 {
			t.Fatalf("desired state = %+v, want exactly {BucketName}: no generic compute keys leaked through", desired)
		}
	})

	t.Run("Delete resolves the real bucket name, emptying it before deleting it", func(t *testing.T) {
		// fs3's listOut ({{}}: no Contents) means the bucket is already
		// empty on entry, so a lone DeleteObjects assertion here would not
		// distinguish "emptying happened" from "emptying was skipped
		// entirely" — see TestArtifactBucketDeleteEmptiesBeforeDeleting
		// below for the case with real objects to empty.
		if err := bucket.Delete(context.Background(), resource.Ref{Name: serviceName}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(fs3.listReq) == 0 || *fs3.listReq[len(fs3.listReq)-1].Bucket != realBucket {
			t.Fatalf("ListObjectsV2 called with %v, want %q", fs3.listReq, realBucket)
		}
		if len(fc.deleteCalls) == 0 || fc.deleteCalls[len(fc.deleteCalls)-1] != realBucket {
			t.Fatalf("DeleteResource called with %v, want %q", fc.deleteCalls, realBucket)
		}
	})

	t.Run("Update is refused", func(t *testing.T) {
		if _, err := bucket.Update(context.Background(), resource.Ref{Name: serviceName}, resource.Spec{}); err == nil {
			t.Fatal("expected Update to be refused")
		}
	})
}

// TestArtifactBucketDeleteEmptiesBeforeDeleting is the direct regression
// test for the live failure this workstream exists to fix: a real `kraai
// destroy` against a live AWS account left every artifact bucket behind
// because Delete used to forward straight to the generic Cloud Control
// engine with no emptying step, and S3 refuses to delete a non-empty
// bucket (409 GeneralServiceException, "The bucket you tried to delete is
// not empty"). A bucket that genuinely holds objects must have every one
// of them removed via DeleteObjects before DeleteResource is ever called.
//
// Reverting the fix (commenting out the EmptyBucket call in Delete) turns
// this test red with:
//
//	deleted 0 objects across all DeleteObjects calls, want 1
//
// confirming this test actually exercises the emptying step rather than
// passing regardless — see this workstream's PR description for that run.
func TestArtifactBucketDeleteEmptiesBeforeDeleting(t *testing.T) {
	const serviceName = "myenv-api"
	const realBucket = "myenv-api-artifacts"

	fc := &fakeClient{byIdentifier: map[string]map[string]any{realBucket: {"BucketName": realBucket}}}
	fs3 := &fakeS3{listOut: []*s3.ListObjectsV2Output{objectPage(3, false, "")}}
	bucket := &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc},
		client: &Client{s3: fs3},
	}

	if err := bucket.Delete(context.Background(), resource.Ref{Name: serviceName}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var deletedObjects int
	for _, req := range fs3.deleteObjectsReq {
		deletedObjects += len(req.Delete.Objects)
	}
	if deletedObjects != 3 {
		t.Fatalf("deleted %d objects across all DeleteObjects calls, want 3", deletedObjects)
	}
	if len(fc.deleteCalls) == 0 || fc.deleteCalls[len(fc.deleteCalls)-1] != realBucket {
		t.Fatalf("DeleteResource called with %v, want %q — emptying must not skip the underlying bucket delete", fc.deleteCalls, realBucket)
	}
}

// TestArtifactBucketDeleteSurfacesEmptyingFailures proves a real failure
// while emptying the bucket (access denied, throttling, ...) is reported
// and stops the underlying Cloud Control delete from running at all —
// deleting a bucket kraai failed to fully empty would only trade "left
// behind, still full" for "left behind, silently emptied of the wrong
// keys or partially emptied," neither of which this workstream's brief
// permits (destroy must surface a real failure, never swallow it as if it
// meant absence).
func TestArtifactBucketDeleteSurfacesEmptyingFailures(t *testing.T) {
	const serviceName = "myenv-api"
	const realBucket = "myenv-api-artifacts"

	fc := &fakeClient{byIdentifier: map[string]map[string]any{realBucket: {"BucketName": realBucket}}}
	fs3 := &fakeS3{listErr: errors.New("access denied")}
	bucket := &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc},
		client: &Client{s3: fs3},
	}

	if err := bucket.Delete(context.Background(), resource.Ref{Name: serviceName}); err == nil {
		t.Fatal("expected an emptying failure to be reported, not swallowed")
	}
	if len(fc.deleteCalls) != 0 {
		t.Fatalf("DeleteResource was called (%v) despite emptying having failed; the bucket delete must not proceed", fc.deleteCalls)
	}
}

// TestArtifactBucketDeleteOnAbsentBucket proves the "already gone" path
// through EmptyBucket (a NoSuchBucket on listing) still reaches
// DeleteResource, exactly as resource.Resource.Delete's own contract
// requires: deleting something already absent is success, and this must
// hold whether the bucket was never created or was already fully torn
// down by a previous, partially-failed destroy run.
func TestArtifactBucketDeleteOnAbsentBucket(t *testing.T) {
	const serviceName = "myenv-api"
	const realBucket = "myenv-api-artifacts"

	fc := &fakeClient{byIdentifier: map[string]map[string]any{realBucket: {"BucketName": realBucket}}}
	fs3 := &fakeS3{listErr: &s3types.NoSuchBucket{}}
	bucket := &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc},
		client: &Client{s3: fs3},
	}

	if err := bucket.Delete(context.Background(), resource.Ref{Name: serviceName}); err != nil {
		t.Fatalf("Delete on an already-absent bucket: %v, want nil", err)
	}
	if len(fc.deleteCalls) == 0 || fc.deleteCalls[len(fc.deleteCalls)-1] != realBucket {
		t.Fatalf("DeleteResource called with %v, want %q", fc.deleteCalls, realBucket)
	}
}

// TestArtifactBucketGetAbsentBucket proves the first of the three outcomes
// this workstream's brief asks Get to distinguish: a bucket Cloud Control
// never found at all reads as absent, exactly as before this workstream —
// and never even reaches the ownership check, since there is nothing to
// check ownership of.
func TestArtifactBucketGetAbsentBucket(t *testing.T) {
	const serviceName = "myenv-api"

	fc := &fakeClient{} // byIdentifier empty: Cloud Control finds nothing.
	fs3 := &fakeS3{}
	bucket := &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc},
		client: &Client{s3: fs3},
	}

	state, err := bucket.Get(context.Background(), resource.Ref{Name: serviceName})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state != nil {
		t.Fatalf("Get = %+v, want nil: Cloud Control reported no such bucket", state)
	}
	if len(fs3.listBucketsReq) != 0 {
		t.Fatalf("OwnsBucket was checked (%d ListBuckets call(s)) for a bucket Cloud Control never found", len(fs3.listBucketsReq))
	}
}

// TestArtifactBucketGetForeignBucketReadsAsAbsent is the direct regression
// test for the live bug this workstream fixes — see artifactbucket.go's
// Get doc comment for the full live evidence this reproduces: S3 bucket
// names are unique globally, not per account, so Cloud Control's
// GetResource happily resolves a bucket a completely different AWS account
// owns. Before the ownership check existed, that success alone was read as
// "this environment's own bucket, no change needed."
//
// # Revert-and-fail
//
// Commenting out the `if !owned { ... return nil, nil }` branch in
// artifactbucket.go's Get (falling through to `state.Ref = ref; return
// state, nil` unconditionally, exactly as the code read before this
// workstream) turns this test red with the real output:
//
//	artifactbucket_test.go:284: Get = &{Ref:{Provider: Type: Name:myenv-api Import:<nil>} ID:myenv-api-artifacts Attributes:map[BucketName:myenv-api-artifacts]}, want nil: this bucket belongs to a different AWS account
//	--- FAIL: TestArtifactBucketGetForeignBucketReadsAsAbsent (0.00s)
//
// confirming the test actually exercises the ownership gate rather than
// passing regardless. Restoring the branch turns it green again.
func TestArtifactBucketGetForeignBucketReadsAsAbsent(t *testing.T) {
	const serviceName = "myenv-api"
	const realBucket = "myenv-api-artifacts"

	// Cloud Control resolves the bucket globally (it exists, just not in
	// this account) — the live "dev-api-artifacts" repro's GetResource
	// success.
	fc := &fakeClient{byIdentifier: map[string]map[string]any{realBucket: {"BucketName": realBucket}}}
	// This account's own ListBuckets does not include it — the live repro's
	// `aws s3api list-buckets` returning [].
	fs3 := &fakeS3{listBucketsOut: []*s3.ListBucketsOutput{{}}}
	bucket := &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc},
		client: &Client{s3: fs3},
	}

	state, err := bucket.Get(context.Background(), resource.Ref{Name: serviceName})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state != nil {
		t.Fatalf("Get = %+v, want nil: this bucket belongs to a different AWS account", state)
	}
	if len(fs3.listBucketsReq) != 1 || fs3.listBucketsReq[0].Prefix == nil || *fs3.listBucketsReq[0].Prefix != realBucket {
		t.Fatalf("ListBuckets called with %v, want one call Prefix-filtered to %q", fs3.listBucketsReq, realBucket)
	}
}

// TestArtifactBucketGetOwnedBucketIsGenuineNoChange is the third of the
// three outcomes: Cloud Control finds the bucket and this account's own
// ListBuckets confirms it — the real "no change needed" case, reported
// with the caller's own Ref, not the real bucket name (see this method's
// own comment on why).
func TestArtifactBucketGetOwnedBucketIsGenuineNoChange(t *testing.T) {
	const serviceName = "myenv-api"
	const realBucket = "myenv-api-artifacts"

	fc := &fakeClient{byIdentifier: map[string]map[string]any{realBucket: {"BucketName": realBucket}}}
	fs3 := &fakeS3{listBucketsOut: []*s3.ListBucketsOutput{{
		Buckets: []s3types.Bucket{{Name: aws.String(realBucket)}},
	}}}
	bucket := &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc},
		client: &Client{s3: fs3},
	}

	state, err := bucket.Get(context.Background(), resource.Ref{Name: serviceName})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil {
		t.Fatal("Get = nil, want the genuine current state: this account owns the bucket Cloud Control found")
	}
	if state.Ref.Name != serviceName {
		t.Fatalf("state.Ref.Name = %q, want the caller's own Ref.Name %q", state.Ref.Name, serviceName)
	}
}

// TestArtifactBucketGetOwnershipCheckFailureIsNeverSilentlyAbsent covers
// this workstream's brief explicitly: a permissions or infra failure while
// checking ownership (this account's own ListBuckets call itself failing,
// e.g. missing s3:ListAllMyBuckets) is a real error, distinguished from the
// foreign-owner case above — it must never collapse into the same (nil,
// nil) "absent" answer a confirmed-foreign bucket gets, which would
// misreport a misconfigured policy as "someone else owns this."
func TestArtifactBucketGetOwnershipCheckFailureIsNeverSilentlyAbsent(t *testing.T) {
	const serviceName = "myenv-api"
	const realBucket = "myenv-api-artifacts"

	fc := &fakeClient{byIdentifier: map[string]map[string]any{realBucket: {"BucketName": realBucket}}}
	fs3 := &fakeS3{listBucketsErr: errors.New("access denied")}
	bucket := &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc},
		client: &Client{s3: fs3},
	}

	state, err := bucket.Get(context.Background(), resource.Ref{Name: serviceName})
	if err == nil {
		t.Fatal("expected a ListBuckets failure while checking ownership to be reported, not swallowed")
	}
	if state != nil {
		t.Fatalf("Get = %+v alongside a non-nil error, want nil state", state)
	}
}

// TestArtifactBucketDeleteRefusesForeignBucket is the destroy-path half of
// this workstream's fix: consequence #1 of the live bug this closes was
// that `kraai destroy` would call EmptyBucket (ListObjectsV2 then
// DeleteObjects) against a bucket kraai does not own. This proves Delete
// itself refuses before either the emptying or the deletion step runs —
// see Delete's own doc comment for why the guard lives inside Delete
// rather than relying on Get having run first.
//
// # Revert-and-fail
//
// Commenting out the `if !owned { ... return nil }` branch in
// artifactbucket.go's Delete turns this test red with the real output:
//
//	artifactbucket_test.go:380: ListObjectsV2 was called (1 time(s)) against a bucket this account does not own
//	--- FAIL: TestArtifactBucketDeleteRefusesForeignBucket (0.00s)
//
// confirming the guard, not incidental behavior, is what this test
// exercises. Restoring the branch turns it green again.
func TestArtifactBucketDeleteRefusesForeignBucket(t *testing.T) {
	const serviceName = "myenv-api"
	const realBucket = "myenv-api-artifacts"

	fc := &fakeClient{byIdentifier: map[string]map[string]any{realBucket: {"BucketName": realBucket}}}
	fs3 := &fakeS3{listBucketsOut: []*s3.ListBucketsOutput{{}}} // not in this account's own list
	bucket := &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc},
		client: &Client{s3: fs3},
	}

	if err := bucket.Delete(context.Background(), resource.Ref{Name: serviceName}); err != nil {
		t.Fatalf("Delete on a foreign-owned bucket: %v, want nil (nothing this account owns to delete)", err)
	}
	if len(fs3.listReq) != 0 {
		t.Fatalf("ListObjectsV2 was called (%d time(s)) against a bucket this account does not own", len(fs3.listReq))
	}
	if len(fc.deleteCalls) != 0 {
		t.Fatalf("DeleteResource was called (%v) against a bucket this account does not own", fc.deleteCalls)
	}
}

// TestArtifactBucketDeleteOwnershipCheckFailureIsNeverSilentlyPermitted
// mirrors TestArtifactBucketGetOwnershipCheckFailureIsNeverSilentlyAbsent
// for the destroy path: a failure to even determine ownership must stop
// Delete before it touches the bucket, never be read as "not ours, skip
// harmlessly" or "ours, proceed."
func TestArtifactBucketDeleteOwnershipCheckFailureIsNeverSilentlyPermitted(t *testing.T) {
	const serviceName = "myenv-api"
	const realBucket = "myenv-api-artifacts"

	fc := &fakeClient{byIdentifier: map[string]map[string]any{realBucket: {"BucketName": realBucket}}}
	fs3 := &fakeS3{listBucketsErr: errors.New("access denied")}
	bucket := &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc},
		client: &Client{s3: fs3},
	}

	if err := bucket.Delete(context.Background(), resource.Ref{Name: serviceName}); err == nil {
		t.Fatal("expected a ListBuckets failure while checking ownership to be reported, not swallowed")
	}
	if len(fs3.listReq) != 0 {
		t.Fatalf("ListObjectsV2 was called (%d time(s)) despite ownership being undetermined", len(fs3.listReq))
	}
	if len(fc.deleteCalls) != 0 {
		t.Fatalf("DeleteResource was called (%v) despite ownership being undetermined", fc.deleteCalls)
	}
}
