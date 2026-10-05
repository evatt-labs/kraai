package direct

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const (
	permissionType = "AWS::Lambda::Permission"
	urlType        = "AWS::Lambda::Url"
	fnArn          = "arn:aws:lambda:us-east-1:123456789012:function:kraai-fn"
)

// fakeLambda holds one function's policy statements and URL config the way
// Lambda answers for them: an account principal is read back as its root
// ARN, and a URL's function as its full ARN.
type fakeLambda struct {
	mu         sync.Mutex
	statements map[string]map[string]any
	url        map[string]any
	added      []map[string]any
}

func (f *fakeLambda) serve(t *testing.T) *Client {
	t.Helper()
	f.statements = map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(raw, &in)
		f.mu.Lock()
		defer f.mu.Unlock()
		answer := func(status int, v any) {
			w.WriteHeader(status)
			body, _ := json.Marshal(v)
			_, _ = w.Write(body)
		}
		notFound := func() {
			w.Header().Set("X-Amzn-Errortype", "ResourceNotFoundException")
			answer(404, map[string]any{"message": "gone"})
		}
		path := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodPost && path == "/2015-03-31/functions/kraai-fn/policy":
			f.added = append(f.added, in)
			sid, _ := in["StatementId"].(string)
			principal, _ := in["Principal"].(string)
			if sid == "" || principal == "" {
				w.Header().Set("X-Amzn-Errortype", "ValidationException")
				answer(400, map[string]any{"message": "StatementId and Principal are required"})
				return
			}
			key := "Service"
			if regexp.MustCompile(`^\d{12}$`).MatchString(principal) {
				principal, key = "arn:aws:iam::"+principal+":root", "AWS"
			}
			f.statements[sid] = map[string]any{"Sid": sid, "Effect": "Allow",
				"Action": in["Action"], "Principal": map[string]any{key: principal}}
			answer(201, map[string]any{"Statement": "{}"})
		case r.Method == http.MethodGet && path == "/2015-03-31/functions/kraai-fn/policy":
			if len(f.statements) == 0 {
				notFound()
				return
			}
			var list []any
			for _, s := range f.statements {
				list = append(list, s)
			}
			doc, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": list})
			answer(200, map[string]any{"Policy": string(doc)})
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/2015-03-31/functions/kraai-fn/policy/"):
			delete(f.statements, strings.TrimPrefix(path, "/2015-03-31/functions/kraai-fn/policy/"))
			answer(204, nil)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/url"):
			f.url = map[string]any{"FunctionArn": fnArn, "FunctionUrl": "https://x.lambda-url.us-east-1.on.aws/", "AuthType": in["AuthType"], "InvokeMode": "BUFFERED"}
			answer(201, f.url)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/url"):
			if f.url == nil {
				notFound()
				return
			}
			answer(200, f.url)
		default:
			w.Header().Set("X-Amzn-Errortype", "InvalidParameterValueException")
			answer(400, map[string]any{"message": "unexpected " + r.Method + " " + path})
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 2 * time.Second, Poll: time.Millisecond}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// The statement ID is generated, sent, and the second part of the
// identifier; an account principal read back as its root ARN does not hold
// the create's wait.
func TestCreatePermissionGeneratesItsStatementID(t *testing.T) {
	f := &fakeLambda{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), permissionType, map[string]any{
		"FunctionName": "kraai-fn", "Action": "lambda:InvokeFunction", "Principal": "123456789012",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.added) != 1 {
		t.Fatalf("AddPermission calls %d, want 1", len(f.added))
	}
	sid, _ := f.added[0]["StatementId"].(string)
	if !uuidPattern.MatchString(sid) || id != "kraai-fn|"+sid {
		t.Fatalf("id %q, StatementId %q; want kraai-fn|<uuid sent>", id, sid)
	}
}

// An ID the desired state gives is sent as given.
func TestCreatePermissionKeepsAGivenStatementID(t *testing.T) {
	f := &fakeLambda{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), permissionType, map[string]any{
		"FunctionName": "kraai-fn", "Id": "mine", "Action": "lambda:InvokeFunction", "Principal": "events.amazonaws.com",
	})
	if err != nil || id != "kraai-fn|mine" {
		t.Fatalf("id %q, err %v; want kraai-fn|mine", id, err)
	}
}

// A URL whose function is given by ARN reads back by name, and the create's
// wait does not hold it to the ARN.
func TestCreateURLForAFunctionGivenByARN(t *testing.T) {
	f := &fakeLambda{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), urlType, map[string]any{"TargetFunctionArn": fnArn, "AuthType": "AWS_IAM"})
	if err != nil || id != fnArn {
		t.Fatalf("id %q, err %v; want %s", id, err, fnArn)
	}
}

func TestCompileRefusesABadGenerate(t *testing.T) {
	for name, c := range map[string]struct{ old, replacement, want string }{
		"a property that is not read-only": {"generate: [Id]", "generate: [FunctionName]", "create generates FunctionName, which must be a read-only identifier"},
		"an echo of a property not sent":   {"unechoed: [Principal]", "unechoed: [SourceArn, PrincipalOrgID, Qualifier]", "create does not hold Qualifier to its echo"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compileAll(edit(t, "AWS--Lambda--Permission.yaml", c.old, c.replacement))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("compile = %v\nwant %q", err, c.want)
			}
		})
	}
}
