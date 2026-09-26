package direct

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// ddbServer answers each DynamoDB operation by its request body, recording
// every body by operation.
func ddbServer(t *testing.T, answer func(op, body string) (int, string)) (*Client, map[string][]string) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		op := r.Header.Get("X-Amz-Target")
		op = op[strings.LastIndex(op, ".")+1:]
		mu.Lock()
		seen[op] = append(seen[op], string(raw))
		mu.Unlock()
		status, body := answer(op, string(raw))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }}, seen
}

const (
	tableARN  = "arn:aws:dynamodb:us-east-1:1:table/t"
	streamARN = tableARN + "/stream/2026"
)

func ddbAnswer(op, body string) (int, string) {
	switch op {
	case "DescribeTable":
		return 200, `{"Table":{"TableName":"t","TableArn":"` + tableARN + `","LatestStreamArn":"` + streamARN + `",` +
			`"BillingModeSummary":{"BillingMode":"PAY_PER_REQUEST"},"ProvisionedThroughput":{"ReadCapacityUnits":0,"WriteCapacityUnits":0},` +
			`"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"}],"SSEDescription":{"Status":"ENABLED","SSEType":"KMS"},` +
			`"StreamSpecification":{"StreamViewType":"KEYS_ONLY"},` +
			`"GlobalSecondaryIndexes":[{"IndexName":"g1","ProvisionedThroughput":{"ReadCapacityUnits":0,"WriteCapacityUnits":0}},{"IndexName":"g2"}]}}`
	case "DescribeContinuousBackups":
		return 200, `{"ContinuousBackupsDescription":{"PointInTimeRecoveryDescription":{"PointInTimeRecoveryStatus":"DISABLED"}}}`
	case "DescribeTimeToLive":
		return 200, `{"TimeToLiveDescription":{"TimeToLiveStatus":"ENABLED","AttributeName":"exp"}}`
	case "DescribeContributorInsights":
		if strings.Contains(body, `"IndexName":"g1"`) {
			return 200, `{"ContributorInsightsStatus":"ENABLED","ContributorInsightsMode":"THROTTLED_KEYS"}`
		}
		return 200, `{"ContributorInsightsStatus":"DISABLED"}`
	case "DescribeKinesisStreamingDestination":
		return 200, `{"KinesisDataStreamDestinations":[{"StreamArn":"k-old","DestinationStatus":"DISABLED"},{"StreamArn":"k-new","DestinationStatus":"ACTIVE"}]}`
	case "ListTagsOfResource":
		if strings.Contains(body, streamARN) {
			return 200, `{"Tags":[{"Key":"of","Value":"stream"}]}`
		}
		return 200, `{"Tags":[{"Key":"of","Value":"table"}]}`
	case "GetResourcePolicy":
		if strings.Contains(body, streamARN) {
			return 200, `{"Policy":"{\"Version\":\"2012-10-17\"}"}`
		}
		return 400, `{"__type":"com.amazonaws.dynamodb.v20120810#PolicyNotFoundException","message":"none"}`
	}
	return 400, `{"__type":"UnexpectedOperation"}`
}

// A table reads through its own calls: statuses as booleans, fields the
// service returns flat as a structure, the active one of several streaming
// destinations, a call per index and one for the stream, tags and policies
// by the ARNs the table read captured, a missing policy as absent, and no
// throughput for an on-demand table.
func TestReadDynamoDBTable(t *testing.T) {
	client, seen := ddbServer(t, ddbAnswer)
	got, err := client.Read(context.Background(), "AWS::DynamoDB::Table", map[string]string{"TableName": "t"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"TableName": "t", "Arn": tableARN, "StreamArn": streamARN, "BillingMode": "PAY_PER_REQUEST",
		"KeySchema":        []any{map[string]any{"AttributeName": "pk", "KeyType": "HASH"}},
		"SSESpecification": map[string]any{"SSEEnabled": true, "SSEType": "KMS"},
		"StreamSpecification": map[string]any{"StreamViewType": "KEYS_ONLY",
			"Tags":           []any{map[string]any{"Key": "of", "Value": "stream"}},
			"ResourcePolicy": map[string]any{"PolicyDocument": map[string]any{"Version": "2012-10-17"}}},
		"GlobalSecondaryIndexes": []any{
			map[string]any{"IndexName": "g1", "ContributorInsightsSpecification": map[string]any{"Enabled": true, "Mode": "THROTTLED_KEYS"}},
			map[string]any{"IndexName": "g2", "ContributorInsightsSpecification": map[string]any{"Enabled": false}},
		},
		"PointInTimeRecoverySpecification": map[string]any{"PointInTimeRecoveryEnabled": false},
		"TimeToLiveSpecification":          map[string]any{"Enabled": true, "AttributeName": "exp"},
		"ContributorInsightsSpecification": map[string]any{"Enabled": false},
		"KinesisStreamSpecification":       map[string]any{"StreamArn": "k-new"},
		"Tags":                             []any{map[string]any{"Key": "of", "Value": "table"}},
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("Read =\n%s\nwant\n%s", g, w)
	}
	if n := len(seen["DescribeContributorInsights"]); n != 3 {
		t.Fatalf("DescribeContributorInsights called %d times, want one for the table and one per index", n)
	}
}

// A table without a stream has no stream ARN; the calls made for the
// stream are not made, and the read does not fail for want of it.
func TestReadDynamoDBTableWithoutAStream(t *testing.T) {
	client, seen := ddbServer(t, func(op, body string) (int, string) {
		if op == "DescribeTable" {
			return 200, `{"Table":{"TableName":"t","TableArn":"` + tableARN + `"}}`
		}
		return ddbAnswer(op, body)
	})
	got, err := client.Read(context.Background(), "AWS::DynamoDB::Table", map[string]string{"TableName": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["StreamSpecification"]; ok {
		t.Fatalf("StreamSpecification = %v, want absent", got["StreamSpecification"])
	}
	for _, body := range seen["ListTagsOfResource"] {
		if strings.Contains(body, "{StreamArn}") || strings.Contains(body, streamARN) {
			t.Fatalf("a stream call was made: %s", body)
		}
	}
}

func TestCompileRefusesABadDynamoDBShape(t *testing.T) {
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
		if o.Type == "AWS::DynamoDB::Table" {
			base = o
		}
	}
	cases := map[string]struct {
		edit func(*Override)
		want string
	}{
		"trueWhen on a non-boolean": {func(o *Override) {
			o.Properties["TableClass"] = Mapping{Member: "TableClassSummary.TableClass", TrueWhen: []string{"STANDARD"}}
		},
			"TableClass reads trueWhen, but the schema does not type it a boolean"},
		"each of a scalar": {func(o *Override) { o.Also[len(o.Also)-1].Each = "TableName" }, "is made for each TableName, which is not a structure or list of structures"},
		"unless of a structure": {func(o *Override) {
			m := o.Properties["ProvisionedThroughput"]
			m.Unless = map[string][]string{"SSESpecification": {"x"}}
			o.Properties["ProvisionedThroughput"] = m
		}, "ProvisionedThroughput is read unless SSESpecification, which is not a scalar property"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := base
			o.Properties = maps.Clone(base.Properties)
			o.Also = append([]Call(nil), base.Also...)
			c.edit(&o)
			if _, errs := compileOne(files, lock, o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}
