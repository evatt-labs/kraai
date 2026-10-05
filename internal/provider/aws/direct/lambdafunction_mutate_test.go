package direct

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const functionType = "AWS::Lambda::Function"

// fakeFunction holds one function the way Lambda answers for it: a create
// or update leaves it settling until the next read, and a change sent
// while it settles is refused with ResourceConflictException.
type fakeFunction struct {
	mu sync.Mutex
	// unassumable is how many creates are refused because the role is not
	// yet assumable.
	unassumable int
	config      map[string]any
	tags        map[string]any
	reserved    any
	calls       map[string][]map[string]any
	conflicts   int
}

func (f *fakeFunction) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]map[string]any{}
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
		refuse := func(status int, code, message string) {
			w.Header().Set("X-Amzn-Errortype", code)
			answer(status, map[string]any{"message": message})
		}
		settling := func() bool {
			return f.config != nil && (f.config["State"] == "Pending" || f.config["LastUpdateStatus"] == "InProgress")
		}
		change := func(op string) bool {
			f.calls[op] = append(f.calls[op], in)
			if f.config == nil {
				refuse(404, "ResourceNotFoundException", "Function not found")
				return false
			}
			if settling() {
				f.conflicts++
				refuse(409, "ResourceConflictException", "An update is in progress")
				return false
			}
			return true
		}
		path := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodPost && path == "/2015-03-31/functions":
			f.calls["CreateFunction"] = append(f.calls["CreateFunction"], in)
			if in["Runtime"] == "python2.7" {
				refuse(400, "InvalidParameterValueException", "The runtime parameter of python2.7 is no longer supported")
				return
			}
			if f.unassumable > 0 {
				f.unassumable--
				refuse(400, "InvalidParameterValueException", "The role defined for the function cannot be assumed by Lambda.")
				return
			}
			f.config = map[string]any{"FunctionName": in["FunctionName"], "FunctionArn": fnArn, "Runtime": in["Runtime"],
				"Handler": in["Handler"], "Role": in["Role"], "Timeout": in["Timeout"], "State": "Pending", "LastUpdateStatus": "Successful"}
			f.tags, _ = in["Tags"].(map[string]any)
			answer(201, f.config)
		case r.Method == http.MethodGet && path == "/2015-03-31/functions/kraai-fn":
			if f.config == nil {
				refuse(404, "ResourceNotFoundException", "Function not found")
				return
			}
			out := map[string]any{"Configuration": f.config, "Tags": f.tags}
			if f.reserved != nil {
				out["Concurrency"] = map[string]any{"ReservedConcurrentExecutions": f.reserved}
			}
			answer(200, out)
			// Whatever was settling is settled by the next read.
			f.config["State"], f.config["LastUpdateStatus"] = "Active", "Successful"
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/code-signing-config"):
			answer(200, map[string]any{"FunctionName": "kraai-fn"})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/recursion-config"):
			answer(200, map[string]any{"RecursiveLoop": "Terminate"})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/runtime-management-config"):
			answer(200, map[string]any{"UpdateRuntimeOn": "Auto"})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/function-scaling-config"):
			refuse(404, "ResourceNotFoundException", "no scaling config")
		case r.Method == http.MethodPut && path == "/2015-03-31/functions/kraai-fn/configuration":
			if change("UpdateFunctionConfiguration") {
				maps.Copy(f.config, in)
				f.config["LastUpdateStatus"] = "InProgress"
				answer(200, f.config)
			}
		case r.Method == http.MethodPut && path == "/2015-03-31/functions/kraai-fn/code":
			if change("UpdateFunctionCode") {
				f.config["LastUpdateStatus"] = "InProgress"
				answer(200, f.config)
			}
		case r.Method == http.MethodPut && path == "/2017-10-31/functions/kraai-fn/concurrency":
			if change("PutFunctionConcurrency") {
				f.reserved = in["ReservedConcurrentExecutions"]
				answer(200, in)
			}
		case r.Method == http.MethodDelete && path == "/2015-03-31/functions/kraai-fn":
			if change("DeleteFunction") {
				f.config = nil
				answer(204, nil)
			}
		default:
			refuse(400, "InvalidParameterValueException", "unexpected "+r.Method+" "+path)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 2 * time.Second, Poll: time.Millisecond}
}

func newFunction() map[string]any {
	return map[string]any{
		"FunctionName": "kraai-fn", "Runtime": "python3.13", "Handler": "index.handler",
		"Role": "arn:aws:iam::123456789012:role/kraai-fn", "Timeout": 3,
		"Code": map[string]any{"S3Bucket": "artifacts", "S3Key": "handler-1.zip"},
		"Tags": []any{map[string]any{"Key": "stage", "Value": "one"}},
	}
}

// A role not yet assumable is retried; the code is sent as the structure
// it is, the tags as the map Lambda takes, and a property only an update
// call sets is set once the new function settles.
func TestCreateFunction(t *testing.T) {
	f := &fakeFunction{unassumable: 2}
	client := f.serve(t)
	desired := newFunction()
	desired["ReservedConcurrentExecutions"] = 1
	id, err := client.Create(context.Background(), functionType, desired)
	if err != nil {
		t.Fatal(err)
	}
	if id != "kraai-fn" {
		t.Fatalf("identifier %q, want kraai-fn", id)
	}
	creates := f.calls["CreateFunction"]
	if len(creates) != 3 {
		t.Fatalf("CreateFunction called %d times, want 3", len(creates))
	}
	sent := creates[2]
	if code, _ := sent["Code"].(map[string]any); code["S3Bucket"] != "artifacts" || code["S3Key"] != "handler-1.zip" {
		t.Fatalf("Code sent %v, want the bucket and key", sent["Code"])
	}
	if tags, _ := sent["Tags"].(map[string]any); tags["stage"] != "one" || len(tags) != 1 {
		t.Fatalf("Tags sent %v, want {stage: one}", sent["Tags"])
	}
	if puts := f.calls["PutFunctionConcurrency"]; len(puts) != 1 || puts[0]["ReservedConcurrentExecutions"] != float64(1) {
		t.Fatalf("PutFunctionConcurrency calls %v, want one reserving 1", puts)
	}
	if f.conflicts != 0 {
		t.Fatalf("%d calls were sent while the function settled", f.conflicts)
	}
}

// An invalid parameter that is not the role is refused at once, not
// retried for the length of the wait.
func TestCreateFunctionRefusesAnInvalidParameter(t *testing.T) {
	f := &fakeFunction{}
	client := f.serve(t)
	desired := newFunction()
	desired["Runtime"] = "python2.7"
	_, err := client.Create(context.Background(), functionType, desired)
	if err == nil || !strings.Contains(err.Error(), "python2.7 is no longer supported") {
		t.Fatalf("err = %v, want the runtime refused", err)
	}
	if n := len(f.calls["CreateFunction"]); n != 1 {
		t.Fatalf("CreateFunction called %d times, want 1", n)
	}
}

// Inline source is Cloud Control's to zip: a direct create refuses it
// before any call.
func TestCreateFunctionRefusesInlineSource(t *testing.T) {
	f := &fakeFunction{}
	client := f.serve(t)
	desired := newFunction()
	desired["Code"] = map[string]any{"ZipFile": "def handler(e, c): pass"}
	_, err := client.Create(context.Background(), functionType, desired)
	if err == nil || !strings.Contains(err.Error(), "Code.ZipFile") {
		t.Fatalf("err = %v, want Code.ZipFile refused", err)
	}
	if n := len(f.calls["CreateFunction"]); n != 0 {
		t.Fatalf("CreateFunction called %d times, want 0", n)
	}
}

// Configuration and code are separate calls, the second sent once the
// first has settled, with the code's location as UpdateFunctionCode's own
// members.
func TestUpdateFunctionConfigurationAndCode(t *testing.T) {
	f := &fakeFunction{}
	client := f.serve(t)
	ctx := context.Background()
	if _, err := client.Create(ctx, functionType, newFunction()); err != nil {
		t.Fatal(err)
	}
	current, err := client.ReadByID(ctx, functionType, "kraai-fn")
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]any{
		"Timeout": 5,
		"Code":    map[string]any{"S3Bucket": "artifacts", "S3Key": "handler-2.zip"},
	}
	if err := client.Update(ctx, functionType, "kraai-fn", current, changes); err != nil {
		t.Fatal(err)
	}
	configs, codes := f.calls["UpdateFunctionConfiguration"], f.calls["UpdateFunctionCode"]
	if len(configs) != 1 || configs[0]["Timeout"] != float64(5) {
		t.Fatalf("UpdateFunctionConfiguration calls %v, want one setting Timeout 5", configs)
	}
	if len(codes) != 1 || codes[0]["S3Bucket"] != "artifacts" || codes[0]["S3Key"] != "handler-2.zip" {
		t.Fatalf("UpdateFunctionCode calls %v, want one sending handler-2.zip", codes)
	}
	if _, nested := codes[0]["Code"]; nested {
		t.Fatalf("UpdateFunctionCode sent Code nested: %v", codes[0])
	}
	if f.conflicts != 0 {
		t.Fatalf("%d calls were sent while the function settled", f.conflicts)
	}
}
