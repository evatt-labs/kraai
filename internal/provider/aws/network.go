package aws

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
	"github.com/evatt-labs/kraai/internal/resource"
)

// A network binding names a VPC Terraform owns, and the subnets in it the
// service's resources are placed in: kraai creates neither. What it owns is
// one security group in that VPC per binding, which a function inside the
// network attaches and which the databases and caches in it admit, so a
// store is reachable from the service's own functions and nothing else in
// the VPC. The group allows every outbound connection; what a function
// reaches beyond the VPC, an S3 or DynamoDB gateway endpoint, a NAT
// gateway, is the VPC's own configuration.

// TypeNetworkSecurityGroup is the one resource a network binding owns.
var TypeNetworkSecurityGroup = resource.RoleType(TypeSecurityGroup, "Network")

var (
	vpcIDPattern    = regexp.MustCompile(`^vpc-[0-9a-f]+$`)
	subnetIDPattern = regexp.MustCompile(`^subnet-[0-9a-f]+$`)
)

// networkConfig is what a network binding entry declares.
type networkConfig struct {
	vpcID     string
	subnetIDs []any
}

// networkOf reads a network binding entry's VPC and subnets. At least two
// subnets, since a database subnet group spans two availability zones;
// that they are in two zones, and in the VPC, is EC2's and RDS's to check.
func networkOf(binding string, config map[string]any) (networkConfig, error) {
	vpcID, _ := config["vpcId"].(string)
	if !vpcIDPattern.MatchString(vpcID) {
		return networkConfig{}, kerrors.Validation(
			"network binding %q: vpcId must be a VPC id such as vpc-0abc, got %q", binding, vpcID)
	}
	raw, _ := config["subnetIds"].([]any)
	if len(raw) < 2 {
		return networkConfig{}, kerrors.Validation(
			"network binding %q: subnetIds must name at least two subnets, one per availability zone, got %d", binding, len(raw))
	}
	for i, id := range raw {
		if s, _ := id.(string); !subnetIDPattern.MatchString(s) {
			return networkConfig{}, kerrors.Validation(
				"network binding %q: subnetIds[%d] must be a subnet id such as subnet-0abc, got %v", binding, i, id)
		}
	}
	return networkConfig{vpcID: vpcID, subnetIDs: raw}, nil
}

// referencedNetwork reads the network binding an entry's network key
// names, from its declared config.
func referencedNetwork(spec resource.Spec, what string) (string, networkConfig, error) {
	name, _ := spec.Config["network"].(string)
	if name == "" {
		return "", networkConfig{}, kerrors.Validation("%s binding %q names no network to place it in", what, spec.Binding)
	}
	config, ok := spec.Referenced["network"]
	if !ok {
		return "", networkConfig{}, kerrors.Validation(
			"%s binding %q: network %q is not a network binding on this service", what, spec.Binding, name)
	}
	network, err := networkOf(name, config)
	return name, network, err
}

// networkVPC is a network binding's VPC id, which the group's VpcId must
// match: a binding naming another VPC gets a new group.
func networkVPC(spec resource.Spec) (string, error) {
	network, err := networkOf(spec.Binding, spec.Config)
	return network.vpcID, err
}

// registerNetwork returns the registration for a network binding's
// security group, whose delete clears Lambda's interfaces through
// interfaces.
func registerNetwork(client ccAPI, interfaces ec2API) []resource.Registration {
	return []resource.Registration{{
		Provider: Provider, Type: TypeNetworkSecurityGroup, VendorType: TypeSecurityGroup,
		Capability: manifest.CapabilityNetwork,
		Lookup:     resource.LookupByTag,
		Resource: &networkGroupResource{
			releaseWait: networkReleaseWait, releaseTimeout: networkReleaseTimeout, interfaces: interfaces,
			translatedResource: translated(taggedLookup(client, TypeSecurityGroup),
				func(spec resource.Spec) (resource.Spec, error) {
					vpcID, err := networkVPC(spec)
					if err != nil {
						return spec, err
					}
					translated := spec
					translated.Config = map[string]any{
						"GroupName":        spec.Name + "-network",
						"GroupDescription": "kraai: the members of the " + spec.Binding + " network",
						"VpcId":            vpcID,
					}
					return translated, nil
				},
				map[string]func(resource.Spec) (string, error){"VpcId": networkVPC}),
		},
	}}
}

// How long a network's group waits for Lambda to let go of it. A function's
// network interfaces outlive the function, attached for up to about twenty
// minutes and then detached but never deleted, and while one is in the
// group EC2 refuses to delete it.
const (
	networkReleaseWait    = 30 * time.Second
	networkReleaseTimeout = 45 * time.Minute
)

// networkGroupResource is the network's group, whose delete clears the
// interfaces of the functions destroyed before it.
type networkGroupResource struct {
	*translatedResource
	releaseWait, releaseTimeout time.Duration
	interfaces                  ec2API
}

// lambdaInterfacePrefix begins the description Lambda gives each network
// interface it creates for a function, followed by the function's name.
const lambdaInterfacePrefix = "AWS Lambda VPC ENI-"

// Delete retries while the group is held by a network interface, up to
// releaseTimeout, and fails on anything else at once. Each time, it first
// deletes the interfaces Lambda has detached in the group, which nothing
// else ever removes; one still attached is waited for. A group held by no
// interface at all is held by another group's rule, which no wait frees.
func (n *networkGroupResource) Delete(ctx context.Context, ref resource.Ref) error {
	deadline := time.Now().Add(n.releaseTimeout)
	for {
		err := n.translatedResource.Delete(ctx, ref)
		if err == nil || !heldByAnInterface(err) {
			return err
		}
		remaining, known, rerr := n.releaseLambdaInterfaces(ctx, ref)
		if rerr != nil {
			return rerr
		}
		if known && remaining == 0 {
			return err
		}
		if time.Now().After(deadline) {
			return kerrors.Wrap(err, kerrors.CodeUnexpected,
				"the %s network's security group is still in use after %s; the next destroy retries it", ref.Name, n.releaseTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(n.releaseWait):
		}
	}
}

// heldByAnInterface reports whether a delete was refused because a network
// interface still uses the group.
func heldByAnInterface(err error) bool {
	var apiErr *direct.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == "DependencyViolation"
	}
	return strings.Contains(err.Error(), "DependencyViolation")
}

// admitting is the ingress rule letting a network's members reach a store
// on its ports.
func admitting(spec resource.Spec, network string, fromPort, toPort int) (map[string]any, error) {
	groupID, err := referencedAttribute(spec, network, TypeNetworkSecurityGroup, "GroupId")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"IpProtocol":            "tcp",
		"FromPort":              fromPort,
		"ToPort":                toPort,
		"SourceSecurityGroupId": groupID,
	}, nil
}

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

// taggedLookup builds the byTag engine for a type found by kraai's identity
// tag in its Tags array.
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

// releaseLambdaInterfaces deletes the network interfaces Lambda left
// detached in the group: available, carrying Lambda's description. The
// group is kraai's own, found by its identity tag and made for this
// binding, so any function interface in it was made for one of this
// environment's functions; and Lambda leaves one available only when no
// function uses it, recreating one on demand, so deleting it costs nothing.
// It returns how many interfaces were in the group, deleted ones included,
// and whether it could ask at all.
func (n *networkGroupResource) releaseLambdaInterfaces(ctx context.Context, ref resource.Ref) (int, bool, error) {
	if n.interfaces == nil {
		return 0, false, nil
	}
	state, err := n.Get(ctx, ref)
	if err != nil || state == nil {
		return 0, false, err
	}
	groupID, _ := state.Attributes["GroupId"].(string)
	if groupID == "" {
		groupID = state.ID
	}
	found := 0
	pages := ec2.NewDescribeNetworkInterfacesPaginator(n.interfaces, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{{Name: aws.String("group-id"), Values: []string{groupID}}},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return 0, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "listing the network interfaces in %s", groupID)
		}
		for _, eni := range page.NetworkInterfaces {
			found++
			if eni.Status != ec2types.NetworkInterfaceStatusAvailable || !strings.HasPrefix(aws.ToString(eni.Description), lambdaInterfacePrefix) {
				continue
			}
			_, err := n.interfaces.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: eni.NetworkInterfaceId})
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidNetworkInterfaceID.NotFound" {
				continue
			}
			if err != nil {
				return 0, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting %s, a network interface Lambda left in %s", aws.ToString(eni.NetworkInterfaceId), groupID)
			}
		}
	}
	return found, true, nil
}
