package direct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const httpAPIType = "AWS::ApiGatewayV2::Api"

// restRequest is one request as it crossed the wire.
type restRequest struct {
	Method, Path string
	Query        map[string][]string
	Body         map[string]any
}

// fakeHTTPAPI is one HTTP API as API Gateway holds it, keyed by its wire
// names, answering by method and escaped path.
type fakeHTTPAPI struct {
	mu   sync.Mutex
	api  map[string]any
	seen []restRequest
}

func (f *fakeHTTPAPI) serve(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(raw, &in)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.seen = append(f.seen, restRequest{r.Method, r.URL.EscapedPath(), r.URL.Query(), in})
		answer := func(status int, v any) {
			w.WriteHeader(status)
			body, _ := json.Marshal(v)
			_, _ = w.Write(body)
		}
		const tagPath = "/v2/tags/arn%3Aaws%3Aapigateway%3Aus-east-1%3A%3A%2Fapis%2Fabc123"
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/apis":
			f.api = map[string]any{"apiId": "abc123", "apiEndpoint": "https://abc123.execute-api.us-east-1.amazonaws.com"}
			for k, v := range in {
				f.api[k] = v
			}
			answer(201, f.api)
		case f.api == nil:
			w.Header().Set("X-Amzn-Errortype", "NotFoundException")
			answer(404, map[string]any{"message": "gone"})
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/v2/apis/abc123":
			answer(200, f.api)
		case r.Method == http.MethodPatch && r.URL.EscapedPath() == "/v2/apis/abc123":
			for k, v := range in {
				f.api[k] = v
			}
			answer(200, f.api)
		case r.Method == http.MethodPost && r.URL.EscapedPath() == tagPath:
			tags, _ := f.api["tags"].(map[string]any)
			for k, v := range in["tags"].(map[string]any) {
				tags[k] = v
			}
			answer(201, map[string]any{})
		case r.Method == http.MethodDelete && r.URL.EscapedPath() == tagPath:
			tags, _ := f.api["tags"].(map[string]any)
			for _, k := range r.URL.Query()["tagKeys"] {
				delete(tags, k)
			}
			answer(204, nil)
		case r.Method == http.MethodDelete && r.URL.EscapedPath() == "/v2/apis/abc123":
			f.api = nil
			answer(204, nil)
		default:
			w.Header().Set("X-Amzn-Errortype", "BadRequestException")
			answer(400, map[string]any{"message": "unexpected " + r.Method + " " + r.URL.EscapedPath()})
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// sent is every request of method, in order.
func (f *fakeHTTPAPI) sent(method string) []restRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []restRequest
	for _, r := range f.seen {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// A create is a POST of the jsonName keys, nested structures included, and
// the identifier is read from the response's jsonName key.
func TestCreateHTTPAPISendsJSONNames(t *testing.T) {
	f := &fakeHTTPAPI{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), httpAPIType, map[string]any{
		"ProtocolType":      "HTTP",
		"CorsConfiguration": map[string]any{"AllowOrigins": []any{"https://example.com"}, "MaxAge": 60},
		"Tags":              map[string]any{"kraai:resource-name": "kraai-api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "abc123" {
		t.Fatalf("id = %q, want the apiId the response carries", id)
	}
	posts := f.sent(http.MethodPost)
	want := map[string]any{
		"name": "kraai-api", "protocolType": "HTTP",
		"corsConfiguration": map[string]any{"allowOrigins": []any{"https://example.com"}, "maxAge": 60.0},
		"tags":              map[string]any{"kraai:resource-name": "kraai-api"},
	}
	if len(posts) != 1 || posts[0].Path != "/v2/apis" || !reflect.DeepEqual(posts[0].Body, want) {
		t.Fatalf("POSTs = %+v\nwant one to /v2/apis with %v", posts, want)
	}
}

// An update is a PATCH of the changed members only, addressed by the label.
func TestUpdateHTTPAPIPatchesTheLabelledAPI(t *testing.T) {
	f := &fakeHTTPAPI{api: map[string]any{"apiId": "abc123", "name": "kraai-api", "protocolType": "HTTP", "description": "old"}}
	client := f.serve(t)
	if err := client.Update(context.Background(), httpAPIType, "abc123", map[string]any{}, map[string]any{"Description": "new"}); err != nil {
		t.Fatal(err)
	}
	patches := f.sent(http.MethodPatch)
	if len(patches) != 1 || patches[0].Path != "/v2/apis/abc123" || !reflect.DeepEqual(patches[0].Body, map[string]any{"description": "new"}) {
		t.Fatalf("PATCHes = %+v, want one description to /v2/apis/abc123", patches)
	}
}

// Tags are changed by the API's ARN, built from the region and percent
// encoded as a label, and every removed key is its own tagKeys parameter.
func TestUpdateHTTPAPITagsByItsARN(t *testing.T) {
	f := &fakeHTTPAPI{api: map[string]any{"apiId": "abc123", "name": "kraai-api", "protocolType": "HTTP",
		"tags": map[string]any{"kraai:resource-name": "kraai-api", "a": "1", "b": "2"}}}
	client := f.serve(t)
	current := map[string]any{"Tags": map[string]any{"kraai:resource-name": "kraai-api", "a": "1", "b": "2"}}
	desired := map[string]any{"Tags": map[string]any{"kraai:resource-name": "kraai-api", "c": "3"}}
	if err := client.Update(context.Background(), httpAPIType, "abc123", current, desired); err != nil {
		t.Fatal(err)
	}
	const tagPath = "/v2/tags/arn%3Aaws%3Aapigateway%3Aus-east-1%3A%3A%2Fapis%2Fabc123"
	adds, removes := f.sent(http.MethodPost), f.sent(http.MethodDelete)
	if len(adds) != 1 || adds[0].Path != tagPath || !reflect.DeepEqual(adds[0].Body, map[string]any{"tags": map[string]any{"c": "3"}}) {
		t.Fatalf("tag POSTs = %+v, want c=3 to %s", adds, tagPath)
	}
	if len(removes) != 1 || removes[0].Path != tagPath || !reflect.DeepEqual(removes[0].Query["tagKeys"], []string{"a", "b"}) {
		t.Fatalf("tag DELETEs = %+v, want tagKeys a and b to %s", removes, tagPath)
	}
}

func TestDeleteHTTPAPI(t *testing.T) {
	f := &fakeHTTPAPI{api: map[string]any{"apiId": "abc123"}}
	client := f.serve(t)
	if err := client.Delete(context.Background(), httpAPIType, "abc123"); err != nil {
		t.Fatal(err)
	}
	if deletes := f.sent(http.MethodDelete); len(deletes) != 1 || deletes[0].Path != "/v2/apis/abc123" {
		t.Fatalf("DELETEs = %+v, want /v2/apis/abc123", deletes)
	}
	// Gone already is gone.
	if err := client.Delete(context.Background(), httpAPIType, "abc123"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// A call whose label is empty or unfilled is never sent: the URI would
// address another resource, or none, and a 404 would read as absence.
func TestRESTMutationRefusesAnUnfilledLabel(t *testing.T) {
	f := &fakeHTTPAPI{api: map[string]any{"apiId": "abc123"}}
	client := f.serve(t)
	r := readers[httpAPIType]
	for name, values := range map[string]map[string]any{
		"unset": {},
		"empty": {"ApiId": ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.mutate(context.Background(), r, *r.Delete, values)
			if err == nil || !strings.Contains(err.Error(), "ApiId") {
				t.Fatalf("err = %v, want a refusal naming ApiId", err)
			}
		})
	}
	if len(f.seen) != 0 {
		t.Fatalf("requests %+v, want none", f.seen)
	}
}

func TestRESTCallRefusesWhatItCannotSend(t *testing.T) {
	model := &smithyModel{Shapes: map[string]smithyShape{
		"s#Blob": {Type: "blob"},
		"s#List": {Type: "list", Member: &smithyMember{Target: "smithy.api#String"}},
	}}
	op := smithyShape{Type: "operation", Traits: map[string]json.RawMessage{"smithy.api#http": json.RawMessage(`{"method":"PUT","uri":"/things/{Id}"}`)}}
	trait := func(name, value string) map[string]json.RawMessage {
		return map[string]json.RawMessage{name: json.RawMessage(value)}
	}
	cases := map[string]struct {
		member smithyMember
		want   string
	}{
		"a payload":           {smithyMember{Target: "s#Blob", Traits: trait("smithy.api#httpPayload", "{}")}, "bound by a trait this client does not send"},
		"prefix headers":      {smithyMember{Target: "smithy.api#String", Traits: trait("smithy.api#httpPrefixHeaders", `"x-"`)}, "bound by a trait"},
		"query params":        {smithyMember{Target: "smithy.api#String", Traits: trait("smithy.api#httpQueryParams", "{}")}, "bound by a trait"},
		"a body blob":         {smithyMember{Target: "s#Blob"}, "is a blob"},
		"a list label":        {smithyMember{Target: "s#List", Traits: trait("smithy.api#httpLabel", "{}")}, "a list sent as a label"},
		"a list header":       {smithyMember{Target: "s#List", Traits: trait("smithy.api#httpHeader", `"x-list"`)}, "a list sent as a header"},
		"an unbound URI part": {smithyMember{Target: "smithy.api#String"}, "puts Id in its URI, which the input does not set"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			input := smithyShape{Members: map[string]smithyMember{"Thing": c.member}}
			var errs []string
			restCall(model, op, input, Mutation{Operation: "PutThing", Input: map[string]any{"Thing": "{X}"}}, &MutationCall{}, "update[0]",
				func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) })
			if !slices.ContainsFunc(errs, func(e string) bool { return strings.Contains(e, c.want) }) {
				t.Fatalf("errors %q, want one containing %q", errs, c.want)
			}
		})
	}
}

// Structures are renamed inside lists and as map values; a map's keys are
// data and keep their names, and a member the shape lacks is left as is.
func TestRenameJSONAtEveryDepth(t *testing.T) {
	model := &smithyModel{Shapes: map[string]smithyShape{
		"s#Rule":  {Type: "structure", Members: map[string]smithyMember{"MaxAge": {Target: "smithy.api#Integer", Traits: map[string]json.RawMessage{"smithy.api#jsonName": json.RawMessage(`"maxAge"`)}}}},
		"s#Rules": {Type: "list", Member: &smithyMember{Target: "s#Rule"}},
		"s#ByKey": {Type: "map", Key: &smithyMember{Target: "smithy.api#String"}, Value: &smithyMember{Target: "s#Rule"}},
		"s#Plain": {Type: "structure", Members: map[string]smithyMember{"Name": {Target: "smithy.api#String"}}},
	}}
	if got := renameJSON([]any{map[string]any{"MaxAge": 1, "Extra": 2}}, bodyShape(model, "s#Rules", map[string]bool{})); !reflect.DeepEqual(got, []any{map[string]any{"maxAge": 1, "Extra": 2}}) {
		t.Errorf("list = %v", got)
	}
	if got := renameJSON(map[string]any{"MaxAge": map[string]any{"MaxAge": 1}}, bodyShape(model, "s#ByKey", map[string]bool{})); !reflect.DeepEqual(got, map[string]any{"MaxAge": map[string]any{"maxAge": 1}}) {
		t.Errorf("map = %v", got)
	}
	if shape := bodyShape(model, "s#Plain", map[string]bool{}); shape != nil {
		t.Errorf("a shape with nothing to rename = %+v, want nil", shape)
	}
}

// ExecuteApiArn, which no read returns, is built from the region, the
// account and the identifier, and left unread when the account is unknown.
func TestReadBuildsTheExecuteAPIArn(t *testing.T) {
	f := &fakeHTTPAPI{api: map[string]any{"apiId": "abc123", "name": "kraai-api", "protocolType": "HTTP"}}
	client := f.serve(t)
	ctx := context.Background()
	props, err := client.ReadByID(ctx, httpAPIType, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := props["ExecuteApiArn"]; ok {
		t.Fatalf("ExecuteApiArn = %v with no account source, want it unread", v)
	}
	calls := 0
	client.Account = func(context.Context) (string, error) { calls++; return "123456789012", nil }
	if props, err = client.ReadByID(ctx, httpAPIType, "abc123"); err != nil {
		t.Fatal(err)
	}
	if got, want := props["ExecuteApiArn"], "arn:aws:execute-api:us-east-1:123456789012:abc123"; got != want {
		t.Fatalf("ExecuteApiArn = %v, want %s", got, want)
	}
	client.Account = func(context.Context) (string, error) { return "", errors.New("no credentials") }
	if _, err := client.ReadByID(ctx, httpAPIType, "abc123"); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("err = %v, want the account failure", err)
	}
	// A reader building nothing from the account never asks for it.
	vars, err := client.templateVars(ctx, Reader{Fields: []Field{{Property: "Name", Member: "Name", Kind: "scalar"}}}, nil)
	if err != nil || calls != 1 || vars[accountPlaceholder] != "" {
		t.Fatalf("vars %v, err %v, account asked %d times; want no account and one earlier ask", vars, err, calls)
	}
}

func TestCompileRefusesATemplateFromAnUnknownValue(t *testing.T) {
	files := edit(t, "AWS--ApiGatewayV2--Api.yaml", "{region}:{account}:{ApiId}", "{region}:{account}:{Name}")
	_, err := compileAll(files)
	if err == nil || !strings.Contains(err.Error(), "ExecuteApiArn is built from {Name}, which is neither the primary identifier") {
		t.Fatalf("compile = %v", err)
	}
}
