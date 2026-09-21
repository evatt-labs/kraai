package aws

import (
	"errors"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// constructedIdentity lists the registrations declared byName whose
// identifier kraai builds itself rather than reads from the schema, with
// what the schema derives instead. Each is a per-type trick the generic
// layer will have to carry as a declared attribute or drop.
var constructedIdentity = map[string]cfschema.Identity{
	// Identifier is RouteTableId|CidrBlock; kraai joins it from the table
	// it looked up and the fixed default route. CidrBlock is read-only.
	TypeRoute:        cfschema.IdentityByAttr,
	TypePrivateRoute: cfschema.IdentityByAttr,
	// Identifier is a provider-assigned Id; kraai lists every association
	// and matches SubnetId and RouteTableId.
	TypeSubnetRouteTableAssociation:         cfschema.IdentityByAttr,
	TypePublicSubnetBRouteTableAssociation:  cfschema.IdentityByAttr,
	TypePrivateSubnetRouteTableAssociation:  cfschema.IdentityByAttr,
	TypePrivateSubnetBRouteTableAssociation: cfschema.IdentityByAttr,
	// Identifier is AttachmentType|VpcId; kraai lists and matches VpcId.
	TypeVPCGatewayAttachment: cfschema.IdentityByAttr,
	// The schema's identifier is the rule's Arn, read-only; kraai passes
	// the rule Name as the identifier and the handler accepts it.
	TypeEventsRule: cfschema.IdentityByTag,
}

// Every registered vendor type is in the schema index, and the index's
// derived identity agrees with the strategy the registration declares by
// hand, except where constructedIdentity records why not. byAttr and byApi
// registrations match on an attribute the schema cannot know about, so for
// those the index only has to offer a list handler.
func TestRegistrationsAgreeWithSchemaIndex(t *testing.T) {
	for _, reg := range Registrations(nil) {
		vendorType := reg.VendorType
		if vendorType == "" {
			vendorType = reg.Type
		}
		facts, err := cfschema.Lookup(vendorType)
		if errors.Is(err, cfschema.ErrUnknownType) {
			t.Errorf("%s: vendor type %s is not in the schema index", reg.Type, vendorType)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if want, ok := constructedIdentity[reg.Type]; ok {
			if facts.Identity != want {
				t.Errorf("%s: schema derives %s, expected %s", reg.Type, facts.Identity, want)
			}
			continue
		}
		switch reg.Lookup {
		case resource.LookupByName:
			if facts.Identity != cfschema.IdentityByName {
				t.Errorf("%s: registered byName, schema derives %s", reg.Type, facts.Identity)
			}
		case resource.LookupByTag:
			if facts.Identity != cfschema.IdentityByTag {
				t.Errorf("%s: registered byTag, schema derives %s", reg.Type, facts.Identity)
			}
		case resource.LookupByAttr, resource.LookupByAPI:
			if facts.Identity == cfschema.IdentityNone {
				t.Errorf("%s: registered %s, schema has no list handler", reg.Type, reg.Lookup)
			}
		}
	}
}
