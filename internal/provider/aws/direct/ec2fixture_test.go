package direct

import (
	"bytes"
	"maps"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"go.yaml.in/yaml/v3"
)

// The ec2Query protocol has one checked-in type, a security group, and the
// behaviours that hold of any XML read or call (a selection from a list, a
// condition over a selected element, a fixed input, a token on a create, a
// further call) have no other type to run on. These fixtures are overrides
// written here over the checked-in EC2 model and a schema each lists the
// properties of, compiled by the real compiler, so the paths they exercise
// are the ones a checked-in override takes.

const ec2Model = "ec2/service/2016-11-15/ec2-2016-11-15.json"

// ec2Fixture is an override's YAML and the schema it compiles against.
type ec2Fixture struct {
	typeName string
	yaml     string
	schema   map[string]any
}

var (
	baseOnce sync.Once
	baseFS   fstest.MapFS
)

// ec2Inputs is the checked-in files with the fixture's schema added, and the
// lock naming it.
func (f ec2Fixture) inputs(t *testing.T) (fstest.MapFS, Lock) {
	t.Helper()
	baseOnce.Do(func() { baseFS = copyFS(t) })
	fsys := maps.Clone(baseFS)
	fsys["fixture-schema.json"] = &fstest.MapFile{Data: encode(t, f.schema)}
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	lock.Schemas = maps.Clone(lock.Schemas)
	lock.Schemas[f.typeName] = LockedFile{File: "fixture-schema.json"}
	return fsys, lock
}

// override decodes the YAML after replacing each [old, new] pair once.
func (f ec2Fixture) override(t *testing.T, edits ...[2]string) Override {
	t.Helper()
	text := f.yaml
	for _, e := range edits {
		if !strings.Contains(text, e[0]) {
			t.Fatalf("%s's override does not contain %q", f.typeName, e[0])
		}
		text = strings.Replace(text, e[0], e[1], 1)
	}
	var o Override
	dec := yaml.NewDecoder(bytes.NewReader([]byte(text)))
	dec.KnownFields(true)
	if err := dec.Decode(&o); err != nil {
		t.Fatal(err)
	}
	return o
}

// compile compiles the override as edited, with the compiler's errors.
func (f ec2Fixture) compile(t *testing.T, edits ...[2]string) (Reader, []error) {
	t.Helper()
	fsys, lock := f.inputs(t)
	return compileOne(fsys, lock, f.override(t, edits...))
}

// register compiles the override and installs its reader for the test.
func (f ec2Fixture) register(t *testing.T, edits ...[2]string) Reader {
	t.Helper()
	r, errs := f.compile(t, edits...)
	if len(errs) > 0 {
		t.Fatalf("%s does not compile: %v", f.typeName, errs)
	}
	old, had := readers[f.typeName]
	readers[f.typeName] = r
	t.Cleanup(func() {
		if had {
			readers[f.typeName] = old
		} else {
			delete(readers, f.typeName)
		}
	})
	return r
}

func schemaOf(typeName string, identifier []string, properties map[string]any, extra map[string]any) map[string]any {
	ids := make([]any, len(identifier))
	for i, id := range identifier {
		ids[i] = "/properties/" + id
	}
	s := map[string]any{
		"typeName":          typeName,
		"primaryIdentifier": ids,
		"properties":        properties,
		"handlers":          map[string]any{"read": map[string]any{"permissions": []any{"ec2:Describe*"}}},
	}
	maps.Copy(s, extra)
	return s
}

func pointers(names ...string) []any {
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = "/properties/" + n
	}
	return out
}

var (
	fxStr     = map[string]any{"type": "string"}
	fxBool    = map[string]any{"type": "boolean"}
	fxStrings = map[string]any{"type": "array", "items": fxStr}
	fxTags    = map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"Key": fxStr, "Value": fxStr}}}
)

const subnetFixtureType = "Test::EC2::Subnet"

// fixtureSubnet reads a subnet as the removed override did: by a list of
// ids, with booleans, a list of structures, a nested structure, a block
// selected by the state it is in, and the network ACL association found by
// a filtered call and selected by the subnet.
var fixtureSubnet = ec2Fixture{
	typeName: subnetFixtureType,
	schema: schemaOf(subnetFixtureType, []string{"SubnetId"}, map[string]any{
		"SubnetId": fxStr, "CidrBlock": fxStr, "MapPublicIpOnLaunch": fxBool, "EnableDns64": fxBool, "Tags": fxTags,
		"Ipv6CidrBlock": fxStr, "Ipv6CidrBlocks": fxStrings, "NetworkAclAssociationId": fxStr,
		"PrivateDnsNameOptionsOnLaunch": map[string]any{"type": "object", "properties": map[string]any{
			"HostnameType": fxStr, "EnableResourceNameDnsARecord": fxBool, "EnableResourceNameDnsAAAARecord": fxBool,
		}},
	}, map[string]any{"readOnlyProperties": pointers("SubnetId", "NetworkAclAssociationId", "Ipv6CidrBlocks")}),
	yaml: `
type: Test::EC2::Subnet
read:
  model: ` + ec2Model + `
  operation: DescribeSubnets
  absentErrors: [InvalidSubnetID.NotFound]
  identifier:
    SubnetId: SubnetIds
  response: Subnets[]
properties:
  CidrBlock: CidrBlock
  EnableDns64: EnableDns64
  Ipv6CidrBlock: Ipv6CidrBlockAssociationSet[Ipv6CidrBlockState/State=associated].Ipv6CidrBlock
  Ipv6CidrBlocks:
    member: Ipv6CidrBlockAssociationSet[Ipv6CidrBlockState/State=associated]*.Ipv6CidrBlock
  MapPublicIpOnLaunch: MapPublicIpOnLaunch
  PrivateDnsNameOptionsOnLaunch:
    member: PrivateDnsNameOptionsOnLaunch
    properties:
      EnableResourceNameDnsAAAARecord: EnableResourceNameDnsAAAARecord
      EnableResourceNameDnsARecord: EnableResourceNameDnsARecord
      HostnameType: HostnameType
  SubnetId: SubnetId
  Tags:
    member: Tags
    properties:
      Key: Key
      Value: Value
also:
  - operation: DescribeNetworkAcls
    input:
      Filters:
        - Name: association.subnet-id
          Values: ["{SubnetId}"]
    response: NetworkAcls[]
    properties:
      NetworkAclAssociationId:
        member: Associations[SubnetId={SubnetId}].NetworkAclAssociationId
update:
  # The three DNS options are three attributes, so three calls; one for a
  # member the desired structure leaves out is not sent.
  - operation: ModifySubnetAttribute
    properties: [PrivateDnsNameOptionsOnLaunch]
    input:
      SubnetId: "{SubnetId}"
      PrivateDnsHostnameTypeOnLaunch: "{PrivateDnsNameOptionsOnLaunch.HostnameType}"
  - operation: ModifySubnetAttribute
    properties: [PrivateDnsNameOptionsOnLaunch]
    input:
      SubnetId: "{SubnetId}"
      EnableResourceNameDnsARecordOnLaunch: {Value: "{PrivateDnsNameOptionsOnLaunch.EnableResourceNameDnsARecord}"}
  - operation: ModifySubnetAttribute
    properties: [PrivateDnsNameOptionsOnLaunch]
    input:
      SubnetId: "{SubnetId}"
      EnableResourceNameDnsAAAARecordOnLaunch: {Value: "{PrivateDnsNameOptionsOnLaunch.EnableResourceNameDnsAAAARecord}"}
undeclaredErrors:
  InvalidSubnetID.NotFound: EC2's model declares no errors.
`,
}

const associationFixtureType = "Test::EC2::Association"

// fixtureAssociation reads an association through the route table holding
// it, by a filter on its own id, absent when it is the table's main one and
// busy while it is associating.
var fixtureAssociation = ec2Fixture{
	typeName: associationFixtureType,
	schema: schemaOf(associationFixtureType, []string{"Id"}, map[string]any{
		"Id": fxStr, "RouteTableId": fxStr, "SubnetId": fxStr,
	}, map[string]any{"readOnlyProperties": pointers("Id")}),
	yaml: `
type: Test::EC2::Association
read:
  model: ` + ec2Model + `
  operation: DescribeRouteTables
  input:
    Filters:
      - Name: association.route-table-association-id
        Values: ["{Id}"]
  response: RouteTables[]
  absent:
    Associations[RouteTableAssociationId={Id}].Main: ["true"]
  busy:
    Associations[RouteTableAssociationId={Id}].AssociationState.State: [associating]
properties:
  Id: Associations[RouteTableAssociationId={Id}].RouteTableAssociationId
  RouteTableId: RouteTableId
  SubnetId: Associations[RouteTableAssociationId={Id}].SubnetId
`,
}

const vpcFixtureType = "Test::EC2::Vpc"

// fixtureVPC reads a VPC as the removed override did: busy while pending,
// the default network ACL and security group found by filtered calls, and
// each DNS attribute read in its own call to one operation, told apart by a
// fixed input. Its create names nothing and carries no token.
var fixtureVPC = ec2Fixture{
	typeName: vpcFixtureType,
	schema: schemaOf(vpcFixtureType, []string{"VpcId"}, map[string]any{
		"VpcId": fxStr, "CidrBlock": fxStr, "CidrBlockAssociations": fxStrings, "Ipv6CidrBlocks": fxStrings, "Tags": fxTags,
		"DefaultNetworkAcl": fxStr, "DefaultSecurityGroup": fxStr, "EnableDnsSupport": fxBool, "EnableDnsHostnames": fxBool,
	}, map[string]any{
		"createOnlyProperties": pointers("CidrBlock"),
		"readOnlyProperties":   pointers("VpcId", "CidrBlockAssociations", "Ipv6CidrBlocks", "DefaultNetworkAcl", "DefaultSecurityGroup"),
	}),
	yaml: `
type: Test::EC2::Vpc
read:
  model: ` + ec2Model + `
  operation: DescribeVpcs
  absentErrors: [InvalidVpcID.NotFound]
  identifier:
    VpcId: VpcIds
  response: Vpcs[]
  busy:
    State: [pending]
properties:
  CidrBlock: CidrBlock
  CidrBlockAssociations:
    member: CidrBlockAssociationSet[].AssociationId
  Ipv6CidrBlocks:
    member: Ipv6CidrBlockAssociationSet[].Ipv6CidrBlock
  Tags:
    member: Tags
    properties:
      Key: Key
      Value: Value
  VpcId: VpcId
also:
  - operation: DescribeNetworkAcls
    input:
      Filters:
        - Name: vpc-id
          Values: ["{VpcId}"]
        - Name: default
          Values: ["true"]
    response: NetworkAcls[]
    properties:
      DefaultNetworkAcl: NetworkAclId
  - operation: DescribeSecurityGroups
    input:
      Filters:
        - Name: vpc-id
          Values: ["{VpcId}"]
        - Name: group-name
          Values: ["default"]
    response: SecurityGroups[]
    properties:
      DefaultSecurityGroup: GroupId
  - operation: DescribeVpcAttribute
    identifier:
      VpcId: VpcId
    input:
      Attribute: enableDnsSupport
    properties:
      EnableDnsSupport: EnableDnsSupport.Value
  - operation: DescribeVpcAttribute
    identifier:
      VpcId: VpcId
    input:
      Attribute: enableDnsHostnames
    properties:
      EnableDnsHostnames: EnableDnsHostnames.Value
create:
  operation: CreateVpc
  input:
    CidrBlock: "{CidrBlock}"
    TagSpecifications:
      - ResourceType: vpc
        Tags: "{Tags:wire}"
  identifier:
    VpcId: Vpc.VpcId
update:
  - operation: ModifyVpcAttribute
    properties: [EnableDnsSupport]
    input:
      VpcId: "{VpcId}"
      EnableDnsSupport: {Value: "{EnableDnsSupport}"}
  - operation: ModifyVpcAttribute
    properties: [EnableDnsHostnames]
    input:
      VpcId: "{VpcId}"
      EnableDnsHostnames: {Value: "{EnableDnsHostnames}"}
  - tags:
      property: Tags
      add:
        operation: CreateTags
        input:
          Resources: ["{VpcId}"]
          Tags: "{added:wire}"
      remove:
        operation: DeleteTags
        input:
          Resources: ["{VpcId}"]
          Tags: "{removed:keys}"
delete:
  operation: DeleteVpc
  input:
    VpcId: "{VpcId}"
  absentErrors: [InvalidVpcID.NotFound]
undeclaredErrors:
  InvalidVpcID.NotFound: EC2's model declares no errors.
`,
}

const tableFixtureType = "Test::EC2::RouteTable"

// fixtureRouteTable is a create whose input carries an idempotency token the
// model marks, which the template does not set.
var fixtureRouteTable = ec2Fixture{
	typeName: tableFixtureType,
	schema: schemaOf(tableFixtureType, []string{"RouteTableId"}, map[string]any{
		"RouteTableId": fxStr, "VpcId": fxStr, "Tags": fxTags,
	}, map[string]any{
		"createOnlyProperties": pointers("VpcId"),
		"readOnlyProperties":   pointers("RouteTableId"),
	}),
	yaml: `
type: Test::EC2::RouteTable
read:
  model: ` + ec2Model + `
  operation: DescribeRouteTables
  absentErrors: [InvalidRouteTableID.NotFound]
  identifier:
    RouteTableId: RouteTableIds
  response: RouteTables[]
properties:
  RouteTableId: RouteTableId
  Tags:
    member: Tags
    properties:
      Key: Key
      Value: Value
  VpcId: VpcId
create:
  operation: CreateRouteTable
  input:
    VpcId: "{VpcId}"
    TagSpecifications:
      - ResourceType: route-table
        Tags: "{Tags:wire}"
  identifier:
    RouteTableId: RouteTable.RouteTableId
update:
  - tags:
      property: Tags
      add:
        operation: CreateTags
        input:
          Resources: ["{RouteTableId}"]
          Tags: "{added:wire}"
      remove:
        operation: DeleteTags
        input:
          Resources: ["{RouteTableId}"]
          Tags: "{removed:keys}"
delete:
  operation: DeleteRouteTable
  input:
    RouteTableId: "{RouteTableId}"
  absentErrors: [InvalidRouteTableID.NotFound]
undeclaredErrors:
  InvalidRouteTableID.NotFound: EC2's model declares no errors.
`,
}
