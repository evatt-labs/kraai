package aws

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

const taskDefinition = "AWS::ECS::TaskDefinition"

// countingCC answers every GetResource with one task definition, counting
// the calls that reach it.
type countingCC struct {
	cloudControlAPI
	gets atomic.Int32
}

func (c *countingCC) GetResource(context.Context, *cloudcontrol.GetResourceInput, ...func(*cloudcontrol.Options)) (*cloudcontrol.GetResourceOutput, error) {
	c.gets.Add(1)
	return &cloudcontrol.GetResourceOutput{ResourceDescription: &cctypes.ResourceDescription{
		Identifier: aws.String("arn:td"), Properties: aws.String(`{"Family":"from-cloud-control"}`),
	}}, nil
}

func directClient(t *testing.T, status int, body string) (*Client, *countingCC, *atomic.Int32) {
	t.Helper()
	var directCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	cc := &countingCC{}
	return &Client{cc: cc, direct: &direct.Client{
		HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL },
	}}, cc, &directCalls
}

// A type proven to agree with Cloud Control is read through its own
// service, and decoded as a Cloud Control read is: numbers as float64.
func TestGetResourceReadsAProvenTypeDirectly(t *testing.T) {
	if !direct.CanRead(taskDefinition) {
		t.Fatal("AWS::ECS::TaskDefinition is not a production direct reader; the evidence or override changed")
	}
	c, cc, directCalls := directClient(t, 200, `{"taskDefinition":{"family":"web","cpu":"256","revision":3,"status":"ACTIVE"},"tags":[{"key":"k","value":"v"}]}`)
	props, found, err := c.GetResource(context.Background(), taskDefinition, "arn:td")
	if err != nil || !found {
		t.Fatalf("GetResource = %v, %v, %v", props, found, err)
	}
	want := map[string]any{"Family": "web", "Cpu": "256", "Tags": []any{map[string]any{"Key": "k", "Value": "v"}}}
	if !reflect.DeepEqual(props, want) {
		t.Fatalf("props = %#v\nwant    %#v", props, want)
	}
	if cc.gets.Load() != 0 || directCalls.Load() != 1 {
		t.Fatalf("Cloud Control calls %d, direct calls %d; want 0 and 1", cc.gets.Load(), directCalls.Load())
	}
	// The answer is cached like one of Cloud Control's.
	if _, _, err := c.GetResource(context.Background(), taskDefinition, "arn:td"); err != nil || directCalls.Load() != 1 {
		t.Fatalf("second read: %v, direct calls %d", err, directCalls.Load())
	}
}

// A revision the service still describes but the override says is gone
// is not found, as Cloud Control reports it, without asking Cloud Control.
func TestGetResourceReadsADirectAbsenceAsNotFound(t *testing.T) {
	c, cc, _ := directClient(t, 200, `{"taskDefinition":{"family":"web","status":"INACTIVE"}}`)
	props, found, err := c.GetResource(context.Background(), taskDefinition, "arn:td")
	if err != nil || found || props != nil || cc.gets.Load() != 0 {
		t.Fatalf("GetResource = %v, %v, %v with %d Cloud Control calls; want not found, none", props, found, err, cc.gets.Load())
	}
}

// Any other direct failure falls back to Cloud Control: the direct path
// may save a call but never changes an answer.
func TestGetResourceFallsBackToCloudControl(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"a service error": {400, `{"__type":"ClientException","message":"Unable to describe task definition."}`},
		"a throttle":      {429, `{"__type":"ThrottlingException"}`},
		"no resource":     {200, `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			client, cc, _ := directClient(t, c.status, c.body)
			props, found, err := client.GetResource(context.Background(), taskDefinition, "arn:td")
			if err != nil || !found || props["Family"] != "from-cloud-control" || cc.gets.Load() != 1 {
				t.Fatalf("GetResource = %v, %v, %v with %d Cloud Control calls", props, found, err, cc.gets.Load())
			}
		})
	}
}

// A type not proven is read through Cloud Control, never directly.
func TestGetResourceLeavesUnprovenTypesToCloudControl(t *testing.T) {
	const subnet = "AWS::EC2::Subnet"
	if direct.CanRead(subnet) {
		t.Fatal("AWS::EC2::Subnet skips properties, so it must not be a production reader")
	}
	c, cc, directCalls := directClient(t, 200, `<never/>`)
	if _, _, err := c.GetResource(context.Background(), subnet, "subnet-1"); err != nil || cc.gets.Load() != 1 || directCalls.Load() != 0 {
		t.Fatalf("err %v, Cloud Control calls %d, direct calls %d", err, cc.gets.Load(), directCalls.Load())
	}
}

// A fallback leaves an event on the read's span, naming the type and the
// service's error code, so a direct path that always falls back shows up.
func TestGetResourceRecordsAFallback(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	ctx, span := tp.Tracer("test").Start(context.Background(), "read")
	c, _, _ := directClient(t, 429, `{"__type":"ThrottlingException"}`)
	if _, _, err := c.GetResource(ctx, taskDefinition, "arn:td"); err != nil {
		t.Fatal(err)
	}
	span.End()
	events := spans.Ended()[0].Events()
	if len(events) != 1 || events[0].Name != "direct read fell back to Cloud Control" {
		t.Fatalf("events = %v", events)
	}
	attrs := map[string]string{}
	for _, a := range events[0].Attributes {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if want := map[string]string{"kraai.type": taskDefinition, "kraai.fallback_reason": "ThrottlingException"}; !reflect.DeepEqual(attrs, want) {
		t.Fatalf("attributes = %v, want %v", attrs, want)
	}
}
