package aws

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"

	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

const dynamoTable = "AWS::DynamoDB::Table"

// unsupportedClient is a client whose DynamoDB table is mutated directly,
// against a service that refuses every call and records which it got, and a
// Cloud Control that succeeds.
func unsupportedClient(t *testing.T) (*Client, *fakeCC, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var ops []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		op := r.Header.Get("X-Amz-Target")
		mu.Lock()
		ops = append(ops, op[strings.LastIndex(op, ".")+1:])
		mu.Unlock()
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"__type":"com.amazonaws.dynamodb.v20120810#ValidationException","message":"refused"}`)
	}))
	t.Cleanup(srv.Close)
	done := &cctypes.ProgressEvent{OperationStatus: cctypes.OperationStatusSuccess, Identifier: aws.String("t"), ResourceModel: aws.String(`{"TableName":"t"}`)}
	cc := &fakeCC{
		createOut: &cloudcontrol.CreateResourceOutput{ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("tok")}},
		updateOut: &cloudcontrol.UpdateResourceOutput{ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("tok")}},
		statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{ProgressEvent: done}},
	}
	c := &Client{cc: cc, direct: &direct.Client{HTTP: srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region:      "us-east-1", Endpoint: func(string) string { return srv.URL }},
		// Stands for direct.CanMutateWith: a property named Marker is one the
		// direct calls cannot set.
		canMutate: func(_ string, props map[string]any) bool { return props["Marker"] == nil }}
	testPollTimings()(c)
	return c, cc, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), ops...) }
}

// A create naming an unsupported property makes no direct call: Cloud
// Control creates it. One that does not goes direct.
func TestCreateNamingAnUnsupportedPropertyFallsBackToCloudControl(t *testing.T) {
	ctx := context.Background()
	for name, desired := range map[string]map[string]any{
		"a top-level property": {"TableName": "t", "Marker": true},
		"a second property":    {"TableName": "t", "Marker": map[string]any{"Nested": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			c, cc, ops := unsupportedClient(t)
			if _, _, err := c.CreateResource(ctx, dynamoTable, desired); err != nil {
				t.Fatalf("CreateResource: %v", err)
			}
			if got := ops(); len(got) != 0 || len(cc.createReq) != 1 {
				t.Fatalf("direct calls %v, Cloud Control creates %d; want none and one", got, len(cc.createReq))
			}
		})
	}
	t.Run("a supported property goes direct", func(t *testing.T) {
		c, cc, ops := unsupportedClient(t)
		desired := map[string]any{"TableName": "t", "TableClass": "STANDARD"}
		if _, _, err := c.CreateResource(ctx, dynamoTable, desired); err == nil {
			t.Fatal("CreateResource succeeded against a service that refuses every call")
		}
		if got := ops(); len(got) != 1 || got[0] != "CreateTable" || len(cc.createReq) != 0 {
			t.Fatalf("direct calls %v, Cloud Control creates %d; want CreateTable and none", got, len(cc.createReq))
		}
	})
}

// An update whose changes name an unsupported property is Cloud Control's,
// before any direct call, and the patch it is sent is the one given.
func TestUpdateNamingAnUnsupportedPropertyFallsBackToCloudControl(t *testing.T) {
	ctx := context.Background()
	patch := `[{"op":"replace","path":"/Marker","value":true},{"op":"replace","path":"/TableClass","value":"STANDARD"}]`
	c, cc, ops := unsupportedClient(t)
	if _, err := c.UpdateResource(ctx, dynamoTable, "t", []byte(patch)); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	if got := ops(); len(got) != 0 || len(cc.updateReq) != 1 || *cc.updateReq[0].PatchDocument != patch {
		t.Fatalf("direct calls %v, Cloud Control updates %v; want none and the patch", got, cc.updateReq)
	}
	t.Run("a supported change goes direct", func(t *testing.T) {
		c, cc, ops := unsupportedClient(t)
		if _, err := c.UpdateResource(ctx, dynamoTable, "t", []byte(`[{"op":"replace","path":"/TableClass","value":"STANDARD"}]`)); err == nil {
			t.Fatal("UpdateResource succeeded against a service that refuses every call")
		}
		if got := ops(); len(got) == 0 || len(cc.updateReq) != 0 {
			t.Fatalf("direct calls %v, Cloud Control updates %d; want direct calls and none", got, len(cc.updateReq))
		}
	})
}

// A type that routes some changes by the instance's state, as an S3
// bucket's tags under ABAC are, reads the instance before deciding; when
// that read fails the change is Cloud Control's, and nothing is written
// directly.
func TestUpdateRoutedByStateGoesToCloudControlWhenTheReadFails(t *testing.T) {
	var mu sync.Mutex
	var writes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if r.Method != http.MethodGet {
			mu.Lock()
			writes++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>refused</Message></Error>`)
	}))
	t.Cleanup(srv.Close)
	done := &cctypes.ProgressEvent{OperationStatus: cctypes.OperationStatusSuccess, Identifier: aws.String("b"), ResourceModel: aws.String(`{"BucketName":"b"}`)}
	cc := &fakeCC{
		updateOut: &cloudcontrol.UpdateResourceOutput{ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("tok")}},
		statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{ProgressEvent: done}},
	}
	c := &Client{cc: cc, direct: &direct.Client{HTTP: srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region:      "us-east-1", Endpoint: func(string) string { return srv.URL }},
		canMutate: func(string, map[string]any) bool { return true }}
	testPollTimings()(c)
	patch := `[{"op":"replace","path":"/Tags","value":[{"Key":"team","Value":"core"}]}]`
	if _, err := c.UpdateResource(context.Background(), TypeS3Bucket, "b", []byte(patch)); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	if len(cc.updateReq) != 1 || writes != 0 {
		t.Fatalf("Cloud Control updates %d, direct writes %d; want the update sent to Cloud Control and nothing written directly", len(cc.updateReq), writes)
	}
}
