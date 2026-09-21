package aws

import (
	"context"
	"encoding/binary"
	"net/netip"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Every subnet tier a network declares is a pair, one subnet in each of two
// availability zones: a database subnet group needs two zones, a serverless
// cache is happiest with two, and a function in two loses nothing. The
// manifest declares one block per tier, and kraai halves it.

// Registry keys for the second subnet of each tier, and the association
// joining it to its tier's route table. The first public subnet keeps the
// bare TypeSubnet key it always had.
var (
	TypePublicSubnetB                       = resource.RoleType(TypeSubnet, "PublicB")
	TypePublicSubnetBRouteTableAssociation  = resource.RoleType(TypeSubnetRouteTableAssociation, "PublicB")
	TypePrivateSubnetB                      = resource.RoleType(TypeSubnet, "PrivateB")
	TypePrivateSubnetBRouteTableAssociation = resource.RoleType(TypeSubnetRouteTableAssociation, "PrivateB")
)

// Tag-value roles for the second subnet of each tier. The first public subnet
// carries the bare name; the first private one carries privateRole.
const (
	publicBRole  = "public-b"
	privateBRole = "private-b"
)

// azsKey is the binding entry key naming the two availability zones a
// network's subnets are spread across, for an account whose region does
// not expose zones a and b.
const azsKey = "azs"

// splitBlock halves an address block into two equal subnets. A block must
// be a /27 or larger: EC2's smallest subnet is a /28, and half of a /28 is
// not one.
func splitBlock(cidr string) ([2]string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return [2]string{}, kerrors.Validation("%q is not an address block: %v", cidr, err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() {
		return [2]string{}, kerrors.Validation("%q is not an IPv4 block", cidr)
	}
	if prefix.Bits() > 27 {
		return [2]string{}, kerrors.Validation(
			"%q is too small to split into two subnets; a tier's block must be a /27 or larger", cidr)
	}
	half := prefix.Bits() + 1
	first := netip.PrefixFrom(prefix.Addr(), half)
	base := prefix.Addr().As4()
	second := binary.BigEndian.Uint32(base[:]) + 1<<(32-half)
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], second)
	return [2]string{first.String(), netip.PrefixFrom(netip.AddrFrom4(raw), half).String()}, nil
}

// networkZones returns the two availability zones a network's subnets span:
// the binding's azs when it names them, otherwise zones a and b of the
// region, which every commercial region exposes to most accounts. An
// account whose region maps its zones differently (us-west-1 exposes two of
// three) names them in the binding.
func networkZones(spec resource.Spec, region string) ([2]string, error) {
	raw, present := spec.Config[azsKey]
	if !present {
		return [2]string{region + "a", region + "b"}, nil
	}
	list, _ := raw.([]any)
	if len(list) != 2 {
		return [2]string{}, kerrors.Validation(
			"network binding %q: azs must name exactly two availability zones, got %v", spec.Binding, raw)
	}
	var zones [2]string
	for i, entry := range list {
		zone, _ := entry.(string)
		if zone == "" {
			return [2]string{}, kerrors.Validation(
				"network binding %q: azs[%d] is not a zone name: %v", spec.Binding, i, entry)
		}
		zones[i] = zone
	}
	if zones[0] == zones[1] {
		return [2]string{}, kerrors.Validation(
			"network binding %q: azs names %q twice; the two subnets must be in different zones", spec.Binding, zones[0])
	}
	return zones, nil
}

// tierHalf is one subnet of a tier: which half of the tier's block it
// takes, and which of the two zones it sits in.
func tierHalf(spec resource.Spec, region, field string, half int) (cidr, zone string, err error) {
	block, err := configString(spec, field)
	if err != nil {
		return "", "", err
	}
	halves, err := splitBlock(block)
	if err != nil {
		return "", "", kerrors.Wrap(err, kerrors.CodeValidation, "network binding %q: %s", spec.Binding, field)
	}
	zones, err := networkZones(spec, region)
	if err != nil {
		return "", "", err
	}
	return halves[half], zones[half], nil
}

// lookupForRole is roleTaggedLookup, or taggedLookup for the empty role.
func lookupForRole(client ccAPI, typeName, role string) *resourceType {
	if role == "" {
		return taggedLookup(client, typeName)
	}
	return roleTaggedLookup(client, typeName, role)
}

// endpointIDForRole is roleEndpointID, or endpointID for the empty role.
func endpointIDForRole(ctx context.Context, client ccAPI, typeName, role, name string) (string, bool, error) {
	if role == "" {
		return endpointID(ctx, client, typeName, name)
	}
	return roleEndpointID(ctx, client, typeName, role, name)
}

// subnetRegistration builds one subnet of a tier: half of the tier's block
// (the binding's field), in one of the two zones, public or private,
// tagged with role.
func subnetRegistration(client ccAPI, region, typeKey, role, field string, half int, public bool,
	applies []resource.Applicability,
) resource.Registration {
	vpcKey := key(TypeVPC)
	reg := resource.Registration{
		Provider: Provider, Type: typeKey, Capability: manifest.CapabilityNetwork,
		Applies: applies, Lookup: resource.LookupByTag, DependsOn: []string{vpcKey},
		Resource: translated(lookupForRole(client, TypeSubnet, role),
			func(spec resource.Spec) (resource.Spec, error) {
				cidr, zone, err := tierHalf(spec, region, field, half)
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
					"AvailabilityZone":    zone,
					"MapPublicIpOnLaunch": public,
				}
				return translated, nil
			},
			map[string]func(resource.Spec) (string, error){
				"CidrBlock": func(spec resource.Spec) (string, error) {
					cidr, _, err := tierHalf(spec, region, field, half)
					return cidr, err
				},
				"AvailabilityZone": func(spec resource.Spec) (string, error) {
					_, zone, err := tierHalf(spec, region, field, half)
					return zone, err
				},
			}),
	}
	if typeKey != TypeSubnet {
		reg.VendorType = TypeSubnet
	}
	return reg
}

// associationRegistration builds the association joining one subnet to its
// tier's route table, each found through its own role's tag.
func associationRegistration(client ccAPI, typeKey, subnetKey, subnetRole, routeTableKey, routeTableRole string,
	applies []resource.Applicability,
) resource.Registration {
	reg := resource.Registration{
		Provider: Provider, Type: typeKey, Capability: manifest.CapabilityNetwork,
		Applies: applies, Lookup: resource.LookupByName, DependsOn: []string{subnetKey, routeTableKey},
		Resource: &relationshipResource{
			provider: Provider, typeName: TypeSubnetRouteTableAssociation, client: client,
			// The only link type with an opaque id, so it cannot be computed
			// from its endpoints: every association in the region is listed
			// and matched on both of them instead.
			identify: func(ctx context.Context, name string) (string, bool, error) {
				subnetID, found, err := endpointIDForRole(ctx, client, TypeSubnet, subnetRole, name)
				if err != nil || !found {
					return "", false, err
				}
				routeTableID, found, err := endpointIDForRole(ctx, client, TypeRouteTable, routeTableRole, name)
				if err != nil || !found {
					return "", false, err
				}
				return findAssociation(ctx, client, subnetID, routeTableID)
			},
			desired: func(spec resource.Spec) (map[string]any, error) {
				subnetID, err := spec.Attribute(subnetKey, "SubnetId")
				if err != nil {
					return nil, err
				}
				routeTableID, err := spec.Attribute(routeTableKey, "RouteTableId")
				if err != nil {
					return nil, err
				}
				return map[string]any{"SubnetId": subnetID, "RouteTableId": routeTableID}, nil
			},
		},
	}
	if typeKey != TypeSubnetRouteTableAssociation {
		reg.VendorType = TypeSubnetRouteTableAssociation
	}
	return reg
}

// tierSubnetKeys returns the registry keys of a tier's two subnets, for a
// consumer that places something in the tier: the function, the cache, a
// database subnet group.
func tierSubnetKeys(private bool) [2]string {
	if private {
		return [2]string{key(TypePrivateSubnet), key(TypePrivateSubnetB)}
	}
	return [2]string{key(TypeSubnet), key(TypePublicSubnetB)}
}
