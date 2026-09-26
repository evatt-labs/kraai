package direct

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// targetServer answers each JSON request by its X-Amz-Target operation,
// recording every body by operation.
func targetServer(t *testing.T, byOperation map[string]string) (*Client, map[string]string) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		op := r.Header.Get("X-Amz-Target")
		op = op[strings.LastIndex(op, ".")+1:]
		mu.Lock()
		seen[op] = string(raw)
		mu.Unlock()
		body, ok := byOperation[op]
		if !ok {
			w.WriteHeader(400)
			body = `{"__type":"UnexpectedOperation"}`
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }}, seen
}

// A queue is one map of attribute strings, read by key and parsed to the
// schema's types; its URL is the identifier, and its tags a map read as
// Key/Value entries.
func TestReadSQSQueue(t *testing.T) {
	const url = "https://sqs.us-east-1.amazonaws.com/1/q"
	client, seen := targetServer(t, map[string]string{
		"GetQueueAttributes": `{"Attributes":{"QueueArn":"arn:aws:sqs:us-east-1:1:q","DelaySeconds":"5","FifoQueue":"false",` +
			`"SqsManagedSseEnabled":"true","RedrivePolicy":"{\"deadLetterTargetArn\":\"arn:aws:sqs:us-east-1:1:dlq\",\"maxReceiveCount\":3}"}}`,
		"ListQueueTags": `{"Tags":{"team":"cloud","env":"dev"}}`,
	})
	got, err := client.Read(context.Background(), "AWS::SQS::Queue", map[string]string{"QueueUrl": url})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"QueueUrl": url, "Arn": "arn:aws:sqs:us-east-1:1:q", "QueueName": "q",
		"DelaySeconds": json.Number("5"), "FifoQueue": false, "SqsManagedSseEnabled": true,
		"RedrivePolicy": map[string]any{"deadLetterTargetArn": "arn:aws:sqs:us-east-1:1:dlq", "maxReceiveCount": json.Number("3")},
		"Tags":          []any{map[string]any{"Key": "env", "Value": "dev"}, map[string]any{"Key": "team", "Value": "cloud"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	if seen["GetQueueAttributes"] != `{"AttributeNames":["All"],"QueueUrl":"`+url+`"}` {
		t.Fatalf("request = %s", seen["GetQueueAttributes"])
	}
}

// A log group's tags are a map, its policies JSON strings, and its resource
// policy the one of several listed for the ARN the read captured.
func TestReadLogGroupPolicies(t *testing.T) {
	const arn = "arn:aws:logs:us-east-1:1:log-group:/g"
	client, seen := targetServer(t, map[string]string{
		"DescribeLogGroups":       `{"logGroups":[{"logGroupName":"/g","logGroupClass":"STANDARD","arn":"` + arn + `:*","logGroupArn":"` + arn + `"}]}`,
		"ListTagsForResource":     `{"tags":{"team":"cloud"}}`,
		"GetDataProtectionPolicy": `{"policyDocument":"{\"Name\":\"p\"}"}`,
		"DescribeIndexPolicies":   `{"indexPolicies":[{"policyDocument":"{\"Fields\":[\"a\"]}"}]}`,
		"DescribeResourcePolicies": `{"resourcePolicies":[{"resourceArn":"arn:aws:logs:us-east-1:1:log-group:/other","policyDocument":"{}"},` +
			`{"resourceArn":"` + arn + `","policyDocument":"{\"Version\":\"2012-10-17\"}"}]}`,
	})
	got, err := client.Read(context.Background(), "AWS::Logs::LogGroup", map[string]string{"LogGroupName": "/g"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"LogGroupName": "/g", "LogGroupClass": "STANDARD", "Arn": arn + ":*",
		"Tags":                   []any{map[string]any{"Key": "team", "Value": "cloud"}},
		"DataProtectionPolicy":   map[string]any{"Name": "p"},
		"FieldIndexPolicies":     []any{map[string]any{"Fields": []any{"a"}}},
		"ResourcePolicyDocument": map[string]any{"Version": "2012-10-17"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	if !strings.Contains(seen["ListTagsForResource"], `"resourceArn":"`+arn+`"`) {
		t.Fatalf("tags request = %s, want the captured ARN", seen["ListTagsForResource"])
	}
}

// Index policies are asked for only on a standard log group; the service
// refuses the call on any other class.
func TestReadLogGroupSkipsIndexPoliciesOffStandard(t *testing.T) {
	const arn = "arn:aws:logs:us-east-1:1:log-group:/ia"
	client, seen := targetServer(t, map[string]string{
		"DescribeLogGroups":        `{"logGroups":[{"logGroupName":"/ia","logGroupClass":"INFREQUENT_ACCESS","arn":"` + arn + `:*","logGroupArn":"` + arn + `"}]}`,
		"ListTagsForResource":      `{}`,
		"GetDataProtectionPolicy":  `{}`,
		"DescribeResourcePolicies": `{"resourcePolicies":[]}`,
	})
	if _, err := client.Read(context.Background(), "AWS::Logs::LogGroup", map[string]string{"LogGroupName": "/ia"}); err != nil {
		t.Fatal(err)
	}
	if _, asked := seen["DescribeIndexPolicies"]; asked {
		t.Fatal("DescribeIndexPolicies was called for an infrequent-access log group")
	}
}

// A cluster parameter group's parameters are a list of name/value
// structures read as a map.
func TestReadDBClusterParameters(t *testing.T) {
	client, forms := xmlServerBy(t, map[string]string{
		"DescribeDBClusterParameterGroups": dbClusterParameterGroupXML,
		"ListTagsForResource":              tagsXML,
		"DescribeDBClusterParameters": `<DescribeDBClusterParametersResponse><DescribeDBClusterParametersResult><Parameters>
<Parameter><ParameterName>timezone</ParameterName><ParameterValue>UTC</ParameterValue></Parameter>
<Parameter><ParameterName>work_mem</ParameterName><ParameterValue>4096</ParameterValue></Parameter>
</Parameters></DescribeDBClusterParametersResult></DescribeDBClusterParametersResponse>`,
	})
	got, err := client.Read(context.Background(), dbClusterParameterGroups, map[string]string{"DBClusterParameterGroupName": "my-params"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"timezone": "UTC", "work_mem": "4096"}; !reflect.DeepEqual(got["Parameters"], want) {
		t.Fatalf("Parameters = %#v, want %#v", got["Parameters"], want)
	}
	for _, f := range *forms {
		if f.Get("Action") == "DescribeDBClusterParameters" && f.Get("Source") != "user" {
			t.Fatalf("parameters request = %v, want Source=user", f)
		}
	}
}

func TestCompileRefusesABadShape(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	var base Override
	for _, o := range all {
		if o.Type == "AWS::SQS::Queue" {
			base = o
		}
	}
	cases := map[string]struct {
		property string
		mapping  Mapping
		want     string
	}{
		"a parse the schema type does not take": {"Arn", Mapping{Member: "Attributes.QueueArn", Transform: "number"}, "Arn is [string] in the schema, which transform number does not produce"},
		"an unknown transform":                  {"Arn", Mapping{Member: "Attributes.QueueArn", Transform: "yaml"}, `names transform "yaml"; it is one of`},
		"entries of a scalar":                   {"Tags", Mapping{Member: "Attributes.QueueArn", Entries: []string{"Key", "Value"}}, "Tags reads entries of"},
		"entries named wrong":                   {"Tags", Mapping{Member: "Attributes", Entries: []string{"Name", "Value"}}, "the schema's items are not exactly those two properties"},
		"keyed from a scalar":                   {"RedrivePolicy", Mapping{Member: "Attributes.RedrivePolicy", Keyed: []string{"a", "b"}}, "RedrivePolicy is keyed from"},
		"another property's value":              {"Arn", Mapping{Member: "{Arn}"}, "Arn reads {Arn}, which is not the primary identifier"},
		"a key of a missing map":                {"Arn", Mapping{Member: "Nope.QueueArn"}, "Arn maps to Nope.QueueArn"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := base
			o.Properties = map[string]Mapping{}
			for k, v := range base.Properties {
				o.Properties[k] = v
			}
			if c.property == "Tags" {
				o.Also = nil
			}
			o.Properties[c.property] = c.mapping
			if _, errs := compileOne(files, lock, o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}
