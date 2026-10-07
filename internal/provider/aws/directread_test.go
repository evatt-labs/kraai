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

const (
	functionURL    = "AWS::Lambda::Url"
	urlFunctionARN = "arn:aws:lambda:us-east-1:123456789012:function:web"
)

// countingCC answers every GetResource with one function URL, counting the
// calls that reach it.
type countingCC struct {
	cloudControlAPI
	gets atomic.Int32
}

func (c *countingCC) GetResource(context.Context, *cloudcontrol.GetResourceInput, ...func(*cloudcontrol.Options)) (*cloudcontrol.GetResourceOutput, error) {
	c.gets.Add(1)
	return &cloudcontrol.GetResourceOutput{ResourceDescription: &cctypes.ResourceDescription{
		Identifier: aws.String(urlFunctionARN), Properties: aws.String(`{"FunctionUrl":"from-cloud-control"}`),
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
	if !direct.CanRead(functionURL) {
		t.Fatal("AWS::Lambda::Url is not a production direct reader; the evidence or override changed")
	}
	c, cc, directCalls := directClient(t, 200, `{"FunctionArn":"`+urlFunctionARN+`","FunctionUrl":"https://u.lambda-url.us-east-1.on.aws/","AuthType":"NONE","Cors":{"MaxAge":300}}`)
	props, found, err := c.GetResource(context.Background(), functionURL, urlFunctionARN)
	if err != nil || !found {
		t.Fatalf("GetResource = %v, %v, %v", props, found, err)
	}
	want := map[string]any{
		"FunctionArn": urlFunctionARN, "FunctionUrl": "https://u.lambda-url.us-east-1.on.aws/", "AuthType": "NONE",
		"TargetFunctionArn": "web", "Cors": map[string]any{"MaxAge": float64(300)},
	}
	if !reflect.DeepEqual(props, want) {
		t.Fatalf("props = %#v\nwant    %#v", props, want)
	}
	if cc.gets.Load() != 0 || directCalls.Load() != 1 {
		t.Fatalf("Cloud Control calls %d, direct calls %d; want 0 and 1", cc.gets.Load(), directCalls.Load())
	}
	// The answer is cached like one of Cloud Control's.
	if _, _, err := c.GetResource(context.Background(), functionURL, urlFunctionARN); err != nil || directCalls.Load() != 1 {
		t.Fatalf("second read: %v, direct calls %d", err, directCalls.Load())
	}
}

// An error the override says means absent is not found, as Cloud Control
// reports it, without asking Cloud Control.
func TestGetResourceReadsADirectAbsenceAsNotFound(t *testing.T) {
	c, cc, _ := directClient(t, 404, `{"__type":"ResourceNotFoundException","Message":"The resource you requested does not exist."}`)
	props, found, err := c.GetResource(context.Background(), functionURL, urlFunctionARN)
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
		"a service error": {400, `{"__type":"InvalidParameterValueException","Message":"bad function"}`},
		"a throttle":      {429, `{"__type":"TooManyRequestsException"}`},
	} {
		t.Run(name, func(t *testing.T) {
			client, cc, _ := directClient(t, c.status, c.body)
			props, found, err := client.GetResource(context.Background(), functionURL, urlFunctionARN)
			if err != nil || !found || props["FunctionUrl"] != "from-cloud-control" || cc.gets.Load() != 1 {
				t.Fatalf("GetResource = %v, %v, %v with %d Cloud Control calls", props, found, err, cc.gets.Load())
			}
		})
	}
}

// A response missing the structure the reader takes the instance from
// falls back too, rather than reading as an instance with no properties.
func TestGetResourceFallsBackOnAMissingWrapper(t *testing.T) {
	client, cc, directCalls := directClient(t, 200, `{}`)
	if _, found, err := client.GetResource(context.Background(), TypeDynamoDBTable, "web"); err != nil || !found || cc.gets.Load() != 1 || directCalls.Load() != 1 {
		t.Fatalf("found %v, err %v, Cloud Control calls %d, direct calls %d; want one of each", found, err, cc.gets.Load(), directCalls.Load())
	}
}

// A type not proven is read through Cloud Control, never directly.
func TestGetResourceLeavesUnprovenTypesToCloudControl(t *testing.T) {
	const subnet = "AWS::Scheduler::ScheduleGroup"
	if direct.CanRead(subnet) {
		t.Fatal("AWS::Scheduler::ScheduleGroup differs from Cloud Control, so it must not be a production reader")
	}
	c, cc, directCalls := directClient(t, 200, `<never/>`)
	if _, _, err := c.GetResource(context.Background(), subnet, "g"); err != nil || cc.gets.Load() != 1 || directCalls.Load() != 0 {
		t.Fatalf("err %v, Cloud Control calls %d, direct calls %d", err, cc.gets.Load(), directCalls.Load())
	}
}

// A fallback leaves an event on the read's span, naming the type and the
// service's error code, so a direct path that always falls back shows up.
func TestGetResourceRecordsAFallback(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	ctx, span := tp.Tracer("test").Start(context.Background(), "read")
	c, _, _ := directClient(t, 429, `{"__type":"TooManyRequestsException"}`)
	if _, _, err := c.GetResource(ctx, functionURL, urlFunctionARN); err != nil {
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
	if want := map[string]string{"kraai.type": functionURL, "kraai.fallback_reason": "TooManyRequestsException"}; !reflect.DeepEqual(attrs, want) {
		t.Fatalf("attributes = %v, want %v", attrs, want)
	}
}
