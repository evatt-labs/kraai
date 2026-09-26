package direct

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A rule's ARN is read by the name, and the bus when it names one: the
// default bus's ARN has none, and the input is left unset.
func TestReadRuleByARN(t *testing.T) {
	for name, c := range map[string]struct {
		arn, describe, targets string
	}{
		"default bus": {"arn:aws:events:us-east-1:1:rule/r", `{"Name":"r"}`, `{"Rule":"r"}`},
		"custom bus":  {"arn:aws:events:us-east-1:1:rule/b/r", `{"EventBusName":"b","Name":"r"}`, `{"EventBusName":"b","Rule":"r"}`},
	} {
		t.Run(name, func(t *testing.T) {
			client, seen := targetServer(t, map[string]string{
				"DescribeRule":        `{"Arn":"` + c.arn + `","Name":"r","State":"ENABLED","EventPattern":"{\"source\":[\"s\"]}"}`,
				"ListTargetsByRule":   `{"Targets":[{"Id":"q","Arn":"arn:aws:sqs:us-east-1:1:q","RetryPolicy":{"MaximumRetryAttempts":2}}]}`,
				"ListTagsForResource": `{"Tags":[{"Key":"team","Value":"cloud"}]}`,
			})
			got, err := client.ReadByID(context.Background(), "AWS::Events::Rule", c.arn)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				"Arn": c.arn, "Name": "r", "RuleName": "r", "State": "ENABLED",
				"EventPattern": map[string]any{"source": []any{"s"}},
				"Targets": []any{map[string]any{"Id": "q", "Arn": "arn:aws:sqs:us-east-1:1:q",
					"RetryPolicy": map[string]any{"MaximumRetryAttempts": json.Number("2")}}},
				"Tags": []any{map[string]any{"Key": "team", "Value": "cloud"}},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Read = %#v\nwant   %#v", got, want)
			}
			if seen["DescribeRule"] != c.describe || seen["ListTargetsByRule"] != c.targets {
				t.Fatalf("requests = %s, %s; want %s, %s", seen["DescribeRule"], seen["ListTargetsByRule"], c.describe, c.targets)
			}
			if !strings.Contains(seen["ListTagsForResource"], c.arn) {
				t.Fatalf("tags request = %s, want the ARN", seen["ListTagsForResource"])
			}
		})
	}
}

// routeTablesXML holds one route table with a subnet association and the
// main association.
const routeTablesXML = `<DescribeRouteTablesResponse><routeTableSet><item>
<routeTableId>rtb-1</routeTableId><associationSet>
<item><routeTableAssociationId>rtbassoc-main</routeTableAssociationId><main>true</main></item>
<item><routeTableAssociationId>rtbassoc-1</routeTableAssociationId><subnetId>subnet-1</subnetId><main>false</main></item>
</associationSet></item></routeTableSet></DescribeRouteTablesResponse>`

// A subnet association is read through its route table by the one id that
// addresses it; the table's main association belongs to no subnet and is
// absent, as Cloud Control reads it.
func TestReadSubnetRouteTableAssociation(t *testing.T) {
	const typeName = "AWS::EC2::SubnetRouteTableAssociation"
	client, forms := xmlServer(t, 200, routeTablesXML)
	got, err := client.ReadByID(context.Background(), typeName, "rtbassoc-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"Id": "rtbassoc-1", "RouteTableId": "rtb-1", "SubnetId": "subnet-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v, want %#v", got, want)
	}
	if f := (*forms)[0]; f.Get("Filter.1.Value.1") != "rtbassoc-1" || f.Get("Id") != "" {
		t.Fatalf("form = %v, want the id only in the filter", f)
	}
	if _, err := client.ReadByID(context.Background(), typeName, "rtbassoc-main"); !errors.Is(err, ErrAbsent) {
		t.Fatalf("Read of the main association = %v, want ErrAbsent", err)
	}
}

func TestCompileRefusesAnUnknownFilter(t *testing.T) {
	o := taggedOverride()
	o.Read.Input = map[string]any{"include": "{WidgetId:upper}"}
	if _, errs := compileTagged(t, taggedWidget(), o); !containsErr(errs, "filters {WidgetId} by upper; the filters are arnName and arnParent") {
		t.Fatalf("errors = %v", errs)
	}
}
