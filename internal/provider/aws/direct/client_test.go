package direct

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// seen is one request as the server received it.
type seen struct {
	method, rawPath, target, contentType, auth string
	body                                       map[string]any
}

// serve answers every request with status and body, recording what it saw.
func serve(t *testing.T, status int, header http.Header, response string) (*Client, *seen) {
	t.Helper()
	got := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.rawPath = r.Method, r.URL.EscapedPath()
		got.target, got.contentType, got.auth = r.Header.Get("X-Amz-Target"), r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &got.body)
		}
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP:        srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region:      "us-east-1",
		Endpoint:    func(string, string) string { return srv.URL },
		Now:         func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	}, got
}

func TestRequestsAreShapedPerProtocol(t *testing.T) {
	arn := "arn:aws:bedrock:us-east-1:123456789012:default-prompt-router/anthropic.claude:1"
	cases := map[string]struct {
		typeName    string
		identifier  map[string]string
		response    string
		method      string
		rawPath     string
		target      string
		contentType string
		body        map[string]any
		scope       string
	}{
		"restJson1 path label": {
			typeName: "AWS::AppConfig::DeploymentStrategy", identifier: map[string]string{"Id": "AppConfig.AllAtOnce"},
			response: `{}`, method: "GET", rawPath: "/deploymentstrategies/AppConfig.AllAtOnce", scope: "/appconfig/",
		},
		"restJson1 label escaping every reserved byte": {
			typeName: "AWS::Bedrock::IntelligentPromptRouter", identifier: map[string]string{"PromptRouterArn": arn},
			response: `{}`, method: "GET",
			rawPath: "/prompt-routers/arn%3Aaws%3Abedrock%3Aus-east-1%3A123456789012%3Adefault-prompt-router%2Fanthropic.claude%3A1",
			scope:   "/bedrock/",
		},
		"restJson1 body": {
			typeName: "AWS::XRay::Group", identifier: map[string]string{"GroupARN": "arn:aws:xray:us-east-1:123456789012:group/Default"},
			response: `{"Group":{}}`, method: "POST", rawPath: "/GetGroup", contentType: "application/json",
			body: map[string]any{"GroupARN": "arn:aws:xray:us-east-1:123456789012:group/Default"}, scope: "/xray/",
		},
		"awsJson1_1": {
			typeName: "AWS::CodeDeploy::DeploymentConfig", identifier: map[string]string{"DeploymentConfigName": "CodeDeployDefault.OneAtATime"},
			response: `{"deploymentConfigInfo":{}}`, method: "POST", rawPath: "/",
			target: "CodeDeploy_20141006.GetDeploymentConfig", contentType: "application/x-amz-json-1.1",
			body: map[string]any{"deploymentConfigName": "CodeDeployDefault.OneAtATime"}, scope: "/codedeploy/",
		},
		"awsJson1_0": {
			typeName: "AWS::AppRunner::AutoScalingConfiguration", identifier: map[string]string{"AutoScalingConfigurationArn": "arn:x"},
			response: `{"AutoScalingConfiguration":{}}`, method: "POST", rawPath: "/",
			target: "AppRunner.DescribeAutoScalingConfiguration", contentType: "application/x-amz-json-1.0",
			body: map[string]any{"AutoScalingConfigurationArn": "arn:x"}, scope: "/apprunner/",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client, got := serve(t, 200, nil, c.response)
			if _, err := client.Read(context.Background(), c.typeName, c.identifier); err != nil {
				t.Fatal(err)
			}
			if got.method != c.method || got.rawPath != c.rawPath || got.target != c.target || got.contentType != c.contentType {
				t.Fatalf("request = %s %s target=%q type=%q", got.method, got.rawPath, got.target, got.contentType)
			}
			if !reflect.DeepEqual(got.body, c.body) {
				t.Fatalf("body = %v, want %v", got.body, c.body)
			}
			want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260924/us-east-1" + c.scope + "aws4_request"
			if !strings.HasPrefix(got.auth, want) {
				t.Fatalf("Authorization = %q, want it to begin %q", got.auth, want)
			}
		})
	}
}

// The response is walked to the resource and renamed to its properties,
// nested structures and lists of structures included; a member no property
// maps to is dropped.
func TestResponsesAreTranslated(t *testing.T) {
	client, _ := serve(t, 200, nil, `{"deploymentConfigInfo":{
		"computePlatform":"Server","deploymentConfigName":"CodeDeployDefault.HalfAtATime","createTime":1.7e9,
		"minimumHealthyHosts":{"type":"FLEET_PERCENT","value":50}}}`)
	got, err := client.Read(context.Background(), "AWS::CodeDeploy::DeploymentConfig",
		map[string]string{"DeploymentConfigName": "CodeDeployDefault.HalfAtATime"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"ComputePlatform": "Server", "DeploymentConfigName": "CodeDeployDefault.HalfAtATime",
		"MinimumHealthyHosts": map[string]any{"Type": "FLEET_PERCENT", "Value": json.Number("50")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant %#v", got, want)
	}

	client, _ = serve(t, 200, nil, `{"promptRouterName":"r","models":[{"modelArn":"a"},{"modelArn":"b"}],"fallbackModel":{"modelArn":"a"}}`)
	got, err = client.Read(context.Background(), "AWS::Bedrock::IntelligentPromptRouter", map[string]string{"PromptRouterArn": "x"})
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]any{
		"PromptRouterName": "r",
		"Models":           []any{map[string]any{"ModelArn": "a"}, map[string]any{"ModelArn": "b"}},
		"FallbackModel":    map[string]any{"ModelArn": "a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant %#v", got, want)
	}
}

func TestErrorsNameTheirType(t *testing.T) {
	cases := map[string]struct {
		header http.Header
		body   string
		code   string
	}{
		"from the header, sanitized": {http.Header{"X-Amzn-Errortype": {"ResourceNotFoundException:http://internal.amazon.com/"}},
			`{"message":"gone"}`, "ResourceNotFoundException"},
		"from __type, sanitized": {nil, `{"__type":"com.amazonaws.xray#InvalidRequestException","Message":"bad"}`, "InvalidRequestException"},
		"from code":              {nil, `{"code":"DeploymentConfigDoesNotExistException","message":"no"}`, "DeploymentConfigDoesNotExistException"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := serve(t, 400, c.header, c.body)
			_, err := client.Read(context.Background(), "AWS::XRay::Group", map[string]string{"GroupARN": "x"})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != c.code || apiErr.Status != 400 || apiErr.Message == "" {
				t.Fatalf("Read = %v, want code %s", err, c.code)
			}
		})
	}
}

func TestAReadNeedsItsIdentifier(t *testing.T) {
	client, _ := serve(t, 200, nil, `{}`)
	if _, err := client.Read(context.Background(), "AWS::XRay::Group", map[string]string{"GroupName": "x"}); err == nil ||
		!strings.Contains(err.Error(), "GroupARN") {
		t.Fatalf("Read = %v", err)
	}
	if _, err := client.Read(context.Background(), "AWS::Nope::Thing", nil); err == nil {
		t.Fatal("a type with no reader was read")
	}
}

func TestGreedyLabelsKeepTheirSlashes(t *testing.T) {
	if got := escapeLabel("a/b c", true); got != "a/b%20c" {
		t.Fatalf("greedy = %q", got)
	}
	if got := escapeLabel("a/b c", false); got != "a%2Fb%20c" {
		t.Fatalf("plain = %q", got)
	}
}

// A jsonName under an awsJson protocol is refused rather than guessed at.
func TestJSONNameUnderAWSJSONIsRefused(t *testing.T) {
	m := copyFS(t)
	model := m["models/codedeploy-2014-10-06.json"]
	var parsed map[string]any
	if err := json.Unmarshal(model.Data, &parsed); err != nil {
		t.Fatal(err)
	}
	info := parsed["shapes"].(map[string]any)["com.amazonaws.codedeploy#DeploymentConfigInfo"].(map[string]any)
	member := info["members"].(map[string]any)["computePlatform"].(map[string]any)
	member["traits"] = map[string]any{"smithy.api#jsonName": "platform"}
	model.Data, _ = json.Marshal(parsed)

	lock, err := loadLock(m)
	if err != nil {
		t.Fatal(err)
	}
	all, err := overrides(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		if o.Type != "AWS::CodeDeploy::DeploymentConfig" {
			continue
		}
		if _, errs := compileOne(m, lock, o); len(errs) == 0 || !strings.Contains(errors.Join(errs...).Error(), "which has a jsonName awsJson1_1 is not known to honour") {
			t.Fatalf("compile = %v", errs)
		}
		return
	}
	t.Fatal("no CodeDeploy override")
}

// The request is sent to the endpoint prefix and signed for the signing
// name, which differ for some services.
func TestSigningNameAndEndpointPrefixAreKeptApart(t *testing.T) {
	readers["Test::Split::Names"] = Reader{
		Type: "Test::Split::Names", Protocol: "awsJson1_1", SigningName: "signer", EndpointPrefix: "endpoint",
		Target: "Svc.Get", Identifier: []Binding{{Property: "Id", Member: "Id", Location: "body"}},
	}
	t.Cleanup(func() { delete(readers, "Test::Split::Names") })
	client, got := serve(t, 200, nil, `{}`)
	var prefix string
	inner := client.Endpoint
	client.Endpoint = func(p, region string) string { prefix = p; return inner(p, region) }
	if _, err := client.Read(context.Background(), "Test::Split::Names", map[string]string{"Id": "x"}); err != nil {
		t.Fatal(err)
	}
	if prefix != "endpoint" || !strings.Contains(got.auth, "/us-east-1/signer/aws4_request") {
		t.Fatalf("endpoint prefix %q, Authorization %q", prefix, got.auth)
	}
}
