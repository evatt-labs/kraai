package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// testPollTimings makes the poll loop resolve near-instantly: a
// microsecond initial/max delay and a timeout generous enough for a
// handful of iterations, but never so long that a genuinely stuck test
// makes the suite hang.
func testPollTimings() Option {
	return WithPollTimings(time.Microsecond, time.Microsecond, 200*time.Millisecond)
}

// fakeCC is a hand-rolled cloudControlAPI: no AWS account, no network needed.
type fakeCC struct {
	getOut  *cloudcontrol.GetResourceOutput
	getErr  error
	listOut []*cloudcontrol.ListResourcesOutput
	listErr error
	listAt  int
	gotReq  []*cloudcontrol.GetResourceInput
	listReq []*cloudcontrol.ListResourcesInput

	createOut *cloudcontrol.CreateResourceOutput
	createErr error
	createReq []*cloudcontrol.CreateResourceInput

	updateOut *cloudcontrol.UpdateResourceOutput
	updateErr error
	updateReq []*cloudcontrol.UpdateResourceInput

	deleteOut *cloudcontrol.DeleteResourceOutput
	deleteErr error
	deleteReq []*cloudcontrol.DeleteResourceInput

	// statusOut is a sequence of responses returned in order, one per
	// GetResourceRequestStatus call, so a test can script
	// PENDING -> IN_PROGRESS -> SUCCESS/FAILED. The last entry repeats once
	// exhausted, the same pattern listOut/listAt already use.
	statusOut   []*cloudcontrol.GetResourceRequestStatusOutput
	statusErr   error
	statusAt    int
	statusCalls int
}

func (f *fakeCC) GetResource(_ context.Context, params *cloudcontrol.GetResourceInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.GetResourceOutput, error) {
	f.gotReq = append(f.gotReq, params)
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getOut, nil
}

func (f *fakeCC) ListResources(_ context.Context, params *cloudcontrol.ListResourcesInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.ListResourcesOutput, error) {
	f.listReq = append(f.listReq, params)
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := f.listOut[f.listAt]
	if f.listAt < len(f.listOut)-1 {
		f.listAt++
	}
	return out, nil
}

func (f *fakeCC) CreateResource(_ context.Context, params *cloudcontrol.CreateResourceInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.CreateResourceOutput, error) {
	f.createReq = append(f.createReq, params)
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createOut, nil
}

func (f *fakeCC) UpdateResource(_ context.Context, params *cloudcontrol.UpdateResourceInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.UpdateResourceOutput, error) {
	f.updateReq = append(f.updateReq, params)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updateOut, nil
}

func (f *fakeCC) DeleteResource(_ context.Context, params *cloudcontrol.DeleteResourceInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.DeleteResourceOutput, error) {
	f.deleteReq = append(f.deleteReq, params)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return f.deleteOut, nil
}

func (f *fakeCC) GetResourceRequestStatus(ctx context.Context, _ *cloudcontrol.GetResourceRequestStatusInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.GetResourceRequestStatusOutput, error) {
	f.statusCalls++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if len(f.statusOut) == 0 {
		return &cloudcontrol.GetResourceRequestStatusOutput{}, nil
	}
	out := f.statusOut[f.statusAt]
	if f.statusAt < len(f.statusOut)-1 {
		f.statusAt++
	}
	// Give a stuck-forever test (no terminal status ever scripted) a chance
	// to observe context cancellation instead of spinning until the
	// suite's own test timeout.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, nil
}

type fakeCF struct {
	out *cloudformation.DescribeTypeOutput
	err error
}

func (f *fakeCF) DescribeType(context.Context, *cloudformation.DescribeTypeInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeTypeOutput, error) {
	return f.out, f.err
}

func TestClientGetResource(t *testing.T) {
	t.Run("decodes properties", func(t *testing.T) {
		cc := &fakeCC{getOut: &cloudcontrol.GetResourceOutput{
			ResourceDescription: &cctypes.ResourceDescription{
				Identifier: aws.String("my-bucket"),
				Properties: aws.String(`{"BucketName":"my-bucket","Arn":"arn:aws:s3:::my-bucket"}`),
			},
		}}
		c := &Client{cc: cc}

		props, found, err := c.GetResource(context.Background(), TypeS3Bucket, "my-bucket")
		if err != nil || !found {
			t.Fatalf("GetResource: found=%v err=%v", found, err)
		}
		if props["BucketName"] != "my-bucket" {
			t.Fatalf("properties = %+v", props)
		}
		if got := *cc.gotReq[0].TypeName; got != TypeS3Bucket {
			t.Fatalf("TypeName = %q", got)
		}
	})

	t.Run("translates ResourceNotFoundException to absence, not error", func(t *testing.T) {
		cc := &fakeCC{getErr: &cctypes.ResourceNotFoundException{Message: aws.String("gone")}}
		c := &Client{cc: cc}

		props, found, err := c.GetResource(context.Background(), TypeS3Bucket, "missing")
		if err != nil {
			t.Fatalf("expected no error for a not-found resource, got %v", err)
		}
		if found || props != nil {
			t.Fatalf("found=%v props=%v, want absence", found, props)
		}
	})

	t.Run("a non-not-found error is never read as absence", func(t *testing.T) {
		// This is the bar the read-only Get contract exists to hold: an
		// outage must never look like "already deleted" to a caller, because
		// teardown treats absence as success.
		cc := &fakeCC{getErr: &cctypes.ThrottlingException{Message: aws.String("slow down")}}
		c := &Client{cc: cc}

		props, found, err := c.GetResource(context.Background(), TypeS3Bucket, "x")
		if err == nil {
			t.Fatal("expected a throttling error to be reported, not swallowed")
		}
		if found || props != nil {
			t.Fatalf("found=%v props=%v, want no result alongside the error", found, props)
		}
	})

	t.Run("a nil ResourceDescription is an error, not absence", func(t *testing.T) {
		cc := &fakeCC{getOut: &cloudcontrol.GetResourceOutput{}}
		c := &Client{cc: cc}

		_, found, err := c.GetResource(context.Background(), TypeS3Bucket, "x")
		if err == nil || found {
			t.Fatalf("found=%v err=%v, want an error", found, err)
		}
	})

	t.Run("malformed properties JSON is an error", func(t *testing.T) {
		cc := &fakeCC{getOut: &cloudcontrol.GetResourceOutput{
			ResourceDescription: &cctypes.ResourceDescription{
				Identifier: aws.String("x"),
				Properties: aws.String(`{not json`),
			},
		}}
		c := &Client{cc: cc}

		_, _, err := c.GetResource(context.Background(), TypeS3Bucket, "x")
		if err == nil {
			t.Fatal("expected a decode error")
		}
	})
}

func TestClientListResources(t *testing.T) {
	t.Run("collects identifiers across pages", func(t *testing.T) {
		cc := &fakeCC{listOut: []*cloudcontrol.ListResourcesOutput{
			{
				ResourceDescriptions: []cctypes.ResourceDescription{
					{Identifier: aws.String("a")}, {Identifier: aws.String("b")},
				},
				NextToken: aws.String("page-2"),
			},
			{
				ResourceDescriptions: []cctypes.ResourceDescription{{Identifier: aws.String("c")}},
			},
		}}
		c := &Client{cc: cc}

		ids, err := c.ListResources(context.Background(), TypeLambdaFunction, nil)
		if err != nil {
			t.Fatalf("ListResources: %v", err)
		}
		want := []string{"a", "b", "c"}
		if len(ids) != len(want) {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
		for i, id := range want {
			if ids[i] != id {
				t.Fatalf("ids[%d] = %q, want %q", i, ids[i], id)
			}
		}
		if len(cc.listReq) != 2 || *cc.listReq[1].NextToken != "page-2" {
			t.Fatalf("second page request = %+v", cc.listReq)
		}
		if cc.listReq[0].ResourceModel != nil {
			t.Fatalf("ResourceModel = %v, want nil for an unscoped list", *cc.listReq[0].ResourceModel)
		}
	})

	t.Run("a non-nil resourceModel is marshaled onto the request", func(t *testing.T) {
		cc := &fakeCC{listOut: []*cloudcontrol.ListResourcesOutput{{
			ResourceDescriptions: []cctypes.ResourceDescription{{Identifier: aws.String("perm1")}},
		}}}
		c := &Client{cc: cc}

		_, err := c.ListResources(context.Background(), realTypeLambdaPermission, map[string]any{"FunctionName": "myenv-api"})
		if err != nil {
			t.Fatalf("ListResources: %v", err)
		}
		if len(cc.listReq) != 1 || cc.listReq[0].ResourceModel == nil {
			t.Fatalf("listReq = %+v, want exactly one request carrying a ResourceModel", cc.listReq)
		}
		if got, want := *cc.listReq[0].ResourceModel, `{"FunctionName":"myenv-api"}`; got != want {
			t.Fatalf("ResourceModel = %q, want %q", got, want)
		}
	})

	t.Run("TypeNotFoundException is reported, not treated as empty", func(t *testing.T) {
		cc := &fakeCC{listErr: &cctypes.TypeNotFoundException{Message: aws.String("no such type")}}
		c := &Client{cc: cc}

		ids, err := c.ListResources(context.Background(), "AWS::Bogus::Type", nil)
		if err == nil {
			t.Fatal("expected an error")
		}
		if ids != nil {
			t.Fatalf("ids = %v, want nil alongside the error", ids)
		}
	})

	t.Run("another error is not treated as empty either", func(t *testing.T) {
		cc := &fakeCC{listErr: &cctypes.ThrottlingException{Message: aws.String("slow down")}}
		c := &Client{cc: cc}

		if _, err := c.ListResources(context.Background(), TypeLambdaFunction, nil); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("a ResourceNotFoundException for an unscoped list is still a real error", func(t *testing.T) {
		// Only ever observed for a scoped (parent-requiring) list in a real
		// account — see Client.ListResources's own doc comment. An unscoped
		// list returning it is unexpected and must not be quietly read as
		// "nothing exists".
		cc := &fakeCC{listErr: &cctypes.ResourceNotFoundException{Message: aws.String("not found")}}
		c := &Client{cc: cc}

		ids, err := c.ListResources(context.Background(), TypeLambdaFunction, nil)
		if err == nil {
			t.Fatal("expected an error for an unscoped list's ResourceNotFoundException")
		}
		if ids != nil {
			t.Fatalf("ids = %v, want nil alongside the error", ids)
		}
	})

	t.Run("a ResourceNotFoundException for a scoped list is absence, not an error", func(t *testing.T) {
		// Verified against a live account: listing AWS::Lambda::Permission
		// scoped to a FunctionName that does not exist returns
		// ResourceNotFoundException, not an empty result — see
		// Client.ListResources's own doc comment for the real API response
		// this reproduces. The parent's absence must translate to the
		// permission's own absence, the same (nil, nil) contract Get
		// promises everywhere else.
		cc := &fakeCC{listErr: &cctypes.ResourceNotFoundException{
			Message: aws.String("AWS::Lambda::Permission Handler returned status FAILED: The resource you requested does not exist."),
		}}
		c := &Client{cc: cc}

		ids, err := c.ListResources(context.Background(), realTypeLambdaPermission, map[string]any{"FunctionName": "does-not-exist"})
		if err != nil {
			t.Fatalf("ListResources: %v, want nil error for a scoped list's absent parent", err)
		}
		if ids != nil {
			t.Fatalf("ids = %v, want nil", ids)
		}
	})

	t.Run("a NextToken that never stops is bounded, not an infinite loop", func(t *testing.T) {
		cc := &fakeCC{listOut: []*cloudcontrol.ListResourcesOutput{{
			ResourceDescriptions: []cctypes.ResourceDescription{{Identifier: aws.String("a")}},
			NextToken:            aws.String("always-more"),
		}}}
		c := &Client{cc: cc}

		_, err := c.ListResources(context.Background(), TypeLambdaFunction, nil)
		if err == nil {
			t.Fatal("expected the page walk to be bounded and report an error")
		}
		if len(cc.listReq) != maxListPages {
			t.Fatalf("made %d requests, want exactly %d", len(cc.listReq), maxListPages)
		}
	})
}

func TestClientDescribeType(t *testing.T) {
	t.Run("decodes the schema fields this package uses", func(t *testing.T) {
		cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{
			Schema: aws.String(`{"primaryIdentifier":["/properties/Id"],"createOnlyProperties":["/properties/DistributionConfig"]}`),
		}}
		c := &Client{cf: cf}

		schema, err := c.DescribeType(context.Background(), TypeCloudFrontDistribution)
		if err != nil {
			t.Fatalf("DescribeType: %v", err)
		}
		if len(schema.PrimaryIdentifier) != 1 || schema.PrimaryIdentifier[0] != "/properties/Id" {
			t.Fatalf("PrimaryIdentifier = %v", schema.PrimaryIdentifier)
		}
		if len(schema.CreateOnly) != 1 {
			t.Fatalf("CreateOnlyProperties = %v", schema.CreateOnly)
		}
	})

	t.Run("TypeNotFoundException is a validation error", func(t *testing.T) {
		cf := &fakeCF{err: &cftypes.TypeNotFoundException{Message: aws.String("no such type")}}
		c := &Client{cf: cf}

		_, err := c.DescribeType(context.Background(), "AWS::Bogus::Type")
		if err == nil {
			t.Fatal("expected an error")
		}
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
			t.Fatalf("err = %v, want a CodeValidation KError", err)
		}
	})

	t.Run("another error is wrapped, not swallowed", func(t *testing.T) {
		cf := &fakeCF{err: errors.New("boom")}
		c := &Client{cf: cf}

		if _, err := c.DescribeType(context.Background(), TypeS3Bucket); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("a nil schema is an error", func(t *testing.T) {
		cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{}}
		c := &Client{cf: cf}

		if _, err := c.DescribeType(context.Background(), TypeS3Bucket); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("malformed schema JSON is an error", func(t *testing.T) {
		cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{Schema: aws.String(`{not json`)}}
		c := &Client{cf: cf}

		if _, err := c.DescribeType(context.Background(), TypeS3Bucket); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestNew(t *testing.T) {
	t.Run("applies options over the default SDK-backed clients", func(t *testing.T) {
		cc := &fakeCC{getOut: &cloudcontrol.GetResourceOutput{
			ResourceDescription: &cctypes.ResourceDescription{Properties: aws.String(`{"ok":true}`)},
		}}
		cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{Schema: aws.String(`{}`)}}

		c, err := New(context.Background(), Settings{Region: "us-east-1"}, WithCloudControlAPI(cc), WithCloudFormationAPI(cf))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, _, err := c.GetResource(context.Background(), TypeS3Bucket, "x"); err != nil {
			t.Fatalf("GetResource through the injected fake: %v", err)
		}
		if _, err := c.DescribeType(context.Background(), TypeS3Bucket); err != nil {
			t.Fatalf("DescribeType through the injected fake: %v", err)
		}
	})

	t.Run("WithS3API and WithSTSAPI substitute their clients", func(t *testing.T) {
		fs3 := &fakeS3{}
		fsts := &fakeSTS{account: "123456789012"}

		c, err := New(context.Background(), Settings{Region: "us-east-1"}, WithS3API(fs3), WithSTSAPI(fsts))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := c.PutObject(context.Background(), "b", "k", []byte("x")); err != nil {
			t.Fatalf("PutObject through the injected fake: %v", err)
		}
		if _, err := c.AccountID(context.Background()); err != nil {
			t.Fatalf("AccountID through the injected fake: %v", err)
		}
	})

	t.Run("empty region defers to the SDK's own default chain instead of failing", func(t *testing.T) {
		// No manifest region, and nothing to force LoadDefaultConfig itself
		// to fail (unlike the malformed-shared-config subtest below): this
		// proves settings.Region == "" is a legitimate "let the SDK decide"
		// signal, not an error condition New has to reject.
		if _, err := New(context.Background(), Settings{}); err != nil {
			t.Fatalf("New with an empty region: %v", err)
		}
	})

	t.Run("a config load failure is reported, not swallowed", func(t *testing.T) {
		// A malformed shared config file is a deterministic way to make the
		// SDK's own LoadDefaultConfig fail without touching the network or
		// real credentials.
		dir := t.TempDir()
		badConfig := filepath.Join(dir, "config")
		if err := os.WriteFile(badConfig, []byte("[profile broken\nkey = value with no closing bracket above"), 0o600); err != nil {
			t.Fatalf("writing fixture: %v", err)
		}
		t.Setenv("AWS_CONFIG_FILE", badConfig)
		t.Setenv("AWS_SDK_LOAD_CONFIG", "1")
		t.Setenv("AWS_PROFILE", "broken")

		if _, err := New(context.Background(), Settings{Region: "us-east-1"}); err == nil {
			t.Fatal("expected a config-loading error to be reported")
		}
	})
}

func TestClientCreateResource(t *testing.T) {
	t.Run("submits desired state and returns the created identifier and properties", func(t *testing.T) {
		cc := &fakeCC{
			createOut: &cloudcontrol.CreateResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{
				ProgressEvent: &cctypes.ProgressEvent{
					OperationStatus: cctypes.OperationStatusSuccess,
					Identifier:      aws.String("my-bucket"),
					ResourceModel:   aws.String(`{"BucketName":"my-bucket"}`),
				},
			}},
		}
		c := &Client{cc: cc}
		testPollTimings()(c)

		id, props, err := c.CreateResource(context.Background(), TypeS3Bucket, map[string]any{"BucketName": "my-bucket"})
		if err != nil {
			t.Fatalf("CreateResource: %v", err)
		}
		if id != "my-bucket" || props["BucketName"] != "my-bucket" {
			t.Fatalf("id=%q props=%+v", id, props)
		}
		// The type is now one a lagging index cannot be trusted about.
		if !c.Created(TypeS3Bucket) || c.Created(typeECSTaskDefinition) {
			t.Fatalf("Created(bucket) = %v, Created(task definition) = %v", c.Created(TypeS3Bucket), c.Created(typeECSTaskDefinition))
		}
		if len(cc.createReq) != 1 || *cc.createReq[0].TypeName != TypeS3Bucket {
			t.Fatalf("createReq = %+v", cc.createReq)
		}
	})

	t.Run("polls through PENDING and IN_PROGRESS before succeeding", func(t *testing.T) {
		cc := &fakeCC{
			createOut: &cloudcontrol.CreateResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{
				{ProgressEvent: &cctypes.ProgressEvent{OperationStatus: cctypes.OperationStatusPending}},
				{ProgressEvent: &cctypes.ProgressEvent{OperationStatus: cctypes.OperationStatusInProgress}},
				{ProgressEvent: &cctypes.ProgressEvent{
					OperationStatus: cctypes.OperationStatusSuccess,
					Identifier:      aws.String("id-1"),
				}},
			},
		}
		c := &Client{cc: cc}
		testPollTimings()(c)

		id, _, err := c.CreateResource(context.Background(), TypeS3Bucket, map[string]any{})
		if err != nil {
			t.Fatalf("CreateResource: %v", err)
		}
		if id != "id-1" {
			t.Fatalf("id = %q", id)
		}
		if cc.statusCalls != 3 {
			t.Fatalf("statusCalls = %d, want 3", cc.statusCalls)
		}
	})

	t.Run("a validation-shaped failure maps to CodeValidation", func(t *testing.T) {
		cc := &fakeCC{
			createOut: &cloudcontrol.CreateResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{
				ProgressEvent: &cctypes.ProgressEvent{
					OperationStatus: cctypes.OperationStatusFailed,
					ErrorCode:       cctypes.HandlerErrorCodeAlreadyExists,
					StatusMessage:   aws.String("a bucket with this name already exists"),
				},
			}},
		}
		c := &Client{cc: cc}
		testPollTimings()(c)

		_, _, err := c.CreateResource(context.Background(), TypeS3Bucket, map[string]any{})
		if err == nil {
			t.Fatal("expected a failure")
		}
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
			t.Fatalf("err = %v, want CodeValidation", err)
		}
	})

	t.Run("an unrecognized failure maps to CodeUnexpected", func(t *testing.T) {
		cc := &fakeCC{
			createOut: &cloudcontrol.CreateResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{
				ProgressEvent: &cctypes.ProgressEvent{
					OperationStatus: cctypes.OperationStatusFailed,
					ErrorCode:       cctypes.HandlerErrorCodeInternalFailure,
				},
			}},
		}
		c := &Client{cc: cc}
		testPollTimings()(c)

		_, _, err := c.CreateResource(context.Background(), TypeS3Bucket, map[string]any{})
		if err == nil {
			t.Fatal("expected a failure")
		}
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeUnexpected {
			t.Fatalf("err = %v, want CodeUnexpected", err)
		}
	})

	t.Run("a CreateResource call failure is reported, not swallowed", func(t *testing.T) {
		cc := &fakeCC{createErr: errors.New("throttled")}
		c := &Client{cc: cc}

		if _, _, err := c.CreateResource(context.Background(), TypeS3Bucket, map[string]any{}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("no request token in the response is an error", func(t *testing.T) {
		cc := &fakeCC{createOut: &cloudcontrol.CreateResourceOutput{}}
		c := &Client{cc: cc}

		if _, _, err := c.CreateResource(context.Background(), TypeS3Bucket, map[string]any{}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("polling never resolving is bounded by the timeout, not an infinite loop", func(t *testing.T) {
		cc := &fakeCC{
			createOut: &cloudcontrol.CreateResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{
				{ProgressEvent: &cctypes.ProgressEvent{OperationStatus: cctypes.OperationStatusInProgress}},
			},
		}
		c := &Client{cc: cc}
		c.pollInitialDelay, c.pollMaxDelay, c.pollTimeout = time.Millisecond, time.Millisecond, 20*time.Millisecond

		start := time.Now()
		_, _, err := c.CreateResource(context.Background(), TypeS3Bucket, map[string]any{})
		if err == nil {
			t.Fatal("expected the poll timeout to produce an error")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("took %v, want it bounded near the 20ms timeout", elapsed)
		}
	})

	t.Run("context cancellation mid-poll stops the loop and is reported", func(t *testing.T) {
		cc := &fakeCC{
			createOut: &cloudcontrol.CreateResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{
				{ProgressEvent: &cctypes.ProgressEvent{OperationStatus: cctypes.OperationStatusInProgress}},
			},
		}
		c := &Client{cc: cc}
		c.pollInitialDelay, c.pollMaxDelay, c.pollTimeout = time.Millisecond, time.Millisecond, time.Minute

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		_, _, err := c.CreateResource(ctx, TypeS3Bucket, map[string]any{})
		if err == nil {
			t.Fatal("expected cancellation to produce an error")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %v, want it to stop promptly after cancellation", elapsed)
		}
	})
}

func TestClientUpdateResource(t *testing.T) {
	t.Run("submits the patch and returns updated properties", func(t *testing.T) {
		cc := &fakeCC{
			updateOut: &cloudcontrol.UpdateResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{
				ProgressEvent: &cctypes.ProgressEvent{
					OperationStatus: cctypes.OperationStatusSuccess,
					ResourceModel:   aws.String(`{"BucketName":"my-bucket","Tags":[]}`),
				},
			}},
		}
		c := &Client{cc: cc}
		testPollTimings()(c)

		props, err := c.UpdateResource(context.Background(), TypeS3Bucket, "my-bucket", []byte(`[{"op":"replace","path":"/Tags","value":[]}]`))
		if err != nil {
			t.Fatalf("UpdateResource: %v", err)
		}
		if props["BucketName"] != "my-bucket" {
			t.Fatalf("props = %+v", props)
		}
		if len(cc.updateReq) != 1 || *cc.updateReq[0].Identifier != "my-bucket" {
			t.Fatalf("updateReq = %+v", cc.updateReq)
		}
	})

	t.Run("a failure is mapped through translateFailure", func(t *testing.T) {
		cc := &fakeCC{
			updateOut: &cloudcontrol.UpdateResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{
				ProgressEvent: &cctypes.ProgressEvent{
					OperationStatus: cctypes.OperationStatusFailed,
					ErrorCode:       cctypes.HandlerErrorCodeNotUpdatable,
				},
			}},
		}
		c := &Client{cc: cc}
		testPollTimings()(c)

		if _, err := c.UpdateResource(context.Background(), TypeS3Bucket, "x", []byte(`[]`)); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("an UpdateResource call failure is reported, not swallowed", func(t *testing.T) {
		cc := &fakeCC{updateErr: errors.New("throttled")}
		c := &Client{cc: cc}

		if _, err := c.UpdateResource(context.Background(), TypeS3Bucket, "x", []byte(`[]`)); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("no request token in the response is an error", func(t *testing.T) {
		cc := &fakeCC{updateOut: &cloudcontrol.UpdateResourceOutput{}}
		c := &Client{cc: cc}

		if _, err := c.UpdateResource(context.Background(), TypeS3Bucket, "x", []byte(`[]`)); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("malformed ResourceModel JSON is an error", func(t *testing.T) {
		cc := &fakeCC{
			updateOut: &cloudcontrol.UpdateResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{
				ProgressEvent: &cctypes.ProgressEvent{
					OperationStatus: cctypes.OperationStatusSuccess,
					ResourceModel:   aws.String(`{not json`),
				},
			}},
		}
		c := &Client{cc: cc}
		testPollTimings()(c)

		if _, err := c.UpdateResource(context.Background(), TypeS3Bucket, "x", []byte(`[]`)); err == nil {
			t.Fatal("expected a decode error")
		}
	})
}

func TestClientDeleteResource(t *testing.T) {
	t.Run("polls to success", func(t *testing.T) {
		cc := &fakeCC{
			deleteOut: &cloudcontrol.DeleteResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{
				ProgressEvent: &cctypes.ProgressEvent{OperationStatus: cctypes.OperationStatusSuccess},
			}},
		}
		c := &Client{cc: cc}
		testPollTimings()(c)

		if err := c.DeleteResource(context.Background(), TypeS3Bucket, "my-bucket"); err != nil {
			t.Fatalf("DeleteResource: %v", err)
		}
	})

	t.Run("a synchronous ResourceNotFoundException is success, not an error", func(t *testing.T) {
		cc := &fakeCC{deleteErr: &cctypes.ResourceNotFoundException{Message: aws.String("gone")}}
		c := &Client{cc: cc}

		if err := c.DeleteResource(context.Background(), TypeS3Bucket, "missing"); err != nil {
			t.Fatalf("DeleteResource: %v, want nil for an already-absent resource", err)
		}
	})

	t.Run("an async terminal NotFound is success, not an error", func(t *testing.T) {
		// The delete handler itself discovered the resource was already
		// gone by the time it ran — same absence-is-success contract as the
		// synchronous exception case above, reached through the async path
		// instead.
		cc := &fakeCC{
			deleteOut: &cloudcontrol.DeleteResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{
				ProgressEvent: &cctypes.ProgressEvent{
					OperationStatus: cctypes.OperationStatusFailed,
					ErrorCode:       cctypes.HandlerErrorCodeNotFound,
				},
			}},
		}
		c := &Client{cc: cc}
		testPollTimings()(c)

		if err := c.DeleteResource(context.Background(), TypeS3Bucket, "vanished"); err != nil {
			t.Fatalf("DeleteResource: %v, want nil for an already-absent resource", err)
		}
	})

	t.Run("a real failure is still reported", func(t *testing.T) {
		cc := &fakeCC{
			deleteOut: &cloudcontrol.DeleteResourceOutput{
				ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")},
			},
			statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{
				ProgressEvent: &cctypes.ProgressEvent{
					OperationStatus: cctypes.OperationStatusFailed,
					ErrorCode:       cctypes.HandlerErrorCodeAccessDenied,
				},
			}},
		}
		c := &Client{cc: cc}
		testPollTimings()(c)

		if err := c.DeleteResource(context.Background(), TypeS3Bucket, "x"); err == nil {
			t.Fatal("expected an access-denied failure to be reported")
		}
	})

	t.Run("a DeleteResource call failure other than not-found is reported", func(t *testing.T) {
		cc := &fakeCC{deleteErr: errors.New("throttled")}
		c := &Client{cc: cc}

		if err := c.DeleteResource(context.Background(), TypeS3Bucket, "x"); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("no request token in the response is an error", func(t *testing.T) {
		cc := &fakeCC{deleteOut: &cloudcontrol.DeleteResourceOutput{}}
		c := &Client{cc: cc}

		if err := c.DeleteResource(context.Background(), TypeS3Bucket, "x"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

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

// fakeSTS is a hand-rolled stsAPI: no AWS account, no network needed.
type fakeSTS struct {
	account string
	err     error
	calls   int
}

func (f *fakeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account)}, nil
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

func TestClientAccountID(t *testing.T) {
	t.Run("resolves and caches", func(t *testing.T) {
		fsts := &fakeSTS{account: "123456789012"}
		c := &Client{sts: fsts}

		id, err := c.AccountID(context.Background())
		if err != nil || id != "123456789012" {
			t.Fatalf("AccountID = %q, err %v", id, err)
		}
		if _, err := c.AccountID(context.Background()); err != nil {
			t.Fatalf("second AccountID call: %v", err)
		}
		if fsts.calls != 1 {
			t.Fatalf("STS called %d times, want exactly 1 (cached after first success)", fsts.calls)
		}
	})

	t.Run("a transient failure is not cached", func(t *testing.T) {
		fsts := &fakeSTS{err: errors.New("throttled")}
		c := &Client{sts: fsts}

		if _, err := c.AccountID(context.Background()); err == nil {
			t.Fatal("expected an error")
		}
		fsts.err = nil
		fsts.account = "999999999999"
		id, err := c.AccountID(context.Background())
		if err != nil || id != "999999999999" {
			t.Fatalf("retry after failure: id=%q err=%v", id, err)
		}
	})

	t.Run("no account id in the response is an error", func(t *testing.T) {
		c := &Client{sts: &fakeSTS{account: ""}}
		if _, err := c.AccountID(context.Background()); err == nil {
			t.Fatal("expected an error for an empty account id")
		}
	})
}

func TestClientRegion(t *testing.T) {
	c := &Client{region: "us-west-2"}
	if got := c.Region(); got != "us-west-2" {
		t.Fatalf("Region() = %q, want %q", got, "us-west-2")
	}
}

func TestClientPollTimingsDefaults(t *testing.T) {
	// A Client built by struct literal (every existing test in this
	// package does exactly that) has zero-value poll fields; pollTimings
	// must fall back to the production defaults rather than let the poll
	// loop busy-spin with no delay at all.
	c := &Client{}
	initial, maxDelay, timeout := c.pollTimings()
	if initial != defaultPollInitialDelay || maxDelay != defaultPollMaxDelay || timeout != defaultPollTimeout {
		t.Fatalf("pollTimings() = (%v, %v, %v), want the defaults", initial, maxDelay, timeout)
	}
}

func TestClientSecretValue(t *testing.T) {
	c := &Client{sm: &fakeSecretsManager{value: `{"username":"u","password":"p"}`}}
	value, err := c.SecretValue(context.Background(), "arn:secret")
	if err != nil || value != `{"username":"u","password":"p"}` {
		t.Fatalf("SecretValue = %q, %v", value, err)
	}
	empty := &Client{sm: &fakeSecretsManager{value: ""}}
	if _, err := empty.SecretValue(context.Background(), "arn:secret"); err == nil {
		t.Fatal("SecretValue(empty) succeeded, want an error")
	}
	failing := &Client{sm: &fakeSecretsManager{err: errors.New("denied")}}
	if _, err := failing.SecretValue(context.Background(), "arn:secret"); err == nil || !strings.Contains(err.Error(), "arn:secret") {
		t.Fatalf("SecretValue(failure): err = %v, want an error carrying no value", err)
	}
}
