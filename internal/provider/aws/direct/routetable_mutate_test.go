package direct

import (
	"context"
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

// routeTable is declared by routetable_test.go.
const subnetRouteTableAssociation = "AWS::EC2::SubnetRouteTableAssociation"

// fakeRouteTables is a stateful EC2 endpoint for route tables and their
// subnet associations, answered as EC2 does: a form request and an XML
// response.
type fakeRouteTables struct {
	mu     sync.Mutex
	tables map[string]*fakeTable
	assocs map[string]*fakeAssoc
	seq    int
	calls  map[string][]url.Values
}

type fakeTable struct {
	vpcID string
	tags  map[string]string
}

type fakeAssoc struct {
	routeTableID, subnetID string
}

func (f *fakeRouteTables) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]url.Values{}
	if f.tables == nil {
		f.tables = map[string]*fakeTable{}
	}
	if f.assocs == nil {
		f.assocs = map[string]*fakeAssoc{}
	}
	tagsXML := func(tags map[string]string) string {
		var b strings.Builder
		for k, v := range tags {
			b.WriteString("<item><key>" + k + "</key><value>" + v + "</value></item>")
		}
		return "<tagSet>" + b.String() + "</tagSet>"
	}
	notFound := func(w http.ResponseWriter, code string) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `<Response><Errors><Error><Code>`+code+`</Code><Message>gone</Message></Error></Errors></Response>`)
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
			f.tables[id] = &fakeTable{vpcID: form.Get("VpcId"), tags: tags}
			_, _ = io.WriteString(w, `<CreateRouteTableResponse><routeTable><routeTableId>`+id+`</routeTableId><vpcId>`+form.Get("VpcId")+`</vpcId>`+tagsXML(tags)+`</routeTable></CreateRouteTableResponse>`)
		case "DeleteRouteTable":
			id := form.Get("RouteTableId")
			if _, ok := f.tables[id]; !ok {
				notFound(w, "InvalidRouteTableID.NotFound")
				return
			}
			delete(f.tables, id)
			_, _ = io.WriteString(w, `<DeleteRouteTableResponse><return>true</return></DeleteRouteTableResponse>`)
		case "CreateTags":
			id := form.Get("ResourceId.1")
			table, ok := f.tables[id]
			if !ok {
				notFound(w, "InvalidRouteTableID.NotFound")
				return
			}
			for i := 1; form.Get("Tag."+strconv.Itoa(i)+".Key") != ""; i++ {
				table.tags[form.Get("Tag."+strconv.Itoa(i)+".Key")] = form.Get("Tag." + strconv.Itoa(i) + ".Value")
			}
			_, _ = io.WriteString(w, `<CreateTagsResponse><return>true</return></CreateTagsResponse>`)
		case "DeleteTags":
			id := form.Get("ResourceId.1")
			table, ok := f.tables[id]
			if !ok {
				notFound(w, "InvalidRouteTableID.NotFound")
				return
			}
			for i := 1; form.Get("Tag."+strconv.Itoa(i)+".Key") != ""; i++ {
				delete(table.tags, form.Get("Tag."+strconv.Itoa(i)+".Key"))
			}
			_, _ = io.WriteString(w, `<DeleteTagsResponse><return>true</return></DeleteTagsResponse>`)
		case "DescribeRouteTables":
			if form.Has("RouteTableId.1") {
				id := form.Get("RouteTableId.1")
				table, ok := f.tables[id]
				if !ok {
					notFound(w, "InvalidRouteTableID.NotFound")
					return
				}
				_, _ = io.WriteString(w, `<DescribeRouteTablesResponse><routeTableSet><item><routeTableId>`+id+`</routeTableId><vpcId>`+table.vpcID+`</vpcId>`+tagsXML(table.tags)+`</item></routeTableSet></DescribeRouteTablesResponse>`)
				return
			}
			if form.Get("Filter.1.Name") == "association.route-table-association-id" {
				assocID := form.Get("Filter.1.Value.1")
				assoc, ok := f.assocs[assocID]
				if !ok {
					_, _ = io.WriteString(w, `<DescribeRouteTablesResponse><routeTableSet/></DescribeRouteTablesResponse>`)
					return
				}
				_, _ = io.WriteString(w, `<DescribeRouteTablesResponse><routeTableSet><item><routeTableId>`+assoc.routeTableID+`</routeTableId>`+
					`<associationSet><item><routeTableAssociationId>`+assocID+`</routeTableAssociationId><subnetId>`+assoc.subnetID+`</subnetId><main>false</main></item></associationSet>`+
					`</item></routeTableSet></DescribeRouteTablesResponse>`)
				return
			}
			_, _ = io.WriteString(w, `<DescribeRouteTablesResponse><routeTableSet/></DescribeRouteTablesResponse>`)
		case "AssociateRouteTable":
			f.seq++
			id := fmt.Sprintf("rtbassoc-%d", f.seq)
			f.assocs[id] = &fakeAssoc{routeTableID: form.Get("RouteTableId"), subnetID: form.Get("SubnetId")}
			_, _ = io.WriteString(w, `<AssociateRouteTableResponse><associationId>`+id+`</associationId></AssociateRouteTableResponse>`)
		case "DisassociateRouteTable":
			id := form.Get("AssociationId")
			if _, ok := f.assocs[id]; !ok {
				notFound(w, "InvalidAssociationID.NotFound")
				return
			}
			delete(f.assocs, id)
			_, _ = io.WriteString(w, `<DisassociateRouteTableResponse><return>true</return></DisassociateRouteTableResponse>`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

var routeTableNameTag = map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-rtb"}

// A route table's create sends its VPC and tags as an ec2Query form and
// reads its identifier from the XML response's routeTable.routeTableId;
// tags are added and removed by CreateTags and DeleteTags, the removed ones
// sent as structures naming each key, and a delete of one already gone is
// done.
func TestCreateAndDeleteRouteTable(t *testing.T) {
	f := &fakeRouteTables{}
	client := f.serve(t)
	team := map[string]any{"Key": "team", "Value": "kraai"}
	id, err := client.Create(context.Background(), routeTable, map[string]any{"VpcId": "vpc-1", "Tags": []any{routeTableNameTag, team}})
	if err != nil || id != "rtb-1" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	sent := f.calls["CreateRouteTable"][0]
	if sent.Get("VpcId") != "vpc-1" || sent.Get("TagSpecification.1.ResourceType") != "route-table" ||
		sent.Get("TagSpecification.1.Tag.1.Key") != "kraai:resource-name" || sent.Get("TagSpecification.1.Tag.1.Value") != "kraai-e-rtb" ||
		sent.Get("TagSpecification.1.Tag.2.Key") != "team" || sent.Get("TagSpecification.1.Tag.2.Value") != "kraai" ||
		sent.Get("Version") != "2016-11-15" {
		t.Fatalf("CreateRouteTable form = %v", sent)
	}

	current := map[string]any{"Tags": []any{routeTableNameTag, team}}
	if err := client.Update(context.Background(), routeTable, id, current, map[string]any{"Tags": []any{routeTableNameTag}}); err != nil {
		t.Fatal(err)
	}
	del := f.calls["DeleteTags"][0]
	if del.Get("ResourceId.1") != id || del.Get("Tag.1.Key") != "team" || del.Has("Tag.1.Value") {
		t.Fatalf("DeleteTags form = %v", del)
	}

	if err := client.Delete(context.Background(), routeTable, id); err != nil {
		t.Fatal(err)
	}
	deleted := f.calls["DeleteRouteTable"][0]
	if deleted.Get("RouteTableId") != id {
		t.Fatalf("DeleteRouteTable form = %v", deleted)
	}
	if err := client.Delete(context.Background(), routeTable, id); err != nil {
		t.Fatalf("Delete of a route table already gone = %v, want done", err)
	}
}

// A route table's tags are added by CreateTags, sent by the same
// ResourceId.N/Tag.N.Key/Tag.N.Value form CreateRouteTable's create uses.
func TestUpdateRouteTableAddsTags(t *testing.T) {
	f := &fakeRouteTables{tables: map[string]*fakeTable{"rtb-1": {vpcID: "vpc-1", tags: map[string]string{}}}}
	client := f.serve(t)
	current := map[string]any{"Tags": []any{}}
	changes := map[string]any{"Tags": []any{map[string]any{"Key": "team", "Value": "kraai"}}}
	if err := client.Update(context.Background(), routeTable, "rtb-1", current, changes); err != nil {
		t.Fatal(err)
	}
	add := f.calls["CreateTags"][0]
	if add.Get("ResourceId.1") != "rtb-1" || add.Get("Tag.1.Key") != "team" || add.Get("Tag.1.Value") != "kraai" {
		t.Fatalf("CreateTags form = %v", add)
	}
}

// AssociateRouteTable sends the route table and subnet as scalar form
// members, and the association's identifier is read from the XML
// response's own associationId, with no nesting.
func TestAssociateAndDisassociateRouteTable(t *testing.T) {
	f := &fakeRouteTables{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), subnetRouteTableAssociation, map[string]any{"RouteTableId": "rtb-1", "SubnetId": "subnet-1"})
	if err != nil || id != "rtbassoc-1" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	sent := f.calls["AssociateRouteTable"][0]
	if sent.Get("RouteTableId") != "rtb-1" || sent.Get("SubnetId") != "subnet-1" {
		t.Fatalf("AssociateRouteTable form = %v", sent)
	}

	if err := client.Delete(context.Background(), subnetRouteTableAssociation, id); err != nil {
		t.Fatal(err)
	}
	deleted := f.calls["DisassociateRouteTable"][0]
	if deleted.Get("AssociationId") != id {
		t.Fatalf("DisassociateRouteTable form = %v", deleted)
	}
	if err := client.Delete(context.Background(), subnetRouteTableAssociation, id); err != nil {
		t.Fatalf("Delete of an association already gone = %v, want done", err)
	}
}

// Both of the association's properties are create-only, so it has no
// direct update at all: a changed property is refused, never silently
// dropped.
func TestUpdateRefusesAnAssociationChange(t *testing.T) {
	f := &fakeRouteTables{assocs: map[string]*fakeAssoc{"rtbassoc-1": {routeTableID: "rtb-1", subnetID: "subnet-1"}}}
	client := f.serve(t)
	err := client.Update(context.Background(), subnetRouteTableAssociation, "rtbassoc-1", nil, map[string]any{"SubnetId": "subnet-2"})
	if err == nil || !strings.Contains(err.Error(), "no direct update for SubnetId") {
		t.Fatalf("Update = %v", err)
	}
}

// EC2 answers in XML, and the compiler refuses a busy condition under any
// XML protocol; the association's AssociationState settling is therefore
// not modeled as Read.Busy, and any attempt to add it fails to compile
// rather than silently doing nothing.
func TestCompileRefusesBusyOnTheAssociation(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	var o Override
	found := false
	for _, c := range all {
		if c.Type == subnetRouteTableAssociation {
			o, found = c, true
		}
	}
	if !found {
		t.Fatalf("%s has no override", subnetRouteTableAssociation)
	}
	o.Read.Busy = map[string][]string{"Associations[RouteTableAssociationId={Id}].AssociationState.State": {"associating", "disassociating"}}
	if _, errs := compileOne(files, lock, o); !containsErr(errs, "busy is not supported under ec2Query") {
		t.Fatalf("errors = %v, want one refusing busy under ec2Query", errs)
	}
}
