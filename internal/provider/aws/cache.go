package aws

import (
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Cloud Control type names for the keyvalue types this package registers.
const (
	TypeElastiCacheServerlessCache = "AWS::ElastiCache::ServerlessCache"
	TypeSecurityGroup              = "AWS::EC2::SecurityGroup"
)

// TypeCacheSecurityGroup is the security group admitting a service's network
// to its cache. A role of AWS::EC2::SecurityGroup rather than the bare type,
// so a security group playing another role can register beside it.
var TypeCacheSecurityGroup = resource.RoleType(TypeSecurityGroup, "Cache")

// DriverRedis is the driver a keyvalue binding declares to ask this provider
// for a Redis-protocol cache: an ElastiCache Serverless cache, reachable
// only from inside the network binding the entry names.
const DriverRedis = "redis"

// Engines a redis binding may ask for, and the major version each is
// created at. Valkey unless the binding says otherwise: it speaks the Redis
// protocol, and it is the engine ElastiCache prices lower and develops.
const (
	engineValkey   = "valkey"
	engineRedisOSS = "redis"
)

var engineMajorVersion = map[string]string{engineValkey: "8", engineRedisOSS: "7"}

// Ports a serverless cache listens on: 6379 for the primary endpoint, 6380
// for the reader endpoint.
const (
	cachePort       = 6379
	cacheReaderPort = 6380
)

// cacheNetwork names the network binding the entry references, which is
// where the cache's subnet and the security group's VPC come from.
func cacheNetwork(spec resource.Spec) (string, error) {
	network, _ := spec.Config["network"].(string)
	if network == "" {
		return "", kerrors.Validation(
			"keyvalue binding %q names no network to place the cache in", spec.Binding)
	}
	return network, nil
}

// cacheEngine reads the binding's engine, valkey when unsaid.
func cacheEngine(spec resource.Spec) (string, error) {
	engine, _ := spec.Config["engine"].(string)
	if engine == "" {
		return engineValkey, nil
	}
	if _, known := engineMajorVersion[engine]; !known {
		return "", kerrors.Validation(
			"keyvalue binding %q asks for engine %q, which this provider does not offer", spec.Binding, engine)
	}
	return engine, nil
}

// registerKeyValue returns the registrations for one Redis-protocol cache: the
// security group admitting the service's network to it, and the serverless
// cache itself, placed in that network's subnet.
//
// Both apply only to a binding declaring driver redis, the one driver this
// capability has on aws today, so a second store can register beside them
// under its own driver.
func registerKeyValue(client ccAPI) []resource.Registration {
	securityGroupKey := key(TypeCacheSecurityGroup)
	redisOnly := []resource.Applicability{bindingDriverIs(DriverRedis)}

	return []resource.Registration{
		{
			Provider: Provider, Type: TypeCacheSecurityGroup, VendorType: TypeSecurityGroup,
			Capability: manifest.CapabilityKeyValue,
			Applies:    redisOnly,
			// The group lives in the referenced network's VPC and admits
			// that VPC's whole address range: anything the service later
			// runs inside the network reaches the cache, nothing outside
			// it does.
			ReadsReferences: []resource.ReferenceRead{{Key: "network", Type: key(TypeVPC)}},
			Lookup:          resource.LookupByTag,
			Resource: translated(taggedLookup(client, TypeSecurityGroup),
				func(spec resource.Spec) (resource.Spec, error) {
					network, err := cacheNetwork(spec)
					if err != nil {
						return spec, err
					}
					vpcID, err := referencedAttribute(spec, network, TypeVPC, "VpcId")
					if err != nil {
						return spec, err
					}
					cidr, err := referencedAttribute(spec, network, TypeVPC, "CidrBlock")
					if err != nil {
						return spec, err
					}
					translated := spec
					translated.Config = map[string]any{
						"GroupName":        spec.Name + "-cache",
						"GroupDescription": "kraai: admits the " + network + " network to the " + spec.Binding + " cache",
						"VpcId":            vpcID,
						"SecurityGroupIngress": []any{map[string]any{
							"IpProtocol": "tcp",
							"FromPort":   cachePort,
							"ToPort":     cacheReaderPort,
							"CidrIp":     cidr,
						}},
					}
					return translated, nil
				},
				nil),
		},
		{
			Provider: Provider, Type: TypeElastiCacheServerlessCache,
			Capability: manifest.CapabilityKeyValue,
			Applies:    redisOnly,
			// Needs its security group's id, and the subnet of the network
			// it is placed in; the network's own ordering (subnet after
			// VPC) is the network binding's business.
			DependsOn:       []string{securityGroupKey},
			ReadsReferences: []resource.ReferenceRead{{Key: "network", Type: key(TypeSubnet)}},
			// ServerlessCacheName is the primary identifier, settable at
			// create and unique per account and region; a derived name is
			// already the lowercase string ElastiCache stores it as.
			Lookup: resource.LookupByName,
			Resource: translated(
				&resourceType{
					provider: Provider, typeName: TypeElastiCacheServerlessCache,
					lookup: resource.LookupByName, client: client,
				},
				func(spec resource.Spec) (resource.Spec, error) {
					network, err := cacheNetwork(spec)
					if err != nil {
						return spec, err
					}
					engine, err := cacheEngine(spec)
					if err != nil {
						return spec, err
					}
					subnetID, err := referencedAttribute(spec, network, TypeSubnet, "SubnetId")
					if err != nil {
						return spec, err
					}
					groupID, err := spec.Attribute(securityGroupKey, "GroupId")
					if err != nil {
						return spec, err
					}
					translated := spec
					translated.Config = map[string]any{
						"ServerlessCacheName": spec.Name,
						"Engine":              engine,
						"MajorEngineVersion":  engineMajorVersion[engine],
						"SubnetIds":           []any{subnetID},
						"SecurityGroupIds":    []any{groupID},
					}
					return translated, nil
				},
				// An engine is not something ElastiCache changes in place;
				// a binding that switches it gets a new cache.
				map[string]func(resource.Spec) (string, error){"Engine": cacheEngine}),
		},
	}
}

// cacheURL builds the connection URL a client opens to the cache from the
// endpoint it published. Always rediss: a serverless cache accepts only
// TLS.
func cacheURL(spec resource.Spec, attributeKey string) (string, error) {
	attrs, ok := spec.Attributes[attributeKey]
	if !ok {
		return "", kerrors.Validation(
			"binding %q: no resource %q has published attributes, so the cache endpoint cannot be resolved",
			spec.Binding, attributeKey)
	}
	endpoint, _ := attrs["Endpoint"].(map[string]any)
	address, _ := endpoint["Address"].(string)
	port, _ := endpoint["Port"].(string)
	if address == "" || port == "" {
		return "", kerrors.Validation(
			"binding %q: resource %q published no Endpoint address and port, got %v",
			spec.Binding, attributeKey, attrs["Endpoint"])
	}
	return "rediss://" + address + ":" + port, nil
}
