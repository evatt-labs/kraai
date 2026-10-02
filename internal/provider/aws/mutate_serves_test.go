package aws

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"

	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

const gatewayAttachment = "AWS::EC2::VPCGatewayAttachment"

// servesClient is a client whose gateway attachments are mutated directly,
// against an EC2 that records the actions it got and answers each as
// refused, and a Cloud Control that succeeds.
func servesClient(t *testing.T) (*Client, *fakeCC, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		mu.Lock()
		actions = append(actions, form.Get("Action"))
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `<Response><Errors><Error><Code>Refused</Code><Message>no</Message></Error></Errors></Response>`)
	}))
	t.Cleanup(srv.Close)
	done := &cctypes.ProgressEvent{OperationStatus: cctypes.OperationStatusSuccess, Identifier: aws.String("VPN|vpc-1"), ResourceModel: aws.String(`{}`)}
	token := &cctypes.ProgressEvent{RequestToken: aws.String("tok")}
	cc := &fakeCC{
		updateOut: &cloudcontrol.UpdateResourceOutput{ProgressEvent: token},
		deleteOut: &cloudcontrol.DeleteResourceOutput{ProgressEvent: token},
		statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{ProgressEvent: done}},
	}
	c := &Client{cc: cc, direct: &direct.Client{HTTP: srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region:      "us-east-1", Endpoint: func(string) string { return srv.URL }},
		// Any type is mutated directly, as a proven one is.
		canMutate: func(string, map[string]any) bool { return true }}
	testPollTimings()(c)
	return c, cc, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), actions...) }
}

// An instance whose identifier names a value the type's direct reader does
// not serve, such as a VPN gateway's attachment, is updated and deleted by
// Cloud Control: the direct read finds nothing for it, which would take a
// delete for one that is already done.
func TestAnIdentifierTheReaderDoesNotServeIsCloudControls(t *testing.T) {
	ctx := context.Background()
	const patch = `[{"op":"replace","path":"/VpnGatewayId","value":"vgw-2"}]`

	c, cc, actions := servesClient(t)
	if err := c.DeleteResource(ctx, gatewayAttachment, "VPN|vpc-1"); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if got := actions(); len(got) != 0 || len(cc.deleteReq) != 1 {
		t.Fatalf("direct calls %v, Cloud Control deletes %d; want none and one", got, len(cc.deleteReq))
	}
	if _, err := c.UpdateResource(ctx, gatewayAttachment, "VPN|vpc-1", []byte(patch)); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	if got := actions(); len(got) != 0 || len(cc.updateReq) != 1 {
		t.Fatalf("direct calls %v, Cloud Control updates %d; want none and one", got, len(cc.updateReq))
	}

	t.Run("an internet gateway's attachment goes direct", func(t *testing.T) {
		c, cc, actions := servesClient(t)
		if err := c.DeleteResource(ctx, gatewayAttachment, "IGW|vpc-1"); err == nil {
			t.Fatal("DeleteResource succeeded against a service that refuses every call")
		}
		if got := actions(); len(got) == 0 || len(cc.deleteReq) != 0 {
			t.Fatalf("direct calls %v, Cloud Control deletes %d; want direct calls and none", got, len(cc.deleteReq))
		}
	})
}
