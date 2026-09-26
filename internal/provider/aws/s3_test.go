package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// fakeS3 is a hand-rolled s3API: no AWS account, no network needed.
type fakeS3 struct {
	err  error
	reqs []*s3.PutObjectInput
	// putConflict answers every conditional PutObject with a 409.
	putConflict bool

	// listOut/listErr/listAt script ListObjectsV2 responses, one entry per
	// call in order — the same sequential-page pattern fakeCC's
	// listOut/listAt already use for ListResources. The last entry repeats
	// once exhausted.
	listOut []*s3.ListObjectsV2Output
	listErr error
	listAt  int
	listReq []*s3.ListObjectsV2Input

	// deleteObjectsOut/deleteObjectsErr script every DeleteObjects call
	// identically: EmptyBucket's tests never need per-call variation here,
	// only whether a batch succeeded, partially failed (via Errors on the
	// output), or errored outright.
	deleteObjectsOut *s3.DeleteObjectsOutput
	deleteObjectsErr error
	deleteObjectsReq []*s3.DeleteObjectsInput

	// listBucketsOut/listBucketsErr script ListBuckets, one entry per call
	// in order (mirroring listOut/listAt's sequential-page pattern above)
	// when listBucketsOut has more than one element; a single element (or
	// none, see ListBuckets's own doc comment) repeats for every call.
	listBucketsOut []*s3.ListBucketsOutput
	listBucketsErr error
	listBucketsAt  int
	listBucketsReq []*s3.ListBucketsInput

	// putPolicyErr scripts PutBucketPolicy; putPolicyReq records every call.
	putPolicyErr error
	putPolicyReq []*s3.PutBucketPolicyInput

	// getPolicyOut/getPolicyErr script GetBucketPolicy; a nil getPolicyOut
	// with no getPolicyErr answers with no policy at all (Policy == nil),
	// distinct from the NoSuchBucketPolicy tests construct explicitly via
	// getPolicyErr.
	getPolicyOut *s3.GetBucketPolicyOutput
	getPolicyErr error
	getPolicyReq []*s3.GetBucketPolicyInput

	// deletePolicyErr scripts DeleteBucketPolicy; deletePolicyReq records
	// every call.
	deletePolicyErr error
	deletePolicyReq []*s3.DeleteBucketPolicyInput

	// objects and buckets give the fake the object-store semantics the
	// lock store relies on (lockstore.go): a conditional create refused
	// when the key exists, a conditional delete refused when the ETag
	// moved, and a bucket that must exist. Both are nil for the tests
	// that only script PutObject through err and reqs above.
	objects map[string]fakeObject
	buckets map[string]bool
	etags   int
	// publicAccessBlocked records the buckets PutPublicAccessBlock ran on.
	publicAccessBlocked []string
}

type fakeObject struct {
	body []byte
	etag string
}

// s3Error builds the generic API error S3 returns for code.
func s3Error(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code}
}

func (f *fakeS3) PutObject(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.reqs = append(f.reqs, params)
	if f.err != nil {
		return nil, f.err
	}
	if f.objects == nil {
		return &s3.PutObjectOutput{}, nil
	}
	key := aws.ToString(params.Bucket) + "/" + aws.ToString(params.Key)
	if aws.ToString(params.IfNoneMatch) == "*" {
		if _, exists := f.objects[key]; exists {
			return nil, s3Error("PreconditionFailed")
		}
	}
	// If-Match as S3 documents it: 404 when the key is gone, 412 when its
	// ETag differs, and a scripted 409 for a concurrent conflict.
	if params.IfMatch != nil {
		if f.putConflict {
			return nil, s3Error("ConditionalRequestConflict")
		}
		object, exists := f.objects[key]
		if !exists {
			return nil, &s3types.NoSuchKey{}
		}
		if object.etag != aws.ToString(params.IfMatch) {
			return nil, s3Error("PreconditionFailed")
		}
	}
	body, _ := io.ReadAll(params.Body)
	f.etags++
	etag := fmt.Sprintf("\"etag-%d\"", f.etags)
	f.objects[key] = fakeObject{body: body, etag: etag}
	return &s3.PutObjectOutput{ETag: aws.String(etag)}, nil
}

func (f *fakeS3) GetObject(_ context.Context, params *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	object, ok := f.objects[aws.ToString(params.Bucket)+"/"+aws.ToString(params.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(object.body)), ETag: aws.String(object.etag)}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, params *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	key := aws.ToString(params.Bucket) + "/" + aws.ToString(params.Key)
	object, ok := f.objects[key]
	if ok && params.IfMatch != nil && aws.ToString(params.IfMatch) != object.etag {
		return nil, s3Error("PreconditionFailed")
	}
	delete(f.objects, key)
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) HeadBucket(_ context.Context, params *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	if !f.buckets[aws.ToString(params.Bucket)] {
		return nil, &s3types.NotFound{}
	}
	return &s3.HeadBucketOutput{}, nil
}

func (f *fakeS3) CreateBucket(_ context.Context, params *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	if f.buckets == nil {
		f.buckets = map[string]bool{}
	}
	f.buckets[aws.ToString(params.Bucket)] = true
	return &s3.CreateBucketOutput{}, nil
}

func (f *fakeS3) PutPublicAccessBlock(_ context.Context, params *s3.PutPublicAccessBlockInput, _ ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error) {
	f.publicAccessBlocked = append(f.publicAccessBlocked, aws.ToString(params.Bucket))
	return &s3.PutPublicAccessBlockOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, params *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.listReq = append(f.listReq, params)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.listOut) == 0 {
		return &s3.ListObjectsV2Output{}, nil
	}
	out := f.listOut[f.listAt]
	if f.listAt < len(f.listOut)-1 {
		f.listAt++
	}
	return out, nil
}

func (f *fakeS3) DeleteObjects(_ context.Context, params *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.deleteObjectsReq = append(f.deleteObjectsReq, params)
	if f.deleteObjectsErr != nil {
		return nil, f.deleteObjectsErr
	}
	if f.deleteObjectsOut != nil {
		return f.deleteObjectsOut, nil
	}
	return &s3.DeleteObjectsOutput{}, nil
}

// ListBuckets defaults to "yes, this account owns whatever bucket name was
// queried" when a test configures neither listBucketsOut nor
// listBucketsErr — the opposite default from ListObjectsV2/DeleteObjects
// above, deliberately: every artifactBucketResource test in
// artifactbucket_test.go that predates the ownership check (name
// rewriting, emptying, Create) exercises none of it, and defaulting to
// "owned" here means none of them needed to be rewritten just to satisfy a
// gate they were never testing. Tests that actually exercise ownership
// (TestClientOwnsBucket, and the foreign-bucket cases in
// artifactbucket_test.go) configure listBucketsOut/listBucketsErr
// explicitly to override this default.
func (f *fakeS3) ListBuckets(_ context.Context, params *s3.ListBucketsInput, _ ...func(*s3.Options)) (*s3.ListBucketsOutput, error) {
	f.listBucketsReq = append(f.listBucketsReq, params)
	if f.listBucketsErr != nil {
		return nil, f.listBucketsErr
	}
	if len(f.listBucketsOut) == 0 {
		var name string
		if params.Prefix != nil {
			name = *params.Prefix
		}
		return &s3.ListBucketsOutput{Buckets: []s3types.Bucket{{Name: aws.String(name)}}}, nil
	}
	out := f.listBucketsOut[f.listBucketsAt]
	if f.listBucketsAt < len(f.listBucketsOut)-1 {
		f.listBucketsAt++
	}
	return out, nil
}

func (f *fakeS3) PutBucketPolicy(_ context.Context, params *s3.PutBucketPolicyInput, _ ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error) {
	f.putPolicyReq = append(f.putPolicyReq, params)
	if f.putPolicyErr != nil {
		return nil, f.putPolicyErr
	}
	return &s3.PutBucketPolicyOutput{}, nil
}

func (f *fakeS3) GetBucketPolicy(_ context.Context, params *s3.GetBucketPolicyInput, _ ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error) {
	f.getPolicyReq = append(f.getPolicyReq, params)
	if f.getPolicyErr != nil {
		return nil, f.getPolicyErr
	}
	if f.getPolicyOut != nil {
		return f.getPolicyOut, nil
	}
	return &s3.GetBucketPolicyOutput{}, nil
}

func (f *fakeS3) DeleteBucketPolicy(_ context.Context, params *s3.DeleteBucketPolicyInput, _ ...func(*s3.Options)) (*s3.DeleteBucketPolicyOutput, error) {
	f.deletePolicyReq = append(f.deletePolicyReq, params)
	if f.deletePolicyErr != nil {
		return nil, f.deletePolicyErr
	}
	return &s3.DeleteBucketPolicyOutput{}, nil
}

func TestClientPutObject(t *testing.T) {
	t.Run("uploads to bucket/key", func(t *testing.T) {
		fs3 := &fakeS3{}
		c := &Client{s3: fs3}

		if err := c.PutObject(context.Background(), "my-bucket", "svc/abc123.zip", []byte("zip-bytes")); err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		if len(fs3.reqs) != 1 {
			t.Fatalf("got %d PutObject calls, want 1", len(fs3.reqs))
		}
		if *fs3.reqs[0].Bucket != "my-bucket" || *fs3.reqs[0].Key != "svc/abc123.zip" {
			t.Fatalf("request = %+v", fs3.reqs[0])
		}
	})

	t.Run("wraps an error", func(t *testing.T) {
		c := &Client{s3: &fakeS3{err: errors.New("access denied")}}
		if err := c.PutObject(context.Background(), "b", "k", nil); err == nil {
			t.Fatal("expected an error")
		}
	})
}

// objectPage builds a ListObjectsV2Output listing n synthetic keys
// (prefixed key-0, key-1, ...), optionally truncated with a continuation
// token to the next page.
func objectPage(n int, truncated bool, nextToken string) *s3.ListObjectsV2Output {
	contents := make([]s3types.Object, n)
	for i := range contents {
		contents[i] = s3types.Object{Key: aws.String("key-" + strconv.Itoa(i))}
	}
	out := &s3.ListObjectsV2Output{Contents: contents, IsTruncated: aws.Bool(truncated)}
	if nextToken != "" {
		out.NextContinuationToken = aws.String(nextToken)
	}
	return out
}

func TestClientEmptyBucket(t *testing.T) {
	t.Run("empties a bucket with objects, then succeeds", func(t *testing.T) {
		fs3 := &fakeS3{listOut: []*s3.ListObjectsV2Output{objectPage(3, false, "")}}
		c := &Client{s3: fs3}

		if err := c.EmptyBucket(context.Background(), "my-bucket"); err != nil {
			t.Fatalf("EmptyBucket: %v", err)
		}
		if len(fs3.deleteObjectsReq) != 1 {
			t.Fatalf("got %d DeleteObjects calls, want 1", len(fs3.deleteObjectsReq))
		}
		if got := len(fs3.deleteObjectsReq[0].Delete.Objects); got != 3 {
			t.Fatalf("deleted %d objects, want 3", got)
		}
		if *fs3.deleteObjectsReq[0].Bucket != "my-bucket" {
			t.Fatalf("Bucket = %q, want %q", *fs3.deleteObjectsReq[0].Bucket, "my-bucket")
		}
	})

	t.Run("pages through more than one page of listing, deleting every key", func(t *testing.T) {
		// Naively reading only the first page would leave page two's keys
		// behind and the bucket non-empty — exactly the bug this test
		// guards against; see this test's revert-and-fail run in the PR
		// description.
		fs3 := &fakeS3{listOut: []*s3.ListObjectsV2Output{
			objectPage(2, true, "page-2"),
			objectPage(2, false, ""),
		}}
		c := &Client{s3: fs3}

		if err := c.EmptyBucket(context.Background(), "my-bucket"); err != nil {
			t.Fatalf("EmptyBucket: %v", err)
		}
		if len(fs3.listReq) != 2 {
			t.Fatalf("got %d ListObjectsV2 calls, want 2", len(fs3.listReq))
		}
		if fs3.listReq[1].ContinuationToken == nil || *fs3.listReq[1].ContinuationToken != "page-2" {
			t.Fatalf("second ListObjectsV2 call's ContinuationToken = %v, want %q", fs3.listReq[1].ContinuationToken, "page-2")
		}
		if len(fs3.deleteObjectsReq) != 2 {
			t.Fatalf("got %d DeleteObjects calls, want 2 (one per page)", len(fs3.deleteObjectsReq))
		}
		total := len(fs3.deleteObjectsReq[0].Delete.Objects) + len(fs3.deleteObjectsReq[1].Delete.Objects)
		if total != 4 {
			t.Fatalf("deleted %d objects total across both pages, want 4", total)
		}
	})

	t.Run("more than 1000 keys issues more than one DeleteObjects batch", func(t *testing.T) {
		fs3 := &fakeS3{listOut: []*s3.ListObjectsV2Output{
			objectPage(1000, true, "page-2"),
			objectPage(50, false, ""),
		}}
		c := &Client{s3: fs3}

		if err := c.EmptyBucket(context.Background(), "my-bucket"); err != nil {
			t.Fatalf("EmptyBucket: %v", err)
		}
		if len(fs3.deleteObjectsReq) != 2 {
			t.Fatalf("got %d DeleteObjects calls, want 2", len(fs3.deleteObjectsReq))
		}
		for i, req := range fs3.deleteObjectsReq {
			if len(req.Delete.Objects) > 1000 {
				t.Fatalf("DeleteObjects call %d carried %d keys, exceeds S3's 1000-key ceiling", i, len(req.Delete.Objects))
			}
		}
		if got := len(fs3.deleteObjectsReq[0].Delete.Objects); got != 1000 {
			t.Fatalf("first batch = %d keys, want 1000", got)
		}
		if got := len(fs3.deleteObjectsReq[1].Delete.Objects); got != 50 {
			t.Fatalf("second batch = %d keys, want 50", got)
		}
	})

	t.Run("an already-absent bucket is success, not an error", func(t *testing.T) {
		fs3 := &fakeS3{listErr: &s3types.NoSuchBucket{}}
		c := &Client{s3: fs3}

		if err := c.EmptyBucket(context.Background(), "gone-bucket"); err != nil {
			t.Fatalf("EmptyBucket on an absent bucket: %v, want nil (already gone is success)", err)
		}
		if len(fs3.deleteObjectsReq) != 0 {
			t.Fatal("expected no DeleteObjects call against a bucket that does not exist")
		}
	})

	t.Run("a listing error surfaces rather than being swallowed", func(t *testing.T) {
		fs3 := &fakeS3{listErr: errors.New("throttled")}
		c := &Client{s3: fs3}

		err := c.EmptyBucket(context.Background(), "my-bucket")
		if err == nil {
			t.Fatal("expected a throttling error during listing to be reported, not treated as absence")
		}
	})

	t.Run("a delete error surfaces rather than being swallowed", func(t *testing.T) {
		fs3 := &fakeS3{
			listOut:          []*s3.ListObjectsV2Output{objectPage(1, false, "")},
			deleteObjectsErr: errors.New("access denied"),
		}
		c := &Client{s3: fs3}

		if err := c.EmptyBucket(context.Background(), "my-bucket"); err == nil {
			t.Fatal("expected a DeleteObjects failure to be reported")
		}
	})

	t.Run("a per-object failure inside an otherwise successful DeleteObjects response surfaces", func(t *testing.T) {
		fs3 := &fakeS3{
			listOut: []*s3.ListObjectsV2Output{objectPage(1, false, "")},
			deleteObjectsOut: &s3.DeleteObjectsOutput{
				Errors: []s3types.Error{{Key: aws.String("key-0"), Code: aws.String("AccessDenied"), Message: aws.String("denied")}},
			},
		}
		c := &Client{s3: fs3}

		if err := c.EmptyBucket(context.Background(), "my-bucket"); err == nil {
			t.Fatal("expected a per-object DeleteObjects failure reported in the response body to be surfaced")
		}
	})
}

// TestClientOwnsBucket is the direct regression test for the live bug this
// workstream fixes: S3 bucket names are unique globally, not per account,
// and Cloud Control's GetResource resolves that global namespace with no
// ownership check at all — verified against the live Evatt Labs account
// (409032463870, us-east-1), see OwnsBucket's own doc comment for the full
// evidence. OwnsBucket is the one place this package can still tell "exists
// somewhere" apart from "exists in this account."
func TestClientOwnsBucket(t *testing.T) {
	t.Run("the bucket is in this account's own list", func(t *testing.T) {
		fs3 := &fakeS3{listBucketsOut: []*s3.ListBucketsOutput{{
			Buckets: []s3types.Bucket{{Name: aws.String("dev-api-artifacts")}},
		}}}
		c := &Client{s3: fs3}

		owned, err := c.OwnsBucket(context.Background(), "dev-api-artifacts")
		if err != nil {
			t.Fatalf("OwnsBucket: %v", err)
		}
		if !owned {
			t.Fatal("OwnsBucket = false, want true: the bucket is in this account's own ListBuckets response")
		}
		if len(fs3.listBucketsReq) != 1 || fs3.listBucketsReq[0].Prefix == nil || *fs3.listBucketsReq[0].Prefix != "dev-api-artifacts" {
			t.Fatalf("ListBuckets called with %v, want a request Prefix-filtered to the bucket being checked", fs3.listBucketsReq)
		}
	})

	t.Run("a foreign bucket this account does not own reads as not owned, not as an error", func(t *testing.T) {
		// The live repro this guards against: this account's own
		// ListBuckets genuinely returns no such bucket, because the real
		// "dev-api-artifacts" belongs to a different AWS account entirely
		// — Prefix narrows the request, but an empty Buckets slice (or one
		// containing only unrelated names) is a normal, successful
		// response, not a failure.
		fs3 := &fakeS3{listBucketsOut: []*s3.ListBucketsOutput{{}}}
		c := &Client{s3: fs3}

		owned, err := c.OwnsBucket(context.Background(), "dev-api-artifacts")
		if err != nil {
			t.Fatalf("OwnsBucket: %v", err)
		}
		if owned {
			t.Fatal("OwnsBucket = true, want false: this account's own ListBuckets does not include this bucket")
		}
	})

	t.Run("Prefix is begins-with, not exact: a same-prefix decoy does not count as owned", func(t *testing.T) {
		fs3 := &fakeS3{listBucketsOut: []*s3.ListBucketsOutput{{
			Buckets: []s3types.Bucket{{Name: aws.String("dev-api-artifacts-decoy")}},
		}}}
		c := &Client{s3: fs3}

		owned, err := c.OwnsBucket(context.Background(), "dev-api-artifacts")
		if err != nil {
			t.Fatalf("OwnsBucket: %v", err)
		}
		if owned {
			t.Fatal("OwnsBucket = true, want false: the only candidate returned is a different bucket name that merely shares the prefix")
		}
	})

	t.Run("pages through more than one page before finding the match", func(t *testing.T) {
		fs3 := &fakeS3{listBucketsOut: []*s3.ListBucketsOutput{
			{Buckets: []s3types.Bucket{{Name: aws.String("dev-api-artifacts-old")}}, ContinuationToken: aws.String("page-2")},
			{Buckets: []s3types.Bucket{{Name: aws.String("dev-api-artifacts")}}},
		}}
		c := &Client{s3: fs3}

		owned, err := c.OwnsBucket(context.Background(), "dev-api-artifacts")
		if err != nil {
			t.Fatalf("OwnsBucket: %v", err)
		}
		if !owned {
			t.Fatal("OwnsBucket = false, want true: the match is on the second page")
		}
		if len(fs3.listBucketsReq) != 2 {
			t.Fatalf("got %d ListBuckets calls, want 2", len(fs3.listBucketsReq))
		}
		if fs3.listBucketsReq[1].ContinuationToken == nil || *fs3.listBucketsReq[1].ContinuationToken != "page-2" {
			t.Fatalf("second ListBuckets call's ContinuationToken = %v, want %q", fs3.listBucketsReq[1].ContinuationToken, "page-2")
		}
	})

	t.Run("a permissions or infra failure on ListBuckets itself is a real error, never read as not-owned", func(t *testing.T) {
		// This is the disambiguation this workstream's brief asks for
		// explicitly: an account-wide failure to even list this account's
		// own buckets (e.g. missing s3:ListAllMyBuckets) says nothing
		// about whether this specific bucket belongs to a stranger, and
		// must not be silently reported as "someone else owns this."
		fs3 := &fakeS3{listBucketsErr: errors.New("access denied")}
		c := &Client{s3: fs3}

		owned, err := c.OwnsBucket(context.Background(), "dev-api-artifacts")
		if err == nil {
			t.Fatal("expected a ListBuckets failure to be reported as an error")
		}
		if owned {
			t.Fatal("OwnsBucket = true alongside a non-nil error; a failed check must never assert ownership")
		}
	})
}

func TestClientPutBucketPolicy(t *testing.T) {
	t.Run("puts the policy on the bucket", func(t *testing.T) {
		fs3 := &fakeS3{}
		c := &Client{s3: fs3}
		if err := c.PutBucketPolicy(context.Background(), "my-bucket", `{"Version":"2012-10-17"}`); err != nil {
			t.Fatalf("PutBucketPolicy: %v", err)
		}
		if len(fs3.putPolicyReq) != 1 {
			t.Fatalf("got %d PutBucketPolicy calls, want 1", len(fs3.putPolicyReq))
		}
		if *fs3.putPolicyReq[0].Bucket != "my-bucket" || *fs3.putPolicyReq[0].Policy != `{"Version":"2012-10-17"}` {
			t.Fatalf("request = %+v", fs3.putPolicyReq[0])
		}
	})

	t.Run("wraps an error", func(t *testing.T) {
		c := &Client{s3: &fakeS3{putPolicyErr: errors.New("access denied")}}
		if err := c.PutBucketPolicy(context.Background(), "b", "{}"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestClientGetBucketPolicy(t *testing.T) {
	t.Run("returns the live policy", func(t *testing.T) {
		c := &Client{s3: &fakeS3{getPolicyOut: &s3.GetBucketPolicyOutput{Policy: aws.String(`{"Version":"2012-10-17"}`)}}}
		policy, found, err := c.GetBucketPolicy(context.Background(), "b")
		if err != nil || !found || policy != `{"Version":"2012-10-17"}` {
			t.Fatalf("policy, found, err = %q, %v, %v", policy, found, err)
		}
	})

	t.Run("NoSuchBucketPolicy is absence, not an error", func(t *testing.T) {
		// S3 does not model this one as its own Go error type — see
		// bucketPolicyAbsent's own doc comment — so the fake reports it
		// exactly as the SDK would: a generic smithy.APIError carrying
		// only the code.
		c := &Client{s3: &fakeS3{getPolicyErr: &smithy.GenericAPIError{Code: "NoSuchBucketPolicy"}}}
		_, found, err := c.GetBucketPolicy(context.Background(), "b")
		if err != nil || found {
			t.Fatalf("found, err = %v, %v, want (false, nil)", found, err)
		}
	})

	t.Run("NoSuchBucket is absence, not an error", func(t *testing.T) {
		c := &Client{s3: &fakeS3{getPolicyErr: &s3types.NoSuchBucket{}}}
		_, found, err := c.GetBucketPolicy(context.Background(), "b")
		if err != nil || found {
			t.Fatalf("found, err = %v, %v, want (false, nil)", found, err)
		}
	})

	t.Run("every other error is real", func(t *testing.T) {
		c := &Client{s3: &fakeS3{getPolicyErr: errors.New("throttled")}}
		_, found, err := c.GetBucketPolicy(context.Background(), "b")
		if err == nil {
			t.Fatal("expected an error")
		}
		if found {
			t.Fatal("found = true alongside a non-nil error")
		}
	})
}

func TestClientDeleteBucketPolicy(t *testing.T) {
	t.Run("deletes the policy", func(t *testing.T) {
		fs3 := &fakeS3{}
		c := &Client{s3: fs3}
		if err := c.DeleteBucketPolicy(context.Background(), "b"); err != nil {
			t.Fatalf("DeleteBucketPolicy: %v", err)
		}
		if len(fs3.deletePolicyReq) != 1 || *fs3.deletePolicyReq[0].Bucket != "b" {
			t.Fatalf("request = %+v", fs3.deletePolicyReq)
		}
	})

	t.Run("NoSuchBucketPolicy is success", func(t *testing.T) {
		c := &Client{s3: &fakeS3{deletePolicyErr: &smithy.GenericAPIError{Code: "NoSuchBucketPolicy"}}}
		if err := c.DeleteBucketPolicy(context.Background(), "b"); err != nil {
			t.Fatalf("DeleteBucketPolicy: %v", err)
		}
	})

	t.Run("NoSuchBucket is success", func(t *testing.T) {
		c := &Client{s3: &fakeS3{deletePolicyErr: &s3types.NoSuchBucket{}}}
		if err := c.DeleteBucketPolicy(context.Background(), "b"); err != nil {
			t.Fatalf("DeleteBucketPolicy: %v", err)
		}
	})

	t.Run("every other error is real", func(t *testing.T) {
		c := &Client{s3: &fakeS3{deletePolicyErr: errors.New("throttled")}}
		if err := c.DeleteBucketPolicy(context.Background(), "b"); err == nil {
			t.Fatal("expected an error")
		}
	})
}
