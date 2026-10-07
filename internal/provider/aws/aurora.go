package aws

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Cloud Control type names for the Aurora types this package registers.
const (
	TypeRDSDBCluster     = "AWS::RDS::DBCluster"
	TypeRDSDBInstance    = "AWS::RDS::DBInstance"
	TypeRDSDBSubnetGroup = "AWS::RDS::DBSubnetGroup"
)

// TypeDatabaseSecurityGroup is the security group admitting a service's
// network to its Aurora cluster: a role of AWS::EC2::SecurityGroup beside
// the cache's.
var TypeDatabaseSecurityGroup = resource.RoleType(TypeSecurityGroup, "Database")

// engineAurora is the engine a postgres binding names to ask for Aurora
// Serverless v2: real PostgreSQL with extensions and procedural languages,
// inside the network binding the entry names, scaling to zero when idle.
// The alternative to DSQL when an application needs what DSQL leaves out.
const engineAurora = "aurora"

// What every Aurora cluster kraai creates is: the PostgreSQL-compatible
// engine, a serverless writer, a capacity range that pauses at zero, and
// the master credential managed by Secrets Manager. The master user is the
// engine's own default; the one database a fresh cluster holds is named
// after it.
const (
	auroraEngine        = "aurora-postgresql"
	auroraInstanceClass = "db.serverless"
	auroraMasterUser    = "postgres"
	auroraDatabase      = "postgres"
	auroraPort          = "5432"
	auroraMinCapacity   = 0
	auroraMaxCapacity   = 2
)

// SecretConnectionURI is the credential an Aurora cluster produces: a
// PostgreSQL connection URL carrying the master password. The same name
// Neon's branch uses, so a manifest maps either through envSecrets alike.
const SecretConnectionURI = "connection_uri"

// bindingEngineIs is satisfied when the binding entry's engine is one of
// engines; the empty string stands for an entry naming none.
func bindingEngineIs(engines ...string) resource.Applicability {
	return func(ctx resource.ApplicabilityContext) bool {
		declared, _ := ctx.Binding["engine"].(string)
		for _, engine := range engines {
			if declared == engine {
				return true
			}
		}
		return false
	}
}

// validateAuroraSpec refuses a binding that carries DynamoDB's keys or
// names no network, before plan reads anything.
func validateAuroraSpec(spec resource.Spec) error {
	for _, field := range []string{"partitionKey", "sortKey"} {
		if _, present := spec.Config[field]; present {
			return kerrors.Validation(
				"database binding %q declares %s, which is a DynamoDB key and means nothing on a postgres database",
				spec.Binding, field)
		}
	}
	if network, _ := spec.Config["network"].(string); network == "" {
		return kerrors.Validation(
			"database binding %q asks for engine aurora but names no network to place the cluster in", spec.Binding)
	}
	return nil
}

// auroraValidated wraps a translated resource with the Aurora binding
// check as plan.SpecValidator, so a binding naming no network fails on a
// fresh environment rather than only once a cluster exists to compare.
type auroraValidated struct {
	*translatedResource
}

func (a *auroraValidated) ValidateSpec(spec resource.Spec) error { return validateAuroraSpec(spec) }

// registerAurora returns the registrations for one Aurora Serverless v2
// cluster: the subnet group naming the network's subnets, the security
// group admitting the network's members, the cluster, and its one
// serverless writer.
func registerAurora(client *Client) []resource.Registration {
	subnetGroupKey := key(TypeRDSDBSubnetGroup)
	securityGroupKey := key(TypeDatabaseSecurityGroup)
	clusterKey := key(TypeRDSDBCluster)
	auroraOnly := []resource.Applicability{bindingDriverIs(DriverPostgres), bindingEngineIs(engineAurora)}

	return []resource.Registration{
		{
			Provider: Provider, Type: TypeRDSDBSubnetGroup, Capability: manifest.CapabilityDatabase,
			Applies: auroraOnly, Lookup: resource.LookupByName,
			Resource: &auroraValidated{translated(
				withIdentity(&resourceType{provider: Provider, typeName: TypeRDSDBSubnetGroup, lookup: resource.LookupByName, client: client}),
				func(spec resource.Spec) (resource.Spec, error) {
					name, network, err := referencedNetwork(spec, "database")
					if err != nil {
						return spec, err
					}
					translated := spec
					translated.Config = map[string]any{
						"DBSubnetGroupName":        spec.Name,
						"DBSubnetGroupDescription": "kraai: subnets of the " + name + " network for the " + spec.Binding + " cluster",
						"SubnetIds":                network.subnetIDs,
					}
					return translated, nil
				},
				nil)},
		},
		{
			Provider: Provider, Type: TypeDatabaseSecurityGroup, VendorType: TypeSecurityGroup,
			Capability: manifest.CapabilityDatabase,
			Applies:    auroraOnly, Lookup: resource.LookupByTag,
			ReadsReferences: []resource.ReferenceRead{{Key: "network", Type: key(TypeNetworkSecurityGroup)}},
			Resource: translated(taggedLookup(client, TypeSecurityGroup),
				func(spec resource.Spec) (resource.Spec, error) {
					name, network, err := referencedNetwork(spec, "database")
					if err != nil {
						return spec, err
					}
					ingress, err := admitting(spec, name, 5432, 5432)
					if err != nil {
						return spec, err
					}
					translated := spec
					translated.Config = map[string]any{
						"GroupName":            spec.Name + "-database",
						"GroupDescription":     "kraai: admits members of the " + name + " network to the " + spec.Binding + " cluster",
						"VpcId":                network.vpcID,
						"SecurityGroupIngress": []any{ingress},
					}
					return translated, nil
				},
				nil),
		},
		{
			Provider: Provider, Type: TypeRDSDBCluster, Capability: manifest.CapabilityDatabase,
			Applies: auroraOnly, Lookup: resource.LookupByName,
			DependsOn: []string{subnetGroupKey, securityGroupKey},
			Resource:  newAuroraClusterResource(client, subnetGroupKey, securityGroupKey),
		},
		{
			Provider: Provider, Type: TypeRDSDBInstance, Capability: manifest.CapabilityDatabase,
			Applies: auroraOnly, DependsOn: []string{clusterKey},
			// An instance's identifier is its own; the writer is found as
			// the instance whose cluster is this binding's.
			Lookup: resource.LookupByAttr,
			Resource: translated(
				&resourceType{
					provider: Provider, typeName: TypeRDSDBInstance, lookup: resource.LookupByAttr, client: client,
					match: func(properties map[string]any, name string) bool {
						return properties["DBClusterIdentifier"] == name
					},
				},
				func(spec resource.Spec) (resource.Spec, error) {
					translated := spec
					translated.Config = map[string]any{
						"DBInstanceIdentifier": spec.Name + "-writer",
						"DBClusterIdentifier":  spec.Name,
						"DBInstanceClass":      auroraInstanceClass,
						"Engine":               auroraEngine,
					}
					return translated, nil
				},
				nil),
		},
	}
}

// auroraClusterResource provisions the cluster and produces its credential.
type auroraClusterResource struct {
	*translatedResource
	client *Client
}

func newAuroraClusterResource(client *Client, subnetGroupKey, securityGroupKey string) *auroraClusterResource {
	engine := withIdentity(&resourceType{provider: Provider, typeName: TypeRDSDBCluster, lookup: resource.LookupByName, client: client})
	return &auroraClusterResource{
		client: client,
		translatedResource: translated(engine,
			func(spec resource.Spec) (resource.Spec, error) {
				groupID, err := spec.Attribute(securityGroupKey, "GroupId")
				if err != nil {
					return spec, err
				}
				subnetGroup, err := spec.Attribute(subnetGroupKey, "DBSubnetGroupName")
				if err != nil {
					return spec, err
				}
				translated := spec
				translated.Config = map[string]any{
					"DBClusterIdentifier": spec.Name,
					"Engine":              auroraEngine,
					"EngineMode":          "provisioned",
					"ServerlessV2ScalingConfiguration": map[string]any{
						"MinCapacity": auroraMinCapacity,
						"MaxCapacity": auroraMaxCapacity,
					},
					"MasterUsername":           auroraMasterUser,
					"ManageMasterUserPassword": true,
					"DBSubnetGroupName":        subnetGroup,
					"VpcSecurityGroupIds":      []any{groupID},
					"StorageEncrypted":         true,
					// Off, explicitly: an environment kraai created is one it
					// must be able to destroy.
					"DeletionProtection": false,
				}
				return translated, nil
			},
			nil),
	}
}

// ValidateSpec implements plan.SpecValidator.
func (a *auroraClusterResource) ValidateSpec(spec resource.Spec) error {
	return validateAuroraSpec(spec)
}

// Secrets implements resource.SecretProducer: the cluster's connection URL
// carries the master password RDS keeps in Secrets Manager, so it is read
// at the moment of use and never held in state.
func (a *auroraClusterResource) Secrets(state *resource.State) map[string]resource.Secret {
	if state == nil {
		return nil
	}
	endpoint, _ := state.Attributes["Endpoint"].(map[string]any)
	host, _ := endpoint["Address"].(string)
	port, _ := endpoint["Port"].(string)
	secret, _ := state.Attributes["MasterUserSecret"].(map[string]any)
	arn, _ := secret["SecretArn"].(string)
	name := state.Ref.Name
	return map[string]resource.Secret{
		SecretConnectionURI: func(ctx context.Context) (string, error) {
			if host == "" || arn == "" {
				return "", kerrors.Validation(
					"cluster %q published no endpoint or master secret to build a connection URL from", name)
			}
			if port == "" {
				port = auroraPort
			}
			value, err := a.client.SecretValue(ctx, arn)
			if err != nil {
				return "", err
			}
			var credential struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			if err := json.Unmarshal([]byte(value), &credential); err != nil {
				return "", kerrors.Validation("the master secret for cluster %q is not the username and password RDS writes", name)
			}
			return (&url.URL{
				Scheme:   "postgres",
				User:     url.UserPassword(credential.Username, credential.Password),
				Host:     host + ":" + port,
				Path:     "/" + auroraDatabase,
				RawQuery: "sslmode=require",
			}).String(), nil
		},
	}
}
