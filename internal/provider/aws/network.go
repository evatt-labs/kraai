package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// CloudFormation type names for the network types this package registers.
const (
	TypeVPC                         = "AWS::EC2::VPC"
	TypeInternetGateway             = "AWS::EC2::InternetGateway"
	TypeVPCGatewayAttachment        = "AWS::EC2::VPCGatewayAttachment"
	TypeSubnet                      = "AWS::EC2::Subnet"
	TypeRouteTable                  = "AWS::EC2::RouteTable"
	TypeRoute                       = "AWS::EC2::Route"
	TypeSubnetRouteTableAssociation = "AWS::EC2::SubnetRouteTableAssociation"
)

// defaultRoute is the destination a public subnet's route table sends
// everything it has no more specific route for.
const defaultRoute = "0.0.0.0/0"

// TypeVPCEndpoint is AWS::EC2::VPCEndpoint's Cloud Control TypeName. Two
// roles of it register under a network: gateway endpoints for S3 and
// DynamoDB, which route those services' traffic inside the VPC.
const TypeVPCEndpoint = "AWS::EC2::VPCEndpoint"

// The registry keys for the two gateway endpoints every network gets. Both
// are free, and without them a function inside the network reaches neither
// its bucket nor its table: a Lambda network interface has no public IP, so
// a public AWS endpoint is unreachable from a subnet with no NAT.
var (
	TypeS3Endpoint       = resource.RoleType(TypeVPCEndpoint, "S3")
	TypeDynamoDBEndpoint = resource.RoleType(TypeVPCEndpoint, "DynamoDB")
)

// gatewayServiceName is the endpoint service a gateway endpoint attaches
// to, spelled the way EC2 names them per region.
func gatewayServiceName(region, service string) string {
	return "com.amazonaws." + region + "." + service
}

// attachmentTypeIGW is the first half of a VPCGatewayAttachment's identifier.
// Cloud Control spells it "IGW", not the "internet-gateway" the EC2 API uses
// elsewhere, and an identifier built with the wrong spelling reads as an
// absent resource rather than an error.
const attachmentTypeIGW = "IGW"

// translatedResource adapts a spec written in manifest vocabulary into the
// CloudFormation properties a type actually takes, then defers to the
// generic engine.
//
// A shared wrapper rather than one hand-written type per resource: the
// network types differ only in which properties they build, and every other
// verb is identical.
type translatedResource struct {
	*resourceType
	// immutable lists the manifest-declared properties whose change forces
	// a replacement, for Diff to compare. Sibling identifiers
	// are deliberately absent from it — see that method.
	immutable map[string]func(spec resource.Spec) (string, error)
}

// translated wires translate into engine's own hook and wraps it with the
// immutable comparison Diff below; immutable may be nil for a type nothing
// compares.
func translated(engine *resourceType, translate func(spec resource.Spec) (resource.Spec, error),
	immutable map[string]func(spec resource.Spec) (string, error),
) *translatedResource {
	engine.translate = func(_ context.Context, spec resource.Spec) (resource.Spec, error) { return translate(spec) }
	return &translatedResource{resourceType: engine, immutable: immutable}
}

// Diff compares only the properties the manifest declares.
//
// It cannot defer to the generic engine here, because that would need the
// full translated config and translation needs sibling identifiers that a
// plan has no way to resolve — plan runs before anything is applied, so
// Spec.Attributes is empty by construction. Comparing them would also be
// wrong: a VpcId is a consequence of applying the manifest, not something
// the manifest asked for, so it is not a field an author can change and not
// a difference that should force a replacement.
//
// Implemented rather than omitted because it is an optional interface: a
// type that does not implement it reports every changed spec as no-change,
// which would silently ignore a CIDR being edited.
func (t *translatedResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	if state == nil {
		return resource.Same, nil
	}
	// Every property this type compares is one it declared immutable, so a
	// difference is Immutable by construction; the mutable answer is not
	// something this comparison can produce.
	for property, want := range t.immutable {
		desired, err := want(spec)
		if err != nil {
			return resource.Same, err
		}
		if current, ok := state.Attributes[property].(string); ok && current != desired {
			return resource.Immutable, nil
		}
	}
	return resource.Same, nil
}

// relationshipResource is a link between two other resources, with no
// identity of its own.
//
// A route, a gateway attachment and a subnet-to-route-table association are
// not things kraai can name. None is taggable, and each is identified either
// by an identifier built from the resources it joins or by an opaque id AWS
// assigns. So identity here is resolved through the endpoints: find those by
// the identity tag they do carry, then compute or match this link from them.
//
// That is deliberately not how Create gets the same values. Create reads
// them from Spec.Attributes, which the applier populates from the
// dependencies this registration declares — ordered, explicit, and free of
// extra API calls. Get has no spec to read, because a plan runs before
// anything is applied, which is why the endpoint lookup exists at all.
type relationshipResource struct {
	provider string
	typeName string
	client   ccAPI

	// identify returns this link's Cloud Control identifier, or found=false
	// when an endpoint it depends on does not exist yet — which is also the
	// answer for the link itself.
	identify func(ctx context.Context, name string) (identifier string, found bool, err error)
	// desired builds the create payload from the identifiers the applier
	// supplied in spec.Attributes.
	desired func(spec resource.Spec) (map[string]any, error)
}

func (r *relationshipResource) ref(name string) resource.Ref {
	return resource.Ref{Provider: r.provider, Type: r.typeName, Name: name}
}

func (r *relationshipResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	identifier, found, err := r.identify(ctx, ref.Name)
	if err != nil || !found {
		return nil, err
	}
	properties, found, err := r.client.GetResource(ctx, r.typeName, identifier)
	if err != nil || !found {
		return nil, err
	}
	return &resource.State{Ref: ref, ID: identifier, Attributes: properties}, nil
}

func (r *relationshipResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	desired, err := r.desired(spec)
	if err != nil {
		return nil, err
	}
	identifier, properties, err := r.client.CreateResource(ctx, r.typeName, desired)
	if err != nil {
		return nil, err
	}
	if len(properties) == 0 && identifier != "" {
		if fetched, found, getErr := r.client.GetResource(ctx, r.typeName, identifier); getErr == nil && found {
			properties = fetched
		}
	}
	return &resource.State{Ref: r.ref(spec.Name), ID: identifier, Attributes: properties}, nil
}

// Update always refuses. Every property of a link is part of what it links,
// so changing one produces a different link rather than a modified one —
// which is a replacement, and apply's job rather than this method's.
func (r *relationshipResource) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	return nil, resource.ErrImmutable
}

func (r *relationshipResource) Delete(ctx context.Context, ref resource.Ref) error {
	identifier, found, err := r.identify(ctx, ref.Name)
	if err != nil {
		return err
	}
	if !found {
		// An endpoint is already gone, so the link is too. Teardown treats
		// deleting something absent as success.
		return nil
	}
	return r.client.DeleteResource(ctx, r.typeName, identifier)
}

// taggedLookup builds the byTag engine for one of the network types that
// does carry kraai's identity tag, used both as a registration's own
// Resource and as the way a relationshipResource finds its endpoints.
func taggedLookup(client ccAPI, typeName string) *resourceType {
	return &resourceType{
		provider: Provider,
		typeName: typeName,
		lookup:   resource.LookupByTag,
		match:    arrayTagsMatch,
		stampTag: arrayTagsStampTag,
		client:   client,
	}
}

// roleTaggedLookup is taggedLookup for a type that registers under more
// than one role in the same binding, two gateway endpoints say. Every
// instance in the binding shares the derived name, so the tag value carries
// the role too, or a lookup for one role would find whichever instance was
// listed first.
func roleTaggedLookup(client ccAPI, typeName, role string) *resourceType {
	value := func(name string) string { return name + "/" + role }
	return &resourceType{
		provider: Provider,
		typeName: typeName,
		lookup:   resource.LookupByTag,
		match:    func(properties map[string]any, name string) bool { return arrayTagsMatch(properties, value(name)) },
		stampTag: func(desired map[string]any, name string) { arrayTagsStampTag(desired, value(name)) },
		client:   client,
	}
}

// endpointID resolves the provider id of the typeName instance carrying the
// identity tag for name.
//
// Looks up by kraai's own derived name, with no import: this resolves a
// *sibling* resource in the same binding — the VPC a subnet attaches to —
// which kraai created and named itself.
//
// The gap that leaves: if that sibling was itself adopted, it carries the
// manifest's identity rather than a derived name and this lookup will not
// find it. Importing a whole network is therefore not yet supported, which is
// the same shape as evatt-labs/kraai#197 — one resource needing another's
// identity across a boundary this function cannot see.
func endpointID(ctx context.Context, client ccAPI, typeName, name string) (string, bool, error) {
	id, _, found, err := taggedLookup(client, typeName).resolve(ctx, resource.Ref{Name: name})
	return id, found, err
}

// tagOnly is the translate step for a type whose whole desired state is its
// identity tag, which the engine stamps on for it.
func tagOnly(spec resource.Spec) (resource.Spec, error) {
	translated := spec
	translated.Config = map[string]any{}
	return translated, nil
}

// configString reads a required string out of a spec's manifest-supplied
// config.
func configString(spec resource.Spec, field string) (string, error) {
	value, ok := spec.Config[field].(string)
	if !ok || value == "" {
		return "", kerrors.Validation(
			"network binding %q declares no %q", spec.Binding, field)
	}
	return value, nil
}

// registerNetwork returns the registrations for one private network: a VPC,
// a public subnet, the routing that makes the subnet reach the internet, and
// gateway endpoints routing S3 and DynamoDB traffic inside the VPC. region
// names the endpoint services, which EC2 spells per region.
//
// Every one of them belongs to a single manifest binding, so they share a
// derived name and are ordered against each other purely by DependsOn.
func registerNetwork(client ccAPI, region string) []resource.Registration {
	vpcKey := key(TypeVPC)
	igwKey := key(TypeInternetGateway)
	subnetKey := key(TypeSubnet)
	routeTableKey := key(TypeRouteTable)
	attachmentKey := key(TypeVPCGatewayAttachment)

	// gatewayEndpoint registers one gateway endpoint on the network's route
	// table. Found by tag, since an endpoint's id is EC2's and nothing the
	// manifest says names it, with the service in the tag value so the two
	// endpoints in one binding stay distinguishable (roleTaggedLookup).
	gatewayEndpoint := func(typeKey, service string) resource.Registration {
		return resource.Registration{
			Provider: Provider, Type: typeKey, VendorType: TypeVPCEndpoint,
			Capability: manifest.CapabilityNetwork,
			Lookup:     resource.LookupByTag, DependsOn: []string{vpcKey, routeTableKey},
			Resource: translated(roleTaggedLookup(client, TypeVPCEndpoint, service),
				func(spec resource.Spec) (resource.Spec, error) {
					vpcID, err := spec.Attribute(vpcKey, "VpcId")
					if err != nil {
						return spec, err
					}
					routeTableID, err := spec.Attribute(routeTableKey, "RouteTableId")
					if err != nil {
						return spec, err
					}
					translated := spec
					translated.Config = map[string]any{
						"VpcId":           vpcID,
						"ServiceName":     gatewayServiceName(region, service),
						"VpcEndpointType": "Gateway",
						"RouteTableIds":   []any{routeTableID},
					}
					return translated, nil
				},
				nil),
		}
	}

	return []resource.Registration{
		gatewayEndpoint(TypeS3Endpoint, "s3"),
		gatewayEndpoint(TypeDynamoDBEndpoint, "dynamodb"),
		{
			Provider: Provider, Type: TypeVPC, Capability: manifest.CapabilityNetwork,
			Lookup: resource.LookupByTag,
			Resource: translated(taggedLookup(client, TypeVPC),
				func(spec resource.Spec) (resource.Spec, error) {
					cidr, err := configString(spec, "cidr")
					if err != nil {
						return spec, err
					}
					translated := spec
					translated.Config = map[string]any{
						"CidrBlock": cidr,
						// Both on, so a private hosted zone associated with
						// this VPC actually resolves inside it. Off is the
						// EC2 default and the usual cause of a database
						// name that resolves everywhere except where it is
						// needed.
						"EnableDnsSupport":   true,
						"EnableDnsHostnames": true,
					}
					return translated, nil
				},
				map[string]func(resource.Spec) (string, error){
					"CidrBlock": func(spec resource.Spec) (string, error) {
						return configString(spec, "cidr")
					},
				}),
		},
		{
			Provider: Provider, Type: TypeInternetGateway, Capability: manifest.CapabilityNetwork,
			Lookup: resource.LookupByTag,
			Resource: translated(taggedLookup(client, TypeInternetGateway),
				tagOnly,
				nil),
		},
		{
			Provider: Provider, Type: TypeSubnet, Capability: manifest.CapabilityNetwork,
			Lookup: resource.LookupByTag, DependsOn: []string{vpcKey},
			Resource: translated(taggedLookup(client, TypeSubnet),
				func(spec resource.Spec) (resource.Spec, error) {
					cidr, err := configString(spec, "subnet")
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
						"MapPublicIpOnLaunch": true,
					}
					return translated, nil
				},
				map[string]func(resource.Spec) (string, error){
					"CidrBlock": func(spec resource.Spec) (string, error) {
						return configString(spec, "subnet")
					},
				}),
		},
		{
			Provider: Provider, Type: TypeRouteTable, Capability: manifest.CapabilityNetwork,
			Lookup: resource.LookupByTag, DependsOn: []string{vpcKey},
			Resource: translated(taggedLookup(client, TypeRouteTable),
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
			Provider: Provider, Type: TypeVPCGatewayAttachment, Capability: manifest.CapabilityNetwork,
			Lookup: resource.LookupByName, DependsOn: []string{vpcKey, igwKey},
			Resource: &relationshipResource{
				provider: Provider, typeName: TypeVPCGatewayAttachment, client: client,
				identify: func(ctx context.Context, name string) (string, bool, error) {
					vpcID, found, err := endpointID(ctx, client, TypeVPC, name)
					if err != nil || !found {
						return "", false, err
					}
					return attachmentTypeIGW + "|" + vpcID, true, nil
				},
				desired: func(spec resource.Spec) (map[string]any, error) {
					vpcID, err := spec.Attribute(vpcKey, "VpcId")
					if err != nil {
						return nil, err
					}
					igwID, err := spec.Attribute(igwKey, "InternetGatewayId")
					if err != nil {
						return nil, err
					}
					return map[string]any{"VpcId": vpcID, "InternetGatewayId": igwID}, nil
				},
			},
		},
		{
			Provider: Provider, Type: TypeRoute, Capability: manifest.CapabilityNetwork,
			Lookup: resource.LookupByName,
			// The attachment, not just the gateway: EC2 refuses a route to
			// an internet gateway that is not attached to the route table's
			// own VPC.
			DependsOn: []string{routeTableKey, attachmentKey},
			Resource: &relationshipResource{
				provider: Provider, typeName: TypeRoute, client: client,
				identify: func(ctx context.Context, name string) (string, bool, error) {
					routeTableID, found, err := endpointID(ctx, client, TypeRouteTable, name)
					if err != nil || !found {
						return "", false, err
					}
					return routeTableID + "|" + defaultRoute, true, nil
				},
				desired: func(spec resource.Spec) (map[string]any, error) {
					routeTableID, err := spec.Attribute(routeTableKey, "RouteTableId")
					if err != nil {
						return nil, err
					}
					igwID, err := spec.Attribute(attachmentKey, "InternetGatewayId")
					if err != nil {
						return nil, err
					}
					return map[string]any{
						"RouteTableId":         routeTableID,
						"DestinationCidrBlock": defaultRoute,
						"GatewayId":            igwID,
					}, nil
				},
			},
		},
		{
			Provider: Provider, Type: TypeSubnetRouteTableAssociation, Capability: manifest.CapabilityNetwork,
			Lookup: resource.LookupByName, DependsOn: []string{subnetKey, routeTableKey},
			Resource: &relationshipResource{
				provider: Provider, typeName: TypeSubnetRouteTableAssociation, client: client,
				// The only one of the three with an opaque id, so it cannot
				// be computed from its endpoints — every association in the
				// region is listed and matched on both of them instead.
				identify: func(ctx context.Context, name string) (string, bool, error) {
					subnetID, found, err := endpointID(ctx, client, TypeSubnet, name)
					if err != nil || !found {
						return "", false, err
					}
					routeTableID, found, err := endpointID(ctx, client, TypeRouteTable, name)
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
		},
	}
}

// findAssociation returns the association joining subnetID to routeTableID.
//
// Cloud Control lists these unscoped, so this reads every association in the
// region. Acceptable because an association carries nothing else to match on
// and the set is small; a region with enough of them for this to hurt would
// need the EC2 API's own filtered describe instead.
func findAssociation(ctx context.Context, client ccAPI, subnetID, routeTableID string) (string, bool, error) {
	identifiers, err := client.ListResources(ctx, TypeSubnetRouteTableAssociation, nil)
	if err != nil {
		return "", false, err
	}
	for _, identifier := range identifiers {
		properties, found, err := client.GetResource(ctx, TypeSubnetRouteTableAssociation, identifier)
		// A candidate that cannot be read is not the association being
		// looked for. Every VPC's main route table is listed here as an
		// association with no subnet, and reading one fails outright with
		// "does not belong to a subnet" — so an unreadable entry is the
		// normal case to filter out, not a reason to abandon the search.
		if err != nil || !found {
			continue
		}
		if properties["SubnetId"] == subnetID && properties["RouteTableId"] == routeTableID {
			return identifier, true, nil
		}
	}
	return "", false, nil
}
