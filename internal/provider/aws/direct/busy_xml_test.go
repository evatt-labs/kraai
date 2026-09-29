package direct

import (
	"context"
	"testing"
)

// A create returns once the VPC has left the pending state, not when the
// first read finds it.
func TestCreateVPCWaitsOutPending(t *testing.T) {
	f := &fakeVPC{createPending: 4}
	client := f.serve(t)
	if _, err := client.Create(context.Background(), vpcType, map[string]any{"CidrBlock": "10.99.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	if f.pending != 0 {
		t.Fatalf("Create returned with the VPC still pending for %d reads", f.pending)
	}
}

// An update waits for a pending VPC too.
func TestUpdateVPCWaitsOutPending(t *testing.T) {
	f := &fakeVPC{id: "vpc-0123", cidr: "10.99.0.0/16", pending: 4}
	client := f.serve(t)
	changes := map[string]any{"EnableDnsHostnames": true}
	if err := client.Update(context.Background(), vpcType, "vpc-0123", map[string]any{}, changes); err != nil {
		t.Fatal(err)
	}
	if f.pending != 0 {
		t.Fatalf("Update returned with the VPC still pending for %d reads", f.pending)
	}
}

const associationsType = "AWS::EC2::SubnetRouteTableAssociation"

func associationXML(state string) string {
	return `<DescribeRouteTablesResponse><routeTableSet><item><routeTableId>rtb-1</routeTableId><associationSet><item>` +
		`<routeTableAssociationId>rtbassoc-1</routeTableAssociationId><routeTableId>rtb-1</routeTableId><subnetId>subnet-1</subnetId>` +
		`<main>false</main><associationState><state>` + state + `</state></associationState></item></associationSet></item></routeTableSet></DescribeRouteTablesResponse>`
}

// A read under an XML protocol reports busy for a declared value, found
// through a selected list element, and only for those.
func TestReadXMLReportsBusy(t *testing.T) {
	for state, want := range map[string]bool{"associating": true, "associated": false, "disassociating": false} {
		t.Run(state, func(t *testing.T) {
			client, _ := xmlServerBy(t, map[string]string{"DescribeRouteTables": associationXML(state)})
			props, _, busy, err := client.readCall(context.Background(), readers[associationsType], map[string]string{"Id": "rtbassoc-1"})
			if err != nil {
				t.Fatal(err)
			}
			if busy != want || props["SubnetId"] != "subnet-1" {
				t.Fatalf("busy = %v, props = %v, want busy %v", busy, props, want)
			}
		})
	}
}
