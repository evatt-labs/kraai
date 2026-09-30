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
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const ddbTable = "AWS::DynamoDB::Table"

// tableFake is a DynamoDB whose table walks through a busy status after
// each change, as the real one does, and records any change made while it
// is busy.
type tableFake struct {
	mu         sync.Mutex
	exists     bool
	busy       int // reads left that answer a status other than ACTIVE
	gsiBusy    int // reads left that answer an index still being built
	status     string
	table      map[string]any
	tags       any
	policy     string
	pitr       string
	ttl        map[string]any
	calls      []string
	bodies     map[string][]map[string]any
	violations []string
	// failOnce answers an operation's first call with this error code.
	failOnce map[string]string
}

func newTableFake() *tableFake {
	return &tableFake{bodies: map[string][]map[string]any{}, failOnce: map[string]string{}, tags: []any{}}
}

// serve answers as the fake service; every mutating call made while the
// table is busy is a violation.
func (f *tableFake) serve(op, raw string) (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.Unmarshal([]byte(raw), &body)
	mutating := map[string]bool{"CreateTable": true, "UpdateTable": true, "TagResource": true, "UntagResource": true,
		"PutResourcePolicy": true, "UpdateContinuousBackups": true, "UpdateTimeToLive": true, "DeleteTable": true}
	if mutating[op] {
		f.calls = append(f.calls, op)
		f.bodies[op] = append(f.bodies[op], body)
		if code := f.failOnce[op]; code != "" {
			delete(f.failOnce, op)
			if code == "ResourceNotFoundException" {
				f.exists = false
			}
			return 400, `{"__type":"com.amazonaws.dynamodb.v20120810#` + code + `","message":"x"}`
		}
		if f.exists && op != "CreateTable" && (f.busy > 0 || f.gsiBusy > 0) {
			f.violations = append(f.violations, op)
		}
	}
	switch op {
	case "CreateTable":
		f.exists, f.busy, f.status = true, 3, "CREATING"
		f.table = map[string]any{"TableName": body["TableName"], "TableArn": tableARN}
		f.merge(body)
		if v, ok := body["Tags"]; ok {
			f.tags = v
		}
		if v, ok := body["ResourcePolicy"].(string); ok {
			f.policy = v
		}
		return 200, `{"TableDescription":{"TableName":"` + body["TableName"].(string) + `"}}`
	case "UpdateTable":
		f.busy, f.status = 2, "UPDATING"
		f.merge(body)
		return 200, `{"TableDescription":{"TableName":"t"}}`
	case "TagResource":
		f.tags = append(f.tags.([]any), body["Tags"].([]any)...)
		return 200, `{}`
	case "UntagResource":
		var kept []any
		for _, tag := range f.tags.([]any) {
			drop := false
			for _, k := range body["TagKeys"].([]any) {
				drop = drop || tag.(map[string]any)["Key"] == k
			}
			if !drop {
				kept = append(kept, tag)
			}
		}
		f.tags = kept
		return 200, `{}`
	case "PutResourcePolicy":
		f.policy, _ = body["Policy"].(string)
		return 200, `{}`
	case "UpdateContinuousBackups":
		f.pitr = "ENABLED"
		return 200, `{}`
	case "UpdateTimeToLive":
		f.ttl, _ = body["TimeToLiveSpecification"].(map[string]any)
		return 200, `{}`
	case "DeleteTable":
		f.exists = false
		return 200, `{}`
	case "DescribeTable":
		if !f.exists {
			return 400, `{"__type":"com.amazonaws.dynamodb.v20120810#ResourceNotFoundException","message":"gone"}`
		}
		out := map[string]any{"TableStatus": "ACTIVE"}
		for k, v := range f.table {
			out[k] = v
		}
		if f.busy > 0 {
			f.busy--
			out["TableStatus"] = f.status
		}
		if f.gsiBusy > 0 {
			f.gsiBusy--
			out["GlobalSecondaryIndexes"] = []any{map[string]any{"IndexName": "g", "IndexStatus": "CREATING"}}
		}
		raw, _ := json.Marshal(map[string]any{"Table": out})
		return 200, string(raw)
	case "DescribeContinuousBackups":
		status := "DISABLED"
		if f.pitr != "" {
			status = f.pitr
		}
		return 200, `{"ContinuousBackupsDescription":{"PointInTimeRecoveryDescription":{"PointInTimeRecoveryStatus":"` + status + `"}}}`
	case "DescribeTimeToLive":
		if f.ttl == nil {
			return 200, `{"TimeToLiveDescription":{"TimeToLiveStatus":"DISABLED"}}`
		}
		return 200, `{"TimeToLiveDescription":{"TimeToLiveStatus":"ENABLED","AttributeName":"` + f.ttl["AttributeName"].(string) + `"}}`
	case "DescribeContributorInsights":
		return 200, `{"ContributorInsightsStatus":"DISABLED"}`
	case "DescribeKinesisStreamingDestination":
		return 200, `{}`
	case "ListTagsOfResource":
		if strings.Contains(raw, streamARN) {
			return 200, `{"Tags":[]}`
		}
		raw, _ := json.Marshal(map[string]any{"Tags": f.tags})
		return 200, string(raw)
	case "GetResourcePolicy":
		if f.policy == "" || strings.Contains(raw, streamARN) {
			return 400, `{"__type":"com.amazonaws.dynamodb.v20120810#PolicyNotFoundException","message":"none"}`
		}
		raw, _ := json.Marshal(map[string]any{"Policy": f.policy})
		return 200, string(raw)
	}
	return 400, `{"__type":"UnexpectedOperation"}`
}

// merge sets what a create or update call carries as DescribeTable
// answers it.
func (f *tableFake) merge(body map[string]any) {
	for _, m := range []string{"KeySchema", "AttributeDefinitions", "DeletionProtectionEnabled", "WarmThroughput", "OnDemandThroughput", "ProvisionedThroughput"} {
		if v, ok := body[m]; ok {
			f.table[m] = v
		}
	}
	if v, ok := body["BillingMode"]; ok {
		f.table["BillingModeSummary"] = map[string]any{"BillingMode": v}
	}
	if v, ok := body["TableClass"]; ok {
		f.table["TableClassSummary"] = map[string]any{"TableClass": v}
	}
	if sse, ok := body["SSESpecification"].(map[string]any); ok {
		f.table["SSEDescription"] = map[string]any{"Status": "ENABLED", "SSEType": sse["SSEType"], "KMSMasterKeyArn": sse["KMSMasterKeyId"]}
	}
	if st, ok := body["StreamSpecification"].(map[string]any); ok {
		f.table["StreamSpecification"] = map[string]any{"StreamViewType": st["StreamViewType"]}
		f.table["LatestStreamArn"] = streamARN
	}
}

func (f *tableFake) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		op := r.Header.Get("X-Amz-Target")
		status, body := f.serve(op[strings.LastIndex(op, ".")+1:], string(raw))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// existing is a table that is up, with the state updates start from.
func (f *tableFake) existing() *tableFake {
	f.exists, f.status = true, "ACTIVE"
	f.table = map[string]any{"TableName": "t", "TableArn": tableARN, "KeySchema": []any{map[string]any{"AttributeName": "pk", "KeyType": "HASH"}},
		"BillingModeSummary": map[string]any{"BillingMode": "PROVISIONED"}, "ProvisionedThroughput": map[string]any{"ReadCapacityUnits": 5, "WriteCapacityUnits": 5}}
	return f
}

// update reads the table as the engine does and sets changes.
func (f *tableFake) update(t *testing.T, changes map[string]any) {
	t.Helper()
	client := f.client(t)
	current, err := client.ReadByID(context.Background(), ddbTable, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Update(context.Background(), ddbTable, "t", current, changes); err != nil {
		t.Fatal(err)
	}
}

func (f *tableFake) only(t *testing.T, op string) map[string]any {
	t.Helper()
	if len(f.bodies[op]) != 1 {
		t.Fatalf("%s calls = %v, want exactly one; all calls %v", op, f.bodies[op], f.calls)
	}
	return f.bodies[op][0]
}

func wantBody(t *testing.T, got, want map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Fatalf("request body\n%s\nwant\n%s", g, w)
	}
}

// Each property update is its own call with the body the API takes, and the
// capacity group is not sent together: a switch to on-demand must not
// resend the provisioned throughput read back.
func TestUpdateDynamoDBTableBodies(t *testing.T) {
	policy := map[string]any{"Version": "2012-10-17"}
	policyText, _ := json.Marshal(policy)
	for name, c := range map[string]struct {
		changes map[string]any
		op      string
		want    map[string]any
	}{
		"capacity alone": {map[string]any{"BillingMode": "PAY_PER_REQUEST"}, "UpdateTable",
			map[string]any{"TableName": "t", "BillingMode": "PAY_PER_REQUEST"}},
		"sse is renamed": {map[string]any{"SSESpecification": map[string]any{"SSEEnabled": true, "SSEType": "KMS", "KMSMasterKeyId": "k"}}, "UpdateTable",
			map[string]any{"TableName": "t", "SSESpecification": map[string]any{"Enabled": true, "SSEType": "KMS", "KMSMasterKeyId": "k"}}},
		"stream is enabled": {map[string]any{"StreamSpecification": map[string]any{"StreamViewType": "NEW_IMAGE"}}, "UpdateTable",
			map[string]any{"TableName": "t", "StreamSpecification": map[string]any{"StreamEnabled": true, "StreamViewType": "NEW_IMAGE"}}},
		"table class":         {map[string]any{"TableClass": "STANDARD_INFREQUENT_ACCESS"}, "UpdateTable", map[string]any{"TableName": "t", "TableClass": "STANDARD_INFREQUENT_ACCESS"}},
		"deletion protection": {map[string]any{"DeletionProtectionEnabled": false}, "UpdateTable", map[string]any{"TableName": "t", "DeletionProtectionEnabled": false}},
		"warm throughput": {map[string]any{"WarmThroughput": map[string]any{"ReadUnitsPerSecond": float64(13000)}}, "UpdateTable",
			map[string]any{"TableName": "t", "WarmThroughput": map[string]any{"ReadUnitsPerSecond": float64(13000)}}},
		"policy is json text": {map[string]any{"ResourcePolicy": map[string]any{"PolicyDocument": policy}}, "PutResourcePolicy",
			map[string]any{"ResourceArn": tableARN, "Policy": string(policyText)}},
		"point in time recovery": {map[string]any{"PointInTimeRecoverySpecification": map[string]any{"PointInTimeRecoveryEnabled": true}}, "UpdateContinuousBackups",
			map[string]any{"TableName": "t", "PointInTimeRecoverySpecification": map[string]any{"PointInTimeRecoveryEnabled": true}}},
		"time to live": {map[string]any{"TimeToLiveSpecification": map[string]any{"Enabled": true, "AttributeName": "exp"}}, "UpdateTimeToLive",
			map[string]any{"TableName": "t", "TimeToLiveSpecification": map[string]any{"Enabled": true, "AttributeName": "exp"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTableFake().existing()
			// An unrelated property read back must not ride along.
			f.tags = []any{map[string]any{"Key": "of", "Value": "table"}}
			f.update(t, c.changes)
			wantBody(t, f.only(t, c.op), c.want)
			if len(f.calls) != 1 {
				t.Fatalf("calls = %v, want only %s", f.calls, c.op)
			}
		})
	}
}

// Tags are added and removed by the table's ARN, which the read captures,
// never by its name.
func TestUpdateDynamoDBTableTags(t *testing.T) {
	f := newTableFake().existing()
	f.tags = []any{map[string]any{"Key": "keep", "Value": "1"}, map[string]any{"Key": "drop", "Value": "2"}}
	f.update(t, map[string]any{"Tags": []any{map[string]any{"Key": "keep", "Value": "1"}, map[string]any{"Key": "new", "Value": "3"}}})
	wantBody(t, f.only(t, "TagResource"), map[string]any{"ResourceArn": tableARN, "Tags": []any{map[string]any{"Key": "new", "Value": "3"}}})
	wantBody(t, f.only(t, "UntagResource"), map[string]any{"ResourceArn": tableARN, "TagKeys": []any{"drop"}})
}

// A change to a property no call sets is refused before any call is made;
// the key schema and local indexes have none because UpdateTable has no
// member for either, and the table is not lifecycle complete while its
// global indexes have no route either.
func TestDynamoDBTableIsNotLifecycleComplete(t *testing.T) {
	r := readers[ddbTable]
	if r.LifecycleComplete || CanMutate(ddbTable) {
		t.Fatalf("LifecycleComplete = %v, CanMutate = %v; index routes are not declared", r.LifecycleComplete, CanMutate(ddbTable))
	}
	if got := CreateOnly(ddbTable); !reflect.DeepEqual(got, []string{"/properties/KeySchema", "/properties/LocalSecondaryIndexes"}) {
		t.Fatalf("CreateOnly = %v", got)
	}
	if r.Wait != 20*time.Minute {
		t.Fatalf("Wait = %s, want 20m", r.Wait)
	}
	f := newTableFake().existing()
	client := f.client(t)
	for _, p := range []string{"KeySchema", "LocalSecondaryIndexes", "GlobalSecondaryIndexes"} {
		err := client.Update(context.Background(), ddbTable, "t", map[string]any{}, map[string]any{p: []any{}})
		if err == nil || !strings.Contains(err.Error(), "has no direct update for "+p) {
			t.Fatalf("update of %s error = %v", p, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %v, want none", f.calls)
	}
}

// A table is refused every change while it or one of its indexes settles
// the last, so a second call waits for ACTIVE on the table and on each
// index.
func TestUpdateDynamoDBTableWaitsWhileBusy(t *testing.T) {
	for name, gsi := range map[string]bool{"table status": false, "index status": true} {
		t.Run(name, func(t *testing.T) {
			f := newTableFake().existing()
			client := f.client(t)
			current, err := client.ReadByID(context.Background(), ddbTable, "t")
			if err != nil {
				t.Fatal(err)
			}
			if gsi {
				f.mu.Lock()
				f.gsiBusy = 4
				f.mu.Unlock()
			}
			changes := map[string]any{"TableClass": "STANDARD_INFREQUENT_ACCESS", "DeletionProtectionEnabled": true}
			if err := client.Update(context.Background(), ddbTable, "t", current, changes); err != nil {
				t.Fatal(err)
			}
			if len(f.bodies["UpdateTable"]) != 2 {
				t.Fatalf("UpdateTable calls = %v, want two", f.bodies["UpdateTable"])
			}
			if len(f.violations) != 0 {
				t.Fatalf("calls made while the table was busy: %v", f.violations)
			}
		})
	}
}

// A created table walks CREATING to ACTIVE across reads; nothing is sent
// to it until it is ACTIVE, and what CreateTable takes goes in the create,
// not after it.
func TestCreateDynamoDBTable(t *testing.T) {
	f := newTableFake()
	f.failOnce["UpdateContinuousBackups"] = "ContinuousBackupsUnavailableException"
	client := f.client(t)
	policy := map[string]any{"Version": "2012-10-17"}
	policyText, _ := json.Marshal(policy)
	desired := map[string]any{
		"AttributeDefinitions":             []any{map[string]any{"AttributeName": "pk", "AttributeType": "S"}},
		"KeySchema":                        []any{map[string]any{"AttributeName": "pk", "KeyType": "HASH"}},
		"BillingMode":                      "PAY_PER_REQUEST",
		"DeletionProtectionEnabled":        true,
		"SSESpecification":                 map[string]any{"SSEEnabled": true, "SSEType": "KMS", "KMSMasterKeyId": "k"},
		"StreamSpecification":              map[string]any{"StreamViewType": "KEYS_ONLY"},
		"ResourcePolicy":                   map[string]any{"PolicyDocument": policy},
		"Tags":                             []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-t"}},
		"PointInTimeRecoverySpecification": map[string]any{"PointInTimeRecoveryEnabled": true},
		"TimeToLiveSpecification":          map[string]any{"Enabled": true, "AttributeName": "exp"},
	}
	id, err := client.Create(context.Background(), ddbTable, desired)
	if err != nil {
		t.Fatal(err)
	}
	if id != "kraai-t" {
		t.Fatalf("id = %q, want the name from the tag", id)
	}
	wantBody(t, f.only(t, "CreateTable"), map[string]any{
		"TableName":                 "kraai-t",
		"AttributeDefinitions":      desired["AttributeDefinitions"],
		"KeySchema":                 desired["KeySchema"],
		"BillingMode":               "PAY_PER_REQUEST",
		"DeletionProtectionEnabled": true,
		"SSESpecification":          map[string]any{"Enabled": true, "SSEType": "KMS", "KMSMasterKeyId": "k"},
		"StreamSpecification":       map[string]any{"StreamEnabled": true, "StreamViewType": "KEYS_ONLY"},
		"ResourcePolicy":            string(policyText),
		"Tags":                      desired["Tags"],
	})
	// Backups and time to live are not CreateTable members: they follow it,
	// the first retried until backups are available.
	if len(f.bodies["UpdateContinuousBackups"]) != 2 || len(f.bodies["UpdateTimeToLive"]) != 1 {
		t.Fatalf("calls = %v", f.calls)
	}
	if len(f.violations) != 0 {
		t.Fatalf("calls made while the table was busy: %v", f.violations)
	}
	if f.calls[0] != "CreateTable" {
		t.Fatalf("calls = %v, want CreateTable first", f.calls)
	}
}

// A stream is enabled only for a manifest that names one: the constant in
// the call's template must not make a stream of a table that has none.
func TestCreateDynamoDBTableWithoutAStream(t *testing.T) {
	f := newTableFake()
	client := f.client(t)
	_, err := client.Create(context.Background(), ddbTable, map[string]any{
		"AttributeDefinitions": []any{map[string]any{"AttributeName": "pk", "AttributeType": "S"}},
		"KeySchema":            []any{map[string]any{"AttributeName": "pk", "KeyType": "HASH"}},
		"Tags":                 []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-t"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := f.only(t, "CreateTable")
	for _, m := range []string{"StreamSpecification", "SSESpecification"} {
		if v, ok := body[m]; ok {
			t.Fatalf("CreateTable sent %s = %v for a manifest that sets none", m, v)
		}
	}
}

// A table still being updated refuses its delete; the delete is retried
// until it is accepted, and one for a table already gone is done.
func TestDeleteDynamoDBTable(t *testing.T) {
	f := newTableFake().existing()
	f.failOnce["DeleteTable"] = "ResourceInUseException"
	if err := f.client(t).Delete(context.Background(), ddbTable, "t"); err != nil {
		t.Fatal(err)
	}
	if len(f.bodies["DeleteTable"]) != 2 {
		t.Fatalf("DeleteTable calls = %d, want the refused one and its retry", len(f.bodies["DeleteTable"]))
	}
	gone := newTableFake()
	gone.failOnce["DeleteTable"] = "ResourceNotFoundException"
	gone.exists = true
	if err := gone.client(t).Delete(context.Background(), ddbTable, "t"); err != nil {
		t.Fatalf("delete of a table that is gone: %v", err)
	}
}

// A structure holding a constant beside placeholders is sent only when one
// of them is set; one of constants alone always is.
func TestRenderStructureOfConstantsAndPlaceholders(t *testing.T) {
	template := map[string]any{"On": true, "Kind": "{Kind}"}
	if got, ok, err := render(template, map[string]any{}, nil); err != nil || ok {
		t.Fatalf("render with Kind unset = %v, %v, %v; want left out", got, ok, err)
	}
	got, ok, err := render(template, map[string]any{"Kind": "x"}, nil)
	if err != nil || !ok || !reflect.DeepEqual(got, map[string]any{"On": true, "Kind": "x"}) {
		t.Fatalf("render with Kind set = %v, %v, %v", got, ok, err)
	}
	if got, ok, _ := render(map[string]any{"On": true}, map[string]any{}, nil); !ok || !reflect.DeepEqual(got, map[string]any{"On": true}) {
		t.Fatalf("render of constants alone = %v, %v", got, ok)
	}
}
