package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Cloud Control type names for the egress types a network registers when
// its binding declares a private subnet.
const (
	TypeEIP        = "AWS::EC2::EIP"
	TypeNatGateway = "AWS::EC2::NatGateway"
)

// Registry keys for the private half of a network: a second subnet, route
// table, default route and association, each a role of the type the public
// half already registers. Their identity tags carry the role too
// (roleTaggedLookup), since both halves share one derived name.
var (
	TypePrivateSubnet                      = resource.RoleType(TypeSubnet, "Private")
	TypePrivateRouteTable                  = resource.RoleType(TypeRouteTable, "Private")
	TypePrivateRoute                       = resource.RoleType(TypeRoute, "Private")
	TypePrivateSubnetRouteTableAssociation = resource.RoleType(TypeSubnetRouteTableAssociation, "Private")
)

// privateRole is the tag-value suffix the private half's resources carry.
const privateRole = "private"

// privateSubnetKey is the binding entry key that opts a network into
// egress: the address block of a private subnet whose default route is a
// NAT gateway in the public subnet. Opt-in, never implied by network: a NAT
// gateway is billed by the hour, and an environment kraai makes per branch
// is where that adds up.
const privateSubnetKey = "private"

// hasPrivateSubnet reports whether a network binding's entry opts into
// egress, from the entry's config as a compute spec's bindings carry it.
func hasPrivateSubnet(config map[string]any) bool {
	cidr, _ := config[privateSubnetKey].(string)
	return cidr != ""
}

// roleEndpointID is endpointID for a resource found by a role-qualified tag.
func roleEndpointID(ctx context.Context, client ccAPI, typeName, role, name string) (string, bool, error) {
	id, _, found, err := roleTaggedLookup(client, typeName, role).resolve(ctx, resource.Ref{Name: name})
	return id, found, err
}

// registerEgress returns the registrations that give a network's private
// subnet a route to the internet: an Elastic IP, a NAT gateway holding it
// in the public subnet, the private subnet, its route table, the default
// route through the gateway, and the association joining the two.
//
// Every one applies only when the binding declares a private block, and
// every one belongs to the same binding as the public half, so DependsOn
// reaches across the two.
func registerEgress(client ccAPI) []resource.Registration {
	vpcKey := key(TypeVPC)
	publicSubnetKey := key(TypeSubnet)
	attachmentKey := key(TypeVPCGatewayAttachment)
	eipKey := key(TypeEIP)
	natKey := key(TypeNatGateway)
	privateSubnet := key(TypePrivateSubnet)
	privateRouteTable := key(TypePrivateRouteTable)
	withPrivate := []resource.Applicability{resource.RequiresBindingKey(privateSubnetKey)}

	return []resource.Registration{
		{
			Provider: Provider, Type: TypeEIP, Capability: manifest.CapabilityNetwork,
			Applies: withPrivate, Lookup: resource.LookupByTag,
			Resource: translated(taggedLookup(client, TypeEIP),
				func(spec resource.Spec) (resource.Spec, error) {
					translated := spec
					translated.Config = map[string]any{"Domain": "vpc"}
					return translated, nil
				},
				nil),
		},
		{
			Provider: Provider, Type: TypeNatGateway, Capability: manifest.CapabilityNetwork,
			Applies: withPrivate, Lookup: resource.LookupByTag,
			// The attachment as well as the subnet: a public NAT gateway
			// sends traffic out through the internet gateway its subnet
			// routes to, which must already be attached to the VPC.
			DependsOn: []string{publicSubnetKey, eipKey, attachmentKey},
			Resource: translated(taggedLookup(client, TypeNatGateway),
				func(spec resource.Spec) (resource.Spec, error) {
					subnetID, err := spec.Attribute(publicSubnetKey, "SubnetId")
					if err != nil {
						return spec, err
					}
					allocationID, err := spec.Attribute(eipKey, "AllocationId")
					if err != nil {
						return spec, err
					}
					translated := spec
					translated.Config = map[string]any{
						"SubnetId":         subnetID,
						"AllocationId":     allocationID,
						"ConnectivityType": "public",
					}
					return translated, nil
				},
				nil),
		},
		{
			Provider: Provider, Type: TypePrivateSubnet, VendorType: TypeSubnet,
			Capability: manifest.CapabilityNetwork,
			Applies:    withPrivate, Lookup: resource.LookupByTag, DependsOn: []string{vpcKey},
			Resource: translated(roleTaggedLookup(client, TypeSubnet, privateRole),
				func(spec resource.Spec) (resource.Spec, error) {
					cidr, err := configString(spec, privateSubnetKey)
					if err != nil {
						return spec, err
					}
					vpcID, err := spec.Attribute(vpcKey, "VpcId")
					if err != nil {
						return spec, err
					}
					translated := spec
					translated.Config = map[string]any{
						"VpcId":               vpcID,
						"CidrBlock":           cidr,
						"MapPublicIpOnLaunch": false,
					}
					return translated, nil
				},
				map[string]func(resource.Spec) (string, error){
					"CidrBlock": func(spec resource.Spec) (string, error) {
						return configString(spec, privateSubnetKey)
					},
				}),
		},
		{
			Provider: Provider, Type: TypePrivateRouteTable, VendorType: TypeRouteTable,
			Capability: manifest.CapabilityNetwork,
			Applies:    withPrivate, Lookup: resource.LookupByTag, DependsOn: []string{vpcKey},
			Resource: translated(roleTaggedLookup(client, TypeRouteTable, privateRole),
				func(spec resource.Spec) (resource.Spec, error) {
					vpcID, err := spec.Attribute(vpcKey, "VpcId")
					if err != nil {
						return spec, err
					}
					translated := spec
					translated.Config = map[string]any{"VpcId": vpcID}
					return translated, nil
				},
				nil),
		},
		{
			Provider: Provider, Type: TypePrivateRoute, VendorType: TypeRoute,
			Capability: manifest.CapabilityNetwork,
			Applies:    withPrivate, Lookup: resource.LookupByName,
			DependsOn: []string{privateRouteTable, natKey},
			Resource: &relationshipResource{
				provider: Provider, typeName: TypeRoute, client: client,
				identify: func(ctx context.Context, name string) (string, bool, error) {
					routeTableID, found, err := roleEndpointID(ctx, client, TypeRouteTable, privateRole, name)
					if err != nil || !found {
						return "", false, err
					}
					return routeTableID + "|" + defaultRoute, true, nil
				},
				desired: func(spec resource.Spec) (map[string]any, error) {
					routeTableID, err := spec.Attribute(privateRouteTable, "RouteTableId")
					if err != nil {
						return nil, err
					}
					natID, err := spec.Attribute(natKey, "NatGatewayId")
					if err != nil {
						return nil, err
					}
					return map[string]any{
						"RouteTableId":         routeTableID,
						"DestinationCidrBlock": defaultRoute,
						"NatGatewayId":         natID,
					}, nil
				},
			},
		},
		{
			Provider: Provider, Type: TypePrivateSubnetRouteTableAssociation, VendorType: TypeSubnetRouteTableAssociation,
			Capability: manifest.CapabilityNetwork,
			Applies:    withPrivate, Lookup: resource.LookupByName,
			DependsOn: []string{privateSubnet, privateRouteTable},
			Resource: &relationshipResource{
				provider: Provider, typeName: TypeSubnetRouteTableAssociation, client: client,
				identify: func(ctx context.Context, name string) (string, bool, error) {
					subnetID, found, err := roleEndpointID(ctx, client, TypeSubnet, privateRole, name)
					if err != nil || !found {
						return "", false, err
					}
					routeTableID, found, err := roleEndpointID(ctx, client, TypeRouteTable, privateRole, name)
					if err != nil || !found {
						return "", false, err
					}
					return findAssociation(ctx, client, subnetID, routeTableID)
				},
				desired: func(spec resource.Spec) (map[string]any, error) {
					subnetID, err := spec.Attribute(privateSubnet, "SubnetId")
					if err != nil {
						return nil, err
					}
					routeTableID, err := spec.Attribute(privateRouteTable, "RouteTableId")
					if err != nil {
						return nil, err
					}
					return map[string]any{"SubnetId": subnetID, "RouteTableId": routeTableID}, nil
				},
			},
		},
	}
}
