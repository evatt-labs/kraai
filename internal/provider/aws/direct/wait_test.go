package direct

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const ddbTableType = "AWS::DynamoDB::Table"

// compileEdited compiles typeName's real override after edit.
func compileEdited(t *testing.T, typeName string, edit func(*Override)) (Reader, []error) {
	t.Helper()
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		if o.Type != typeName {
			continue
		}
		o.Read.Busy = maps.Clone(o.Read.Busy)
		if o.Read.Busy == nil {
			o.Read.Busy = map[string][]string{}
		}
		o.Read.Absent = maps.Clone(o.Read.Absent)
		edit(&o)
		return compileOne(files, lock, o)
	}
	t.Fatalf("no override for %s", typeName)
	return Reader{}, nil
}

// busyTable is a DynamoDB table that reports UPDATING for a number of reads
// after each change, and an index that reports CREATING for some, recording
// each UpdateTable with whether it was made while the table was busy and
// each table read with the status it answered.
type busyTable struct {
	mu    sync.Mutex
	table *tableFake
	// events is the reads and calls in order.
	events []string
}

// busyTableFake is a table that is up, busy for tableReads further reads
// and with an index still being built for indexReads of them.
func busyTableFake(tableReads, indexReads int) *busyTable {
	f := newTableFake().existing()
	f.busy, f.gsiBusy, f.status = tableReads, indexReads, "UPDATING"
	return &busyTable{table: f}
}

func (f *busyTable) serve(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		op := r.Header.Get("X-Amz-Target")
		op = op[strings.LastIndex(op, ".")+1:]
		f.mu.Lock()
		defer f.mu.Unlock()
		violations := len(f.table.violations)
		status, body := f.table.serve(op, string(raw))
		switch op {
		case "DescribeTable":
			var out struct{ Table struct{ TableStatus string } }
			_ = json.Unmarshal([]byte(body), &out)
			index := "ACTIVE"
			if strings.Contains(body, `"IndexStatus":"CREATING"`) {
				index = "CREATING"
			}
			f.events = append(f.events, "read "+out.Table.TableStatus+"/"+index)
		case "UpdateTable":
			f.events = append(f.events, fmt.Sprintf("%s busy=%v", op, len(f.table.violations) > violations))
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Poll: 5 * time.Millisecond}
}

// tableClient is f's client with the real table override, compiled with the
// type's wait, installed for the test. The client sets no Wait, so the
// override's governs.
func tableClient(t *testing.T, f *busyTable, wait string) *Client {
	t.Helper()
	r, errs := compileEdited(t, ddbTableType, func(o *Override) { o.Wait = wait })
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	old := readers[ddbTableType]
	readers[ddbTableType] = r
	t.Cleanup(func() { readers[ddbTableType] = old })
	return f.serve(t)
}

// tableChanges are two properties that are each their own call.
var tableChanges = map[string]any{
	"TableClass":                "STANDARD_INFREQUENT_ACCESS",
	"DeletionProtectionEnabled": false,
}

func updateTable(ctx context.Context, client *Client, changes map[string]any) error {
	return client.Update(ctx, ddbTableType, "t", map[string]any{}, changes)
}

func callsOf(events []string) []string {
	return slices.DeleteFunc(slices.Clone(events), func(e string) bool { return strings.HasPrefix(e, "read ") })
}

// An update waits out a table that is busy before its first call, and
// again before the second, which a service refusing a change to an instance
// still changing would otherwise fail.
func TestUpdateSettlesBeforeEachCall(t *testing.T) {
	f := busyTableFake(3, 0)
	client := tableClient(t, f, "5s")
	if err := updateTable(context.Background(), client, tableChanges); err != nil {
		t.Fatalf("%v\nevents: %v", err, f.events)
	}
	if got := callsOf(f.events); !slices.Equal(got, []string{"UpdateTable busy=false", "UpdateTable busy=false"}) {
		t.Fatalf("calls = %v, want two, neither made while busy\nevents: %v", got, f.events)
	}
}

// A call waits for an index still being created although the table reads
// complete, which only a condition over every element sees.
func TestUpdateSettlesOnAnyElement(t *testing.T) {
	f := busyTableFake(0, 3)
	client := tableClient(t, f, "5s")
	if err := updateTable(context.Background(), client, map[string]any{"TableClass": tableChanges["TableClass"]}); err != nil {
		t.Fatalf("%v\nevents: %v", err, f.events)
	}
	if got := callsOf(f.events); !slices.Equal(got, []string{"UpdateTable busy=false"}) {
		t.Fatalf("calls = %v, want one, made once the index was created\nevents: %v", got, f.events)
	}
}

// A create that has just waited for the instance to settle makes its first
// update call without reading again; the next waits as any does.
func TestApplyOfASettledInstanceMakesItsFirstCallAtOnce(t *testing.T) {
	f := busyTableFake(0, 0)
	client := tableClient(t, f, "5s")
	err := client.apply(context.Background(), readers[ddbTableType], map[string]any{"TableName": "t"}, map[string]any{}, tableChanges, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"UpdateTable busy=false", "read UPDATING/ACTIVE", "read UPDATING/ACTIVE", "read ACTIVE/ACTIVE", "UpdateTable busy=false"}
	if !slices.Equal(f.events, want) {
		t.Fatalf("events = %v, want %v", f.events, want)
	}
}

// An instance that never settles is not changed, and the error says so
// after the override's wait, not the client's default.
func TestUpdateOfAnUnsettledInstanceTimesOut(t *testing.T) {
	f := busyTableFake(1<<30, 0)
	client := tableClient(t, f, "100ms")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := updateTable(ctx, client, tableChanges)
	if err == nil || !strings.Contains(err.Error(), "was still busy after 100ms") {
		t.Fatalf("error = %v, want one saying the table was still busy after 100ms", err)
	}
	if got := callsOf(f.events); len(got) > 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// A client's own wait wins over the type's.
func TestClientWaitOverridesTheTypes(t *testing.T) {
	f := busyTableFake(1<<30, 0)
	client := tableClient(t, f, "1h")
	client.Wait = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := updateTable(ctx, client, tableChanges)
	if err == nil || !strings.Contains(err.Error(), "was still busy after 50ms") {
		t.Fatalf("error = %v, want one saying the table was still busy after 50ms", err)
	}
}

// The type's wait is also the ceiling of the wait for a read to show a
// change.
func TestTypeWaitBoundsTheVisibilityWait(t *testing.T) {
	f := busyTableFake(0, 0)
	client := tableClient(t, f, "100ms")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.waitFor(ctx, ddbTableType, "t", func(map[string]any, error) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "was not visible after 100ms") {
		t.Fatalf("error = %v, want one giving up after 100ms", err)
	}
}

// A call the service keeps refusing is retried for the type's wait, not
// the default two minutes.
func TestTypeWaitBoundsACallsRetries(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"__type":"ContinuousBackupsUnavailableException","message":"busy"}`)
	}))
	t.Cleanup(srv.Close)
	r, errs := compileEdited(t, ddbTableType, func(o *Override) { o.Wait = "100ms" })
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	i := slices.IndexFunc(r.Update, func(m MutationCall) bool { return m.Operation == "UpdateContinuousBackups" })
	if i < 0 || len(r.Update[i].RetryErrors) == 0 {
		t.Fatalf("no UpdateContinuousBackups call with retry errors in %d update calls", len(r.Update))
	}
	client := &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Poll: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.mutate(ctx, r, r.Update[i], map[string]any{"TableName": "t", "PointInTimeRecoverySpecification": map[string]any{"PointInTimeRecoveryEnabled": true}})
	if err == nil || !strings.Contains(err.Error(), "ContinuousBackupsUnavailableException") {
		t.Fatalf("error = %v, want the service's refusal", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("calls = %d, want the call retried", calls)
	}
}

func TestCompileRefusesABadWait(t *testing.T) {
	for wait, want := range map[string]string{
		"0":     `wait "0" must be positive`,
		"20":    `wait "20" is not a duration`,
		"-1s":   `wait "-1s" must be positive`,
		"0s":    `wait "0s" must be positive`,
		"soon":  `wait "soon" is not a duration`,
		"1h30m": "",
	} {
		t.Run(wait, func(t *testing.T) {
			r, errs := compileEdited(t, ddbTableType, func(o *Override) { o.Wait = wait })
			if want == "" {
				if len(errs) > 0 || r.Wait != 90*time.Minute {
					t.Fatalf("errors = %v, wait = %s, want none and 1h30m", errs, r.Wait)
				}
				return
			}
			if !containsErr(errs, want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, want)
			}
		})
	}
}

func TestCompileAnyElementStep(t *testing.T) {
	const bad = "must select one element of a list, step into every element with [], or be a structure"
	cases := map[string]struct {
		path, want string
	}{
		"a structure stepped into as a list": {"SSEDescription[].Status", "SSEDescription " + bad},
		"a list stepped into":                {"GlobalSecondaryIndexes[].IndexStatus", ""},
		"a list not stepped into":            {"GlobalSecondaryIndexes.IndexStatus", "GlobalSecondaryIndexes " + bad},
	}
	for name, c := range cases {
		for _, label := range []string{"busy", "absent"} {
			t.Run(name+"/"+label, func(t *testing.T) {
				_, errs := compileEdited(t, ddbTableType, func(o *Override) {
					spec := map[string][]string{c.path: {"CREATING"}}
					if label == "busy" {
						o.Read.Busy = spec
					} else {
						o.Read.Absent = spec
					}
				})
				if c.want == "" && len(errs) > 0 || c.want != "" && !containsErr(errs, c.want) {
					t.Fatalf("errors = %v, want %q", errs, c.want)
				}
			})
		}
	}
}

// A DynamoDB table is busy while any index is, and absent when an absent
// condition holds of any.
func TestReadDynamoDBBusyOfAnyIndex(t *testing.T) {
	r, errs := compileEdited(t, ddbTableType, func(o *Override) {
		o.Read.Busy = map[string][]string{"TableStatus": {"CREATING", "UPDATING"}, "GlobalSecondaryIndexes[].IndexStatus": {"CREATING", "UPDATING", "DELETING"}}
	})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	cases := map[string]struct {
		table, indexes string
		want           bool
	}{
		"settled":          {"ACTIVE", `[{"IndexName":"a","IndexStatus":"ACTIVE"},{"IndexName":"b","IndexStatus":"ACTIVE"}]`, false},
		"the first index":  {"ACTIVE", `[{"IndexName":"a","IndexStatus":"CREATING"},{"IndexName":"b","IndexStatus":"ACTIVE"}]`, true},
		"the last index":   {"ACTIVE", `[{"IndexName":"a","IndexStatus":"ACTIVE"},{"IndexName":"b","IndexStatus":"DELETING"}]`, true},
		"the table":        {"UPDATING", `[{"IndexName":"a","IndexStatus":"ACTIVE"}]`, true},
		"no indexes":       {"ACTIVE", `[]`, false},
		"an index without": {"ACTIVE", `[{"IndexName":"a"}]`, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := ddbServer(t, func(op, body string) (int, string) {
				if op == "DescribeTable" {
					return 200, `{"Table":{"TableName":"t","TableStatus":"` + c.table + `","GlobalSecondaryIndexes":` + c.indexes + `}}`
				}
				return ddbAnswer(op, body)
			})
			_, _, busy, err := client.readCall(context.Background(), r, map[string]string{"TableName": "t"})
			if err != nil {
				t.Fatal(err)
			}
			if busy != c.want {
				t.Fatalf("busy = %v, want %v", busy, c.want)
			}
		})
	}
}

// Under an XML protocol a condition over every element holds when any
// element has one of the values.
func TestReadXMLBusyOfAnyElement(t *testing.T) {
	r, errs := compileEdited(t, associationsType, func(o *Override) {
		o.Read.Busy = map[string][]string{"Associations[].AssociationState.State": {"associating"}}
	})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	xml := func(states ...string) string {
		var items string
		for i, s := range states {
			items += fmt.Sprintf(`<item><routeTableAssociationId>rtbassoc-%d</routeTableAssociationId><routeTableId>rtb-1</routeTableId>`+
				`<subnetId>subnet-%d</subnetId><main>false</main><associationState><state>%s</state></associationState></item>`, i+1, i+1, s)
		}
		return `<DescribeRouteTablesResponse><routeTableSet><item><routeTableId>rtb-1</routeTableId><associationSet>` + items +
			`</associationSet></item></routeTableSet></DescribeRouteTablesResponse>`
	}
	cases := map[string]struct {
		states []string
		want   bool
	}{
		"the first of two": {[]string{"associating", "associated"}, true},
		"the last of two":  {[]string{"associated", "associating"}, true},
		"neither":          {[]string{"associated", "disassociating"}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := xmlServerBy(t, map[string]string{"DescribeRouteTables": xml(c.states...)})
			_, _, busy, err := client.readCall(context.Background(), r, map[string]string{"Id": "rtbassoc-1"})
			if err != nil {
				t.Fatal(err)
			}
			if busy != c.want {
				t.Fatalf("busy = %v, want %v", busy, c.want)
			}
		})
	}
}
