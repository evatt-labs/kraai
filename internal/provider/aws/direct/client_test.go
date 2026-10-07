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
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// seen is one request as the server received it.
type seen struct {
	method, rawPath, target, contentType, auth string
	body                                       map[string]any
}

// serve answers every request with status and body, recording the first
// request it saw: a type with further calls makes them after its read.
func serve(t *testing.T, status int, header http.Header, response string) (*Client, *seen) {
	t.Helper()
	got := &seen{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		if got.method == "" {
			got.method, got.rawPath = r.Method, r.URL.EscapedPath()
			got.target, got.contentType, got.auth = r.Header.Get("X-Amz-Target"), r.Header.Get("Content-Type"), r.Header.Get("Authorization")
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &got.body)
			}
		}
		mu.Unlock()
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
		Endpoint:    func(string) string { return srv.URL },
		Now:         func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	}, got
}

func TestRequestsAreShapedPerProtocol(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:team/fn:1"
	// No kept type reads by a restJson1 body, so one is registered here.
	readers["Test::Body::Read"] = Reader{
		Type: "Test::Body::Read", Protocol: "restJson1", SigningName: "xray", Host: "xray.{region}.amazonaws.com",
		Method: "POST", URI: "/GetGroup", Identifier: []Binding{{Property: "GroupARN", Member: "GroupARN", Location: "body"}},
	}
	t.Cleanup(func() { delete(readers, "Test::Body::Read") })
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
			typeName: "AWS::ApiGatewayV2::Api", identifier: map[string]string{"ApiId": "abcde12345"},
			response: `{}`, method: "GET", rawPath: "/v2/apis/abcde12345", scope: "/apigateway/",
		},
		"restJson1 label escaping every reserved byte": {
			typeName: "AWS::Lambda::Url", identifier: map[string]string{"FunctionArn": arn},
			response: `{}`, method: "GET",
			rawPath: "/2021-10-31/functions/arn%3Aaws%3Alambda%3Aus-east-1%3A123456789012%3Afunction%3Ateam%2Ffn%3A1/url",
			scope:   "/lambda/",
		},
		"restJson1 body": {
			typeName: "Test::Body::Read", identifier: map[string]string{"GroupARN": "arn:aws:xray:us-east-1:123456789012:group/Default"},
			response: `{"Group":{}}`, method: "POST", rawPath: "/GetGroup", contentType: "application/json",
			body: map[string]any{"GroupARN": "arn:aws:xray:us-east-1:123456789012:group/Default"}, scope: "/xray/",
		},
		"awsJson1_1": {
			typeName: "AWS::SSM::Parameter", identifier: map[string]string{"Name": "/kraai/one"},
			response: `{"Parameter":{}}`, method: "POST", rawPath: "/",
			target: "AmazonSSM.GetParameter", contentType: "application/x-amz-json-1.1",
			body: map[string]any{"Name": "/kraai/one"}, scope: "/ssm/",
		},
		"awsJson1_0": {
			typeName: "AWS::DynamoDB::Table", identifier: map[string]string{"TableName": "kraai-table"},
			response: `{"Table":{"TableArn":"arn:aws:dynamodb:us-east-1:123456789012:table/kraai-table"}}`, method: "POST", rawPath: "/",
			target: "DynamoDB_20120810.DescribeTable", contentType: "application/x-amz-json-1.0",
			body: map[string]any{"TableName": "kraai-table"}, scope: "/dynamodb/",
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
	client, _ := serve(t, 200, nil, `{"apiId":"abcde12345","name":"kraai","protocolType":"HTTP","createdDate":"2026-09-24T12:00:00Z",
		"corsConfiguration":{"allowOrigins":["https://a.example","https://b.example"],"maxAge":600,"allowCredentials":true}}`)
	got, err := client.Read(context.Background(), "AWS::ApiGatewayV2::Api", map[string]string{"ApiId": "abcde12345"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"ApiId": "abcde12345", "Name": "kraai", "ProtocolType": "HTTP",
		"CorsConfiguration": map[string]any{"AllowOrigins": []any{"https://a.example", "https://b.example"}, "MaxAge": json.Number("600"), "AllowCredentials": true},
	}
	// The ARN is built from the id, the region and the account.
	delete(got, "ExecuteApiArn")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant %#v", got, want)
	}

	client, _ = serve(t, 200, nil, `{"Table":{"TableName":"t","TableArn":"arn:aws:dynamodb:us-east-1:1:table/t","ItemCount":7,
		"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"N"}],
		"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}]}}`)
	got, err = client.Read(context.Background(), "AWS::DynamoDB::Table", map[string]string{"TableName": "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"AttributeDefinitions", "KeySchema"} {
		want := map[string]any{
			"AttributeDefinitions": []any{map[string]any{"AttributeName": "pk", "AttributeType": "S"}, map[string]any{"AttributeName": "sk", "AttributeType": "N"}},
			"KeySchema":            []any{map[string]any{"AttributeName": "pk", "KeyType": "HASH"}, map[string]any{"AttributeName": "sk", "KeyType": "RANGE"}},
		}[p]
		if !reflect.DeepEqual(got[p], want) {
			t.Fatalf("%s = %#v\nwant %#v", p, got[p], want)
		}
	}
	if _, ok := got["ItemCount"]; ok {
		t.Fatalf("a member no property maps to was kept: %#v", got)
	}
}

func TestErrorsNameTheirType(t *testing.T) {
	cases := map[string]struct {
		header http.Header
		body   string
		code   string
	}{
		"from the header, sanitized": {http.Header{"X-Amzn-Errortype": {"ConflictException:http://internal.amazon.com/"}},
			`{"message":"gone"}`, "ConflictException"},
		"from __type, sanitized": {nil, `{"__type":"com.amazonaws.apigatewayv2#BadRequestException","Message":"bad"}`, "BadRequestException"},
		"from code":              {nil, `{"code":"AccessDeniedException","message":"no"}`, "AccessDeniedException"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := serve(t, 400, c.header, c.body)
			_, err := client.Read(context.Background(), "AWS::ApiGatewayV2::Api", map[string]string{"ApiId": "x"})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != c.code || apiErr.Status != 400 || apiErr.Message == "" {
				t.Fatalf("Read = %v, want code %s", err, c.code)
			}
		})
	}
}

func TestAReadNeedsItsIdentifier(t *testing.T) {
	client, _ := serve(t, 200, nil, `{}`)
	if _, err := client.Read(context.Background(), "AWS::ApiGatewayV2::Api", map[string]string{"ApiName": "x"}); err == nil ||
		!strings.Contains(err.Error(), "ApiId") {
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
	model := m["models/ssm-2014-11-06.json"]
	var parsed map[string]any
	if err := json.Unmarshal(model.Data, &parsed); err != nil {
		t.Fatal(err)
	}
	info := parsed["shapes"].(map[string]any)["com.amazonaws.ssm#Parameter"].(map[string]any)
	member := info["members"].(map[string]any)["DataType"].(map[string]any)
	member["traits"] = map[string]any{"smithy.api#jsonName": "dataType"}
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
		if o.Type != "AWS::SSM::Parameter" {
			continue
		}
		if _, errs := compileOne(m, lock, o); len(errs) == 0 || !strings.Contains(errors.Join(errs...).Error(), "which has a jsonName awsJson1_1 is not known to honour") {
			t.Fatalf("compile = %v", errs)
		}
		return
	}
	t.Fatal("no SSM Parameter override")
}

// The request is sent to the reader's host in the client's region and
// signed for the signing name, which differ for some services; a global
// endpoint is signed for its own region, not the client's.
func TestHostAndSigningAreKeptApart(t *testing.T) {
	for name, c := range map[string]struct {
		host, signingRegion, wantHost, wantScope string
	}{
		"regional": {"endpoint.{region}.amazonaws.com", "", "endpoint.eu-west-1.amazonaws.com", "/eu-west-1/signer/aws4_request"},
		"global":   {"endpoint.amazonaws.com", "us-east-1", "endpoint.amazonaws.com", "/us-east-1/signer/aws4_request"},
	} {
		t.Run(name, func(t *testing.T) {
			readers["Test::Split::Names"] = Reader{
				Type: "Test::Split::Names", Protocol: "awsJson1_1", SigningName: "signer", Host: c.host, SigningRegion: c.signingRegion,
				Target: "Svc.Get", Identifier: []Binding{{Property: "Id", Member: "Id", Location: "body"}},
			}
			t.Cleanup(func() { delete(readers, "Test::Split::Names") })
			client, got := serve(t, 200, nil, `{}`)
			client.Region = "eu-west-1"
			var host string
			inner := client.Endpoint
			client.Endpoint = func(h string) string { host = h; return inner(h) }
			if _, err := client.Read(context.Background(), "Test::Split::Names", map[string]string{"Id": "x"}); err != nil {
				t.Fatal(err)
			}
			if host != c.wantHost || !strings.Contains(got.auth, c.wantScope) {
				t.Fatalf("host %q, Authorization %q", host, got.auth)
			}
		})
	}
}

// bodyPages is a client over a server answering each awsJson request with
// respond, given the page token the request carries in its body, and
// recording each request's target and body.
func bodyPages(t *testing.T, respond func(token string) (int, string)) (*Client, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Header.Get("X-Amz-Target")+" "+string(raw))
		var in struct {
			NextToken string `json:"nextToken"`
		}
		_ = json.Unmarshal(raw, &in)
		status, body := respond(in.NextToken)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{
		HTTP:        srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region:      "us-east-1",
		Endpoint:    func(string) string { return srv.URL },
		Now:         func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	}, &seen
}
