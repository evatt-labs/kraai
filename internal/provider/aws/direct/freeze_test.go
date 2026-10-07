package direct

import (
	"slices"
	"strings"
	"testing"
)

// frozenTypes is every type the direct package carries an override for: the
// types kraai's capabilities create, and no others.
var frozenTypes = []string{
	"AWS::ApiGatewayV2::Api",
	"AWS::DynamoDB::Table",
	"AWS::EC2::InternetGateway",
	"AWS::EC2::Route",
	"AWS::EC2::RouteTable",
	"AWS::EC2::SecurityGroup",
	"AWS::EC2::Subnet",
	"AWS::EC2::SubnetRouteTableAssociation",
	"AWS::EC2::VPC",
	"AWS::EC2::VPCGatewayAttachment",
	"AWS::Events::Rule",
	"AWS::IAM::Role",
	"AWS::Lambda::Function",
	"AWS::Lambda::Permission",
	"AWS::Lambda::Url",
	"AWS::RDS::DBSubnetGroup",
	"AWS::S3::Bucket",
	"AWS::SQS::Queue",
	"AWS::SSM::Parameter",
}

func TestDirectIsFrozenToTheTypesKraaiCreates(t *testing.T) {
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no overrides are checked in, so nothing was checked")
	}
	var stray []string
	for _, o := range all {
		if !slices.Contains(frozenTypes, o.Type) {
			stray = append(stray, o.Type)
		}
	}
	if len(stray) > 0 {
		slices.Sort(stray)
		t.Fatalf("the direct package is frozen to the types kraai's capabilities create (see the refocus issue #499), "+
			"but it has an override for %s; a type is added only alongside the capability that creates it, "+
			"and then to frozenTypes in freeze_test.go", strings.Join(stray, ", "))
	}
}
