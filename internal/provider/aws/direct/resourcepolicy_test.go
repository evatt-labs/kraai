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
	"testing/fstest"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const resourcePolicyType = "AWS::Logs::ResourcePolicy"

const (
	policyDocA = `{"Version":"2012-10-17","Statement":[{"Sid":"a"}]}`
	policyDocB = `{"Version":"2012-10-17","Statement":[{"Sid":"b"}]}`
)

// DescribeResourcePolicies answers every policy of the account and has no
// filter by name, so the read selects the one named from the list.
func TestReadResourcePolicySelectsByName(t *testing.T) {
	client, seen := targetServer(t, map[string]string{
		"DescribeResourcePolicies": `{"resourcePolicies":[` +
			`{"policyName":"other","policyDocument":` + jsonText(policyDocA) + `},` +
			`{"policyName":"kraai-p","policyDocument":` + jsonText(policyDocB) + `}]}`,
	})
	got, err := client.Read(context.Background(), resourcePolicyType, map[string]string{"PolicyName": "kraai-p"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"PolicyName": "kraai-p", "PolicyDocument": policyDocB}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v, want %#v", got, want)
	}
	if seen["DescribeResourcePolicies"] != `{"policyScope":"ACCOUNT"}` {
		t.Fatalf("request = %s, want it scoped to the account", seen["DescribeResourcePolicies"])
	}
}

// A name no policy holds is absence, even though the call answers a list
// of other policies; the read must not return one of them, or an empty
// instance.
func TestReadResourcePolicyAbsent(t *testing.T) {
	for name, body := range map[string]string{
		"other policies": `{"resourcePolicies":[{"policyName":"other","policyDocument":"{}"}]}`,
		"no policies":    `{"resourcePolicies":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := targetServer(t, map[string]string{"DescribeResourcePolicies": body})
			_, err := client.Read(context.Background(), resourcePolicyType, map[string]string{"PolicyName": "kraai-p"})
			if !errors.Is(err, ErrAbsent) {
				t.Fatalf("Read = %v, want ErrAbsent", err)
			}
		})
	}
}

// Two policies of one name would be a service defect; the read fails
// rather than choosing one.
func TestReadResourcePolicyRefusesTwoMatches(t *testing.T) {
	client, _ := targetServer(t, map[string]string{
		"DescribeResourcePolicies": `{"resourcePolicies":[{"policyName":"kraai-p","policyDocument":"{}"},{"policyName":"kraai-p","policyDocument":"{}"}]}`,
	})
	_, err := client.Read(context.Background(), resourcePolicyType, map[string]string{"PolicyName": "kraai-p"})
	if err == nil || errors.Is(err, ErrAbsent) || !strings.Contains(err.Error(), "want exactly the one read") {
		t.Fatalf("Read = %v, want the refusal of two matches", err)
	}
}

// PutResourcePolicy creates and replaces by name; the document goes as
// JSON text however the manifest gave it, and the name is the identifier.
func TestCreateUpdateAndDeleteResourcePolicy(t *testing.T) {
	var mu sync.Mutex
	policies := map[string]string{}
	var puts, deletes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in map[string]string
		_ = json.Unmarshal(raw, &in)
		op := r.Header.Get("X-Amz-Target")
		op = op[strings.LastIndex(op, ".")+1:]
		mu.Lock()
		defer mu.Unlock()
		switch op {
		case "PutResourcePolicy":
			puts = append(puts, string(raw))
			policies[in["policyName"]] = in["policyDocument"]
			_, _ = io.WriteString(w, `{}`)
		case "DescribeResourcePolicies":
			list := []any{}
			for name, doc := range policies {
				list = append(list, map[string]any{"policyName": name, "policyDocument": doc})
			}
			body, _ := json.Marshal(map[string]any{"resourcePolicies": list})
			_, _ = w.Write(body)
		case "DeleteResourcePolicy":
			deletes = append(deletes, string(raw))
			if _, ok := policies[in["policyName"]]; !ok {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"__type":"ResourceNotFoundException","message":"gone"}`)
				return
			}
			delete(policies, in["policyName"])
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"__type":"UnexpectedOperation"}`)
		}
	}))
	t.Cleanup(srv.Close)
	client := &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
	ctx := context.Background()

	id, err := client.Create(ctx, resourcePolicyType, map[string]any{"PolicyName": "kraai-p", "PolicyDocument": policyDocA})
	if err != nil {
		t.Fatal(err)
	}
	if id != "kraai-p" {
		t.Fatalf("id = %q, want the name sent", id)
	}
	if want := []string{`{"policyDocument":` + jsonText(policyDocA) + `,"policyName":"kraai-p"}`}; !reflect.DeepEqual(puts, want) {
		t.Fatalf("create sent %v, want %v", puts, want)
	}
	if err := client.Update(ctx, resourcePolicyType, "kraai-p", map[string]any{}, map[string]any{"PolicyDocument": policyDocB}); err != nil {
		t.Fatal(err)
	}
	if want := `{"policyDocument":` + jsonText(policyDocB) + `,"policyName":"kraai-p"}`; puts[len(puts)-1] != want {
		t.Fatalf("update sent %s, want %s", puts[len(puts)-1], want)
	}
	if err := client.Delete(ctx, resourcePolicyType, "kraai-p"); err != nil {
		t.Fatal(err)
	}
	if want := []string{`{"policyName":"kraai-p"}`}; !reflect.DeepEqual(deletes, want) {
		t.Fatalf("delete sent %v, want %v", deletes, want)
	}
	// Gone already, the delete is done.
	if err := client.Delete(ctx, resourcePolicyType, "kraai-p"); err != nil {
		t.Fatalf("Delete of a policy already gone = %v, want done", err)
	}
}

func TestCompileRefusesABadResponseSelection(t *testing.T) {
	const policy, subnet = "AWS--Logs--ResourcePolicy.yaml", "AWS--EC2--Subnet.yaml"
	for name, c := range map[string]struct {
		files fstest.MapFS
		want  string
	}{
		"a placeholder that is not the identifier": {edit(t, policy, "policyName={PolicyName}", "policyName={Other}"),
			"selects by {Other}, which is not the primary identifier"},
		"a member the element lacks": {edit(t, policy, "policyName={PolicyName}", "nope={PolicyName}"),
			"has no member nope"},
		"a selection under XML": {edit(t, subnet, "response: Subnets[]", "response: Subnets[SubnetId={SubnetId}]"),
			"read from JSON only"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compileAll(c.files)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("compile = %v\nwant an error containing %q", err, c.want)
			}
		})
	}
}

// jsonText is s as a JSON string literal.
func jsonText(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
