package aws

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// keyValueResource returns the registered Resource for one keyvalue type.
func keyValueResource(t *testing.T, client ccAPI, typeName string) resource.Resource {
	t.Helper()
	for _, r := range registerKeyValue(client) {
		if r.Type == typeName {
			return r.Resource
		}
	}
	t.Fatalf("no keyvalue registration for %s", typeName)
	return nil
}

func cacheSpec(config map[string]any, attrs map[string]map[string]any) resource.Spec {
	base := map[string]any{"driver": DriverRedis, "network": "NET"}
	maps.Copy(base, config)
	return resource.Spec{Binding: "CACHE", Name: "env-svc-cache", Config: base, Attributes: attrs}
}

// The group lives in the referenced network's VPC and admits its whole
// address range on the cache's two ports, both read from what the VPC
// published under the network binding's name.
func TestCacheSecurityGroupAdmitsTheReferencedNetwork(t *testing.T) {
	fc := &fakeClient{createID: "sg-1", createProps: map[string]any{"GroupId": "sg-1"}}
	res := keyValueResource(t, fc, TypeCacheSecurityGroup)

	spec := cacheSpec(nil, map[string]map[string]any{
		"NET." + key(TypeVPC): {"VpcId": "vpc-abc", "CidrBlock": "10.90.0.0/16"},
	})
	if _, err := res.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := fc.createCalls[0]
	if desired["VpcId"] != "vpc-abc" {
		t.Fatalf("VpcId = %v, want the referenced VPC's id", desired["VpcId"])
	}
	ingress, _ := desired["SecurityGroupIngress"].([]any)
	if len(ingress) != 1 {
		t.Fatalf("SecurityGroupIngress = %v, want one rule", desired["SecurityGroupIngress"])
	}
	rule := ingress[0].(map[string]any)
	if rule["CidrIp"] != "10.90.0.0/16" || rule["FromPort"] != cachePort || rule["ToPort"] != cacheReaderPort || rule["IpProtocol"] != "tcp" {
		t.Fatalf("ingress rule = %v, want tcp %d-%d from the VPC's block", rule, cachePort, cacheReaderPort)
	}
	if _, tagged := desired["Tags"]; !tagged {
		t.Fatal("the group carries no identity tag, so it could never be found again")
	}
}

func TestCacheSecurityGroupFailsLoudlyWithoutTheVPC(t *testing.T) {
	res := keyValueResource(t, &fakeClient{}, TypeCacheSecurityGroup)
	_, err := res.Create(context.Background(), cacheSpec(nil, nil))
	if err == nil || !strings.Contains(err.Error(), key(TypeVPC)) {
		t.Fatalf("Create without the VPC's attributes: err = %v, want one naming the VPC", err)
	}
}

// The cache is placed in the referenced network's subnet behind its own
// security group, on Valkey unless the binding says Redis OSS.
func TestServerlessCacheCreateUsesItsGroupAndTheNetworkSubnet(t *testing.T) {
	fc := &fakeClient{createID: "env-svc-cache", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/ServerlessCacheName"}}}
	res := keyValueResource(t, fc, TypeElastiCacheServerlessCache)

	attrs := map[string]map[string]any{
		"NET." + key(TypeSubnet):        {"SubnetId": "subnet-1"},
		"NET." + key(TypePublicSubnetB): {"SubnetId": "subnet-2"},
		key(TypeCacheSecurityGroup):     {"GroupId": "sg-1"},
	}
	if _, err := res.Create(context.Background(), cacheSpec(nil, attrs)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := fc.createCalls[0]
	if desired["ServerlessCacheName"] != "env-svc-cache" || desired["Engine"] != engineValkey || desired["MajorEngineVersion"] != "8" {
		t.Fatalf("desired = %v, want the derived name on valkey 8", desired)
	}
	if subnets, _ := desired["SubnetIds"].([]any); len(subnets) != 2 || subnets[0] != "subnet-1" || subnets[1] != "subnet-2" {
		t.Fatalf("SubnetIds = %v, want both of the network's public subnets", desired["SubnetIds"])
	}
	if groups, _ := desired["SecurityGroupIds"].([]any); len(groups) != 1 || groups[0] != "sg-1" {
		t.Fatalf("SecurityGroupIds = %v, want the cache's own group", desired["SecurityGroupIds"])
	}

	fc.createCalls = nil
	if _, err := res.Create(context.Background(), cacheSpec(map[string]any{"engine": engineRedisOSS}, attrs)); err != nil {
		t.Fatalf("Create(redis): %v", err)
	}
	if fc.createCalls[0]["Engine"] != engineRedisOSS || fc.createCalls[0]["MajorEngineVersion"] != "7" {
		t.Fatalf("desired = %v, want redis 7 when the binding asks for it", fc.createCalls[0])
	}
}

func TestServerlessCacheRejectsAnEngineItDoesNotOffer(t *testing.T) {
	res := keyValueResource(t, &fakeClient{}, TypeElastiCacheServerlessCache)
	_, err := res.Create(context.Background(), cacheSpec(map[string]any{"engine": "memcached"}, nil))
	if err == nil || !strings.Contains(err.Error(), "memcached") {
		t.Fatalf("Create(memcached): err = %v, want one naming the engine", err)
	}
}

// Plan compares a cache with no attributes to hand, so Diff compares only
// what the manifest declares: the engine, which ElastiCache cannot change in
// place.
func TestServerlessCacheDiffComparesTheEngineWithoutAttributes(t *testing.T) {
	res := keyValueResource(t, &fakeClient{}, TypeElastiCacheServerlessCache)
	differ, ok := res.(interface {
		Diff(resource.Spec, *resource.State) (resource.Difference, error)
	})
	if !ok {
		t.Fatal("the cache resource no longer implements Diff, so a switched engine would read as no-change")
	}
	live := &resource.State{Attributes: map[string]any{"Engine": engineValkey}}
	if d, err := differ.Diff(cacheSpec(nil, nil), live); err != nil || d != resource.Same {
		t.Fatalf("Diff(same engine) = %v, %v; want Same", d, err)
	}
	if d, err := differ.Diff(cacheSpec(map[string]any{"engine": engineRedisOSS}, nil), live); err != nil || d != resource.Immutable {
		t.Fatalf("Diff(other engine) = %v, %v; want Immutable", d, err)
	}
}

func TestCacheURLIsTLSFromThePublishedEndpoint(t *testing.T) {
	attrKey := "CACHE." + key(TypeElastiCacheServerlessCache)
	spec := resource.Spec{Binding: "api", Attributes: map[string]map[string]any{
		attrKey: {"Endpoint": map[string]any{"Address": "cache.example.cache.amazonaws.com", "Port": "6379"}},
	}}
	url, err := cacheURL(spec, attrKey)
	if err != nil || url != "rediss://cache.example.cache.amazonaws.com:6379" {
		t.Fatalf("cacheURL = %q, %v", url, err)
	}

	spec.Attributes[attrKey] = map[string]any{"Endpoint": map[string]any{"Address": "only"}}
	if _, err := cacheURL(spec, attrKey); err == nil || !strings.Contains(err.Error(), "Endpoint") {
		t.Fatalf("cacheURL(no port): err = %v, want one naming the endpoint", err)
	}
	if _, err := cacheURL(spec, "CACHE.missing"); err == nil {
		t.Fatal("cacheURL(unpublished) succeeded, want an error")
	}
}

// Both keyvalue types apply only to a binding asking for redis, and both
// read the network binding the entry names.
func TestKeyValueRegistrationsApplyToRedisAndReadTheNetwork(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg, &Client{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	vendors := map[string]string{manifest.CapabilityKeyValue: Provider}
	regs, err := reg.Resolve(manifest.CapabilityKeyValue, resource.ApplicabilityContext{
		Vendors: vendors, Binding: map[string]any{"driver": DriverRedis, "network": "NET"},
	})
	if err != nil || len(regs) != 2 {
		t.Fatalf("Resolve(driver redis) = %v, %v; want the group and the cache", regs, err)
	}
	for _, r := range regs {
		if len(r.ReadsReferences) == 0 {
			t.Errorf("%s reads nothing, want the network reference", r.Type)
		}
		for _, read := range r.ReadsReferences {
			if read.Key != "network" {
				t.Errorf("%s reads %v, want only the network reference", r.Type, r.ReadsReferences)
			}
		}
	}
	if _, err := reg.Resolve(manifest.CapabilityKeyValue, resource.ApplicabilityContext{
		Vendors: vendors, Binding: map[string]any{"driver": "memcached", "network": "NET"},
	}); err == nil {
		t.Fatal("Resolve(driver memcached) succeeded, want an error: no aws type speaks it")
	}
}
