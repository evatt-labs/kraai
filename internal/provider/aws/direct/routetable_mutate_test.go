package direct

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// routeTable is the fixture whose create carries an idempotency token.
const routeTable = tableFixtureType

// fakeRouteTables is a stateful EC2 endpoint for fixtureRouteTable's
// route tables, answered as EC2 does: a form request and an XML response.
type fakeRouteTables struct {
	mu     sync.Mutex
	tables map[string]map[string]string
	seq    int
	calls  map[string][]url.Values
}

func (f *fakeRouteTables) serve(t *testing.T) *Client {
	t.Helper()
	fixtureRouteTable.register(t)
	f.calls = map[string][]url.Values{}
	if f.tables == nil {
		f.tables = map[string]map[string]string{}
	}
	tagsXML := func(tags map[string]string) string {
		var b strings.Builder
		for k, v := range tags {
			b.WriteString("<item><key>" + k + "</key><value>" + v + "</value></item>")
		}
		return "<tagSet>" + b.String() + "</tagSet>"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		op := form.Get("Action")
		f.calls[op] = append(f.calls[op], form)
		switch op {
		case "CreateRouteTable":
			f.seq++
			id := fmt.Sprintf("rtb-%d", f.seq)
			tags := map[string]string{}
			for i := 1; form.Get("TagSpecification.1.Tag."+strconv.Itoa(i)+".Key") != ""; i++ {
				tags[form.Get("TagSpecification.1.Tag."+strconv.Itoa(i)+".Key")] = form.Get("TagSpecification.1.Tag." + strconv.Itoa(i) + ".Value")
			}
			f.tables[id] = tags
			_, _ = io.WriteString(w, `<CreateRouteTableResponse><routeTable><routeTableId>`+id+`</routeTableId><vpcId>`+form.Get("VpcId")+`</vpcId>`+tagsXML(tags)+`</routeTable></CreateRouteTableResponse>`)
		case "DescribeRouteTables":
			id := form.Get("RouteTableId.1")
			tags, ok := f.tables[id]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `<Response><Errors><Error><Code>InvalidRouteTableID.NotFound</Code><Message>gone</Message></Error></Errors></Response>`)
				return
			}
			_, _ = io.WriteString(w, `<DescribeRouteTablesResponse><routeTableSet><item><routeTableId>`+id+`</routeTableId><vpcId>vpc-1</vpcId>`+tagsXML(tags)+`</item></routeTableSet></DescribeRouteTablesResponse>`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

var routeTableNameTag = map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-rtb"}

// The association's AssociationState settling is a busy condition under
// ec2Query, checked against the model's enum: a value the state does not
// have fails to compile.
func TestCompileChecksBusyOnTheAssociation(t *testing.T) {
	const declared = "Associations[RouteTableAssociationId={Id}].AssociationState.State: [associating]"
	const path = "Associations[RouteTableAssociationId={Id}].AssociationState.State: "
	if r, errs := fixtureAssociation.compile(t, [2]string{declared, path + "[associating, disassociating]"}); len(errs) > 0 || len(r.Busy) != 1 {
		t.Fatalf("compile = %d busy conditions, %v", len(r.Busy), errs)
	}
	if _, errs := fixtureAssociation.compile(t, [2]string{declared, path + "[settling]"}); !containsErr(errs, "not one of") {
		t.Fatalf("errors = %v, want one refusing a value the enum lacks", errs)
	}
}
