package aws

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// auroraResource returns the registered Resource for one Aurora type, wired
// to fc for Cloud Control and sm for Secrets Manager.
func auroraResource(t *testing.T, fc *fakeClient, sm secretsManagerAPI, typeName string) resource.Resource {
	t.Helper()
	client := &Client{sm: sm}
	for _, r := range registerAurora(client) {
		if r.Type != typeName {
			continue
		}
		switch res := r.Resource.(type) {
		case *auroraValidated:
			res.client = fc
		case *auroraClusterResource:
			res.resourceType.client = fc
		case *translatedResource:
			res.client = fc
		}
		return r.Resource
	}
	t.Fatalf("no aurora registration for %s", typeName)
	return nil
}

func auroraSpec(config map[string]any, attrs map[string]map[string]any) resource.Spec {
	base := map[string]any{"driver": DriverPostgres, "engine": engineAurora, "network": "NET"}
	maps.Copy(base, config)
	return resource.Spec{Binding: "SQL", Name: "env-svc-sql", Config: base, Attributes: attrs}
}

type fakeSecretsManager struct {
	value string
	err   error
	arns  []string
	// inputs captures every call's full input, so a test can assert on
	// VersionStage or VersionId, not just SecretId. secretref_test.go uses
	// this; aurora_test.go's own cases only ever check arns.
	inputs []*secretsmanager.GetSecretValueInput
}

func (f *fakeSecretsManager) GetSecretValue(_ context.Context, params *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.arns = append(f.arns, aws.ToString(params.SecretId))
	f.inputs = append(f.inputs, params)
	if f.err != nil {
		return nil, f.err
	}
	return &secretsmanager.GetSecretValueOutput{SecretString: aws.String(f.value)}, nil
}

// The four Aurora types apply to a postgres binding asking for the aurora
// engine and to no other; DSQL keeps the bare and dsql engines.
func TestAuroraAppliesOnlyToItsEngine(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg, &Client{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	vendors := map[string]string{manifest.CapabilityDatabase: Provider}
	aurora, err := reg.Resolve(manifest.CapabilityDatabase, resource.ApplicabilityContext{
		Vendors: vendors, Binding: map[string]any{"driver": DriverPostgres, "engine": engineAurora, "network": "NET"},
	})
	if err != nil || len(aurora) != 4 {
		t.Fatalf("Resolve(engine aurora) = %d registrations, %v; want 4", len(aurora), err)
	}
	for _, r := range aurora {
		if r.Type == TypeDSQLCluster {
			t.Fatal("the DSQL cluster applied to an aurora binding")
		}
	}
	for _, engine := range []string{"", engineDSQL} {
		dsql, err := reg.Resolve(manifest.CapabilityDatabase, resource.ApplicabilityContext{
			Vendors: vendors, Binding: map[string]any{"driver": DriverPostgres, "engine": engine},
		})
		if err != nil || len(dsql) != 1 || dsql[0].Type != TypeDSQLCluster {
			t.Fatalf("Resolve(engine %q) = %v, %v; want the DSQL cluster alone", engine, dsql, err)
		}
	}
}

// The subnet group takes the network's private pair when it has one and the
// public pair otherwise, from what the network published.
func TestAuroraSubnetGroupPrefersThePrivatePair(t *testing.T) {
	fc := &fakeClient{createID: "env-svc-sql", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/DBSubnetGroupName"}}}
	res := auroraResource(t, fc, nil, TypeRDSDBSubnetGroup)

	public := map[string]map[string]any{
		"NET." + key(TypeSubnet):        {"SubnetId": "subnet-pub-a"},
		"NET." + key(TypePublicSubnetB): {"SubnetId": "subnet-pub-b"},
	}
	if _, err := res.Create(context.Background(), auroraSpec(nil, public)); err != nil {
		t.Fatalf("Create(public network): %v", err)
	}
	if ids, _ := fc.createCalls[0]["SubnetIds"].([]any); len(ids) != 2 || ids[0] != "subnet-pub-a" || ids[1] != "subnet-pub-b" {
		t.Fatalf("SubnetIds = %v, want the public pair", fc.createCalls[0]["SubnetIds"])
	}
	if fc.createCalls[0]["DBSubnetGroupName"] != "env-svc-sql" {
		t.Fatalf("DBSubnetGroupName = %v", fc.createCalls[0]["DBSubnetGroupName"])
	}

	private := maps.Clone(public)
	private["NET."+key(TypePrivateSubnet)] = map[string]any{"SubnetId": "subnet-priv-a"}
	private["NET."+key(TypePrivateSubnetB)] = map[string]any{"SubnetId": "subnet-priv-b"}
	fc.createCalls = nil
	if _, err := res.Create(context.Background(), auroraSpec(nil, private)); err != nil {
		t.Fatalf("Create(private network): %v", err)
	}
	if ids, _ := fc.createCalls[0]["SubnetIds"].([]any); len(ids) != 2 || ids[0] != "subnet-priv-a" || ids[1] != "subnet-priv-b" {
		t.Fatalf("SubnetIds = %v, want the private pair", fc.createCalls[0]["SubnetIds"])
	}
}

func TestAuroraValidateSpecRequiresANetworkAndRefusesDynamoDBKeys(t *testing.T) {
	res := auroraResource(t, &fakeClient{}, nil, TypeRDSDBCluster)
	validator := res.(interface{ ValidateSpec(resource.Spec) error })
	if err := validator.ValidateSpec(auroraSpec(nil, nil)); err != nil {
		t.Fatalf("ValidateSpec(complete): %v", err)
	}
	noNetwork := auroraSpec(nil, nil)
	delete(noNetwork.Config, "network")
	if err := validator.ValidateSpec(noNetwork); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("ValidateSpec(no network): err = %v, want one naming the network", err)
	}
	if err := validator.ValidateSpec(auroraSpec(map[string]any{"partitionKey": map[string]any{"name": "pk"}}, nil)); err == nil {
		t.Fatal("ValidateSpec(partitionKey) succeeded, want an error")
	}
}

// The cluster is a serverless PostgreSQL writer pausing at zero, with its
// master password managed by Secrets Manager and deletion protection off.
func TestAuroraClusterIsServerlessAndDeletable(t *testing.T) {
	fc := &fakeClient{createID: "env-svc-sql", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/DBClusterIdentifier"}}}
	res := auroraResource(t, fc, nil, TypeRDSDBCluster)
	attrs := map[string]map[string]any{
		key(TypeDatabaseSecurityGroup): {"GroupId": "sg-db"},
		key(TypeRDSDBSubnetGroup):      {"DBSubnetGroupName": "env-svc-sql"},
	}
	if _, err := res.Create(context.Background(), auroraSpec(nil, attrs)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := fc.createCalls[0]
	if desired["Engine"] != auroraEngine || desired["EngineMode"] != "provisioned" || desired["MasterUsername"] != auroraMasterUser {
		t.Fatalf("desired = %v, want an aurora-postgresql cluster in provisioned mode with the postgres master user", desired)
	}
	scaling, _ := desired["ServerlessV2ScalingConfiguration"].(map[string]any)
	if scaling["MinCapacity"] != auroraMinCapacity || scaling["MaxCapacity"] != auroraMaxCapacity {
		t.Fatalf("ServerlessV2ScalingConfiguration = %v, want %d to %d ACUs", scaling, auroraMinCapacity, auroraMaxCapacity)
	}
	if desired["ManageMasterUserPassword"] != true || desired["DeletionProtection"] != false || desired["StorageEncrypted"] != true {
		t.Fatalf("desired = %v, want a managed password, no deletion protection, encrypted storage", desired)
	}
	if groups, _ := desired["VpcSecurityGroupIds"].([]any); len(groups) != 1 || groups[0] != "sg-db" || desired["DBSubnetGroupName"] != "env-svc-sql" {
		t.Fatalf("desired = %v, want the database security group and subnet group", desired)
	}
}

func TestAuroraWriterBelongsToItsCluster(t *testing.T) {
	fc := &fakeClient{createID: "env-svc-sql-writer", createProps: map[string]any{},
		list: []string{"other-writer", "env-svc-sql-writer"},
		byIdentifier: map[string]map[string]any{
			"other-writer":       {"DBClusterIdentifier": "other"},
			"env-svc-sql-writer": {"DBClusterIdentifier": "env-svc-sql"},
		}}
	res := auroraResource(t, fc, nil, TypeRDSDBInstance)
	state, err := res.Get(context.Background(), resource.Ref{Name: "env-svc-sql"})
	if err != nil || state == nil || state.ID != "env-svc-sql-writer" {
		t.Fatalf("Get = %+v, %v; want the instance whose cluster is this binding's", state, err)
	}
	if _, err := res.Create(context.Background(), auroraSpec(nil, nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := fc.createCalls[0]
	if desired["DBInstanceClass"] != auroraInstanceClass || desired["DBClusterIdentifier"] != "env-svc-sql" || desired["Engine"] != auroraEngine {
		t.Fatalf("desired = %v, want a serverless writer in the cluster", desired)
	}
}

// The credential is the connection URL with the password RDS keeps in
// Secrets Manager, read at the moment of use and never from state.
func TestAuroraClusterProducesItsConnectionURL(t *testing.T) {
	sm := &fakeSecretsManager{value: `{"username":"postgres","password":"p@ss word"}`}
	res := auroraResource(t, &fakeClient{}, sm, TypeRDSDBCluster)
	producer := res.(resource.SecretProducer)
	state := &resource.State{
		Ref: resource.Ref{Name: "env-svc-sql"},
		Attributes: map[string]any{
			"Endpoint":         map[string]any{"Address": "sql.cluster-abc.us-east-1.rds.amazonaws.com", "Port": "5432"},
			"MasterUserSecret": map[string]any{"SecretArn": "arn:aws:secretsmanager:us-east-1:123456789012:secret:rds!x"}, //nolint:gosec // a fixture ARN, not a credential
		},
	}
	secrets := producer.Secrets(state)
	if len(sm.arns) != 0 {
		t.Fatal("Secrets read the secret eagerly; it must be read only when the credential is used")
	}
	url, err := secrets[SecretConnectionURI](context.Background())
	if err != nil {
		t.Fatalf("connection_uri: %v", err)
	}
	want := "postgres://postgres:p%40ss%20word@sql.cluster-abc.us-east-1.rds.amazonaws.com:5432/postgres?sslmode=require" //nolint:gosec // the fixture password this test exists to see encoded
	if url != want {
		t.Fatalf("connection_uri = %q, want %q", url, want)
	}
	if len(sm.arns) != 1 || sm.arns[0] != "arn:aws:secretsmanager:us-east-1:123456789012:secret:rds!x" {
		t.Fatalf("Secrets Manager asked for %v, want the cluster's master secret once", sm.arns)
	}

	unpublished := producer.Secrets(&resource.State{Ref: resource.Ref{Name: "env-svc-sql"}, Attributes: map[string]any{}})
	if _, err := unpublished[SecretConnectionURI](context.Background()); err == nil {
		t.Fatal("a cluster with no endpoint or secret produced a URL")
	}
	if producer.Secrets(nil) != nil {
		t.Fatal("Secrets(nil) produced something")
	}
}
