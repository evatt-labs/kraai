package direct

const routeFixtureType = "Test::EC2::Route"

// fixtureRoute is a type with a composite identifier, its table and
// whichever destination the create sent: a response path selecting by any of
// three members, the create's identifier the first alternative it sent, and
// a delete and an update naming the destination by the member that held it.
var fixtureRoute = ec2Fixture{
	typeName: routeFixtureType,
	schema: schemaOf(routeFixtureType, []string{"RouteTableId", "CidrBlock"}, map[string]any{
		"RouteTableId": fxStr, "CidrBlock": fxStr, "DestinationCidrBlock": fxStr, "DestinationIpv6CidrBlock": fxStr,
		"DestinationPrefixListId": fxStr, "GatewayId": fxStr, "NatGatewayId": fxStr, "VpcEndpointId": fxStr,
	}, map[string]any{
		"createOnlyProperties": pointers("RouteTableId", "DestinationCidrBlock", "DestinationIpv6CidrBlock", "DestinationPrefixListId"),
		"readOnlyProperties":   pointers("CidrBlock"),
	}),
	yaml: `
type: Test::EC2::Route
read:
  model: ` + ec2Model + `
  operation: DescribeRouteTables
  identifier:
    RouteTableId: RouteTableIds
  response: RouteTables[].Routes[DestinationCidrBlock|DestinationIpv6CidrBlock|DestinationPrefixListId={CidrBlock}]
  capture:
    DestinationCidrBlock: DestinationCidrBlock
    DestinationIpv6CidrBlock: DestinationIpv6CidrBlock
    DestinationPrefixListId: DestinationPrefixListId
properties:
  CidrBlock: "{CidrBlock}"
  DestinationCidrBlock: DestinationCidrBlock
  DestinationIpv6CidrBlock: DestinationIpv6CidrBlock
  DestinationPrefixListId: DestinationPrefixListId
  GatewayId: GatewayId
  NatGatewayId: NatGatewayId
  RouteTableId: "{RouteTableId}"
  VpcEndpointId: GatewayId
create:
  operation: CreateRoute
  input:
    RouteTableId: "{RouteTableId}"
    DestinationCidrBlock: "{DestinationCidrBlock}"
    DestinationIpv6CidrBlock: "{DestinationIpv6CidrBlock}"
    DestinationPrefixListId: "{DestinationPrefixListId}"
    GatewayId: "{GatewayId}"
    NatGatewayId: "{NatGatewayId}"
    VpcEndpointId: "{VpcEndpointId}"
  identifier:
    RouteTableId: "{RouteTableId}"
    CidrBlock: "{DestinationCidrBlock|DestinationIpv6CidrBlock|DestinationPrefixListId}"
update:
  - operation: ReplaceRoute
    properties:
      - GatewayId
      - NatGatewayId
      - VpcEndpointId
    input:
      RouteTableId: "{RouteTableId}"
      DestinationCidrBlock: "{DestinationCidrBlock}"
      DestinationIpv6CidrBlock: "{DestinationIpv6CidrBlock}"
      DestinationPrefixListId: "{DestinationPrefixListId}"
      GatewayId: "{GatewayId}"
      NatGatewayId: "{NatGatewayId}"
      VpcEndpointId: "{VpcEndpointId}"
delete:
  operation: DeleteRoute
  input:
    RouteTableId: "{RouteTableId}"
    DestinationCidrBlock: "{DestinationCidrBlock}"
    DestinationIpv6CidrBlock: "{DestinationIpv6CidrBlock}"
    DestinationPrefixListId: "{DestinationPrefixListId}"
  absentErrors: [InvalidRoute.NotFound]
undeclaredErrors:
  InvalidRoute.NotFound: EC2's model declares no errors.
`,
}

const attachmentFixtureType = "Test::EC2::VPCGatewayAttachment"

// fixtureAttachment is a type whose reader serves only some identifiers, and
// whose update calls a before call first, with an absent code of its own.
var fixtureAttachment = ec2Fixture{
	typeName: attachmentFixtureType,
	schema: schemaOf(attachmentFixtureType, []string{"AttachmentType", "VpcId"}, map[string]any{
		"AttachmentType": fxStr, "InternetGatewayId": fxStr, "VpcId": fxStr, "VpnGatewayId": fxStr,
	}, map[string]any{
		"createOnlyProperties": pointers("VpcId"),
		"readOnlyProperties":   pointers("AttachmentType"),
	}),
	yaml: `
type: Test::EC2::VPCGatewayAttachment
read:
  model: ` + ec2Model + `
  operation: DescribeInternetGateways
  input:
    Filters:
      - Name: attachment.vpc-id
        Values: ["{VpcId}"]
  response: InternetGateways[]
  serves:
    AttachmentType: [IGW]
  unserved:
    VpnGatewayId: only a VPN gateway attachment has one
  capture:
    CurrentInternetGatewayId: InternetGatewayId
properties:
  AttachmentType: "{AttachmentType}"
  InternetGatewayId: InternetGatewayId
  VpcId: "{VpcId}"
create:
  operation: AttachInternetGateway
  input:
    InternetGatewayId: "{InternetGatewayId}"
    VpcId: "{VpcId}"
  identifier:
    AttachmentType: =IGW
    VpcId: "{VpcId}"
update:
  - operation: AttachInternetGateway
    properties: [InternetGatewayId]
    input:
      InternetGatewayId: "{InternetGatewayId}"
      VpcId: "{VpcId}"
    before:
      operation: DetachInternetGateway
      input:
        InternetGatewayId: "{CurrentInternetGatewayId}"
        VpcId: "{VpcId}"
      absentErrors: [Gateway.NotAttached]
delete:
  operation: DetachInternetGateway
  input:
    InternetGatewayId: "{CurrentInternetGatewayId}"
    VpcId: "{VpcId}"
  absentErrors: [Gateway.NotAttached]
unsupported:
  VpnGatewayId: the attachment of a VPN gateway is made through Cloud Control
undeclaredErrors:
  Gateway.NotAttached: EC2's model declares no errors.
`,
}
