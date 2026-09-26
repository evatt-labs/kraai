package aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
)

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
