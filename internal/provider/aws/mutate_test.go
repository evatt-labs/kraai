package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

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
