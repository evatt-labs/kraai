package aws

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

func clusterSpec(config map[string]any) resource.Spec {
	base := map[string]any{"driver": DriverPostgres}
	maps.Copy(base, config)
	return resource.Spec{Binding: "PG", Name: "env-svc-pg", Config: base}
}

// A cluster has no name property, so its identity is kraai's tag stamped at
// create; deletion protection is off so the environment can be destroyed.
func TestDSQLClusterCreateIsTaggedAndDeletable(t *testing.T) {
	fc := &fakeClient{createID: "abc123", createProps: map[string]any{
		"Identifier": "abc123", "Endpoint": "abc123.dsql.us-east-1.on.aws",
	}}
	cluster := newDSQLClusterResource(fc)

	state, err := cluster.Create(context.Background(), clusterSpec(nil))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := fc.createCalls[0]
	if desired["DeletionProtectionEnabled"] != false {
		t.Fatalf("DeletionProtectionEnabled = %v, want false", desired["DeletionProtectionEnabled"])
	}
	if !arrayTagsMatch(desired, "env-svc-pg") {
		t.Fatalf("desired state %v carries no identity tag, so the cluster could never be found again", desired)
	}
	if state.Attributes["Endpoint"] != "abc123.dsql.us-east-1.on.aws" {
		t.Fatalf("published attributes = %v, want the endpoint for the function to read", state.Attributes)
	}
}

func TestDSQLClusterGetMatchesOnTheIdentityTag(t *testing.T) {
	fc := &fakeClient{
		list: []string{"other", "mine"},
		byIdentifier: map[string]map[string]any{
			"other": taggedProps("env-svc-other", nil),
			"mine":  taggedProps("env-svc-pg", map[string]any{"Endpoint": "mine.dsql.us-east-1.on.aws"}),
		},
	}
	cluster := newDSQLClusterResource(fc)
	state, err := cluster.Get(context.Background(), resource.Ref{Name: "env-svc-pg"})
	if err != nil || state == nil || state.ID != "mine" {
		t.Fatalf("Get = %+v, %v; want the cluster carrying the tag", state, err)
	}
}

// One `databases:` entry shape serves DynamoDB and DSQL, so the schema
// accepts DynamoDB's keys on a postgres binding; the cluster refuses them
// before plan reads anything.
func TestDSQLClusterValidateSpecRefusesOtherEnginesKeys(t *testing.T) {
	cluster := newDSQLClusterResource(&fakeClient{})
	cases := map[string]map[string]any{
		"partitionKey":   {"partitionKey": map[string]any{"name": "pk"}},
		"sortKey":        {"sortKey": map[string]any{"name": "sk"}},
		"unknown engine": {"engine": "aurora"},
	}
	for label, config := range cases {
		t.Run(label, func(t *testing.T) {
			err := cluster.ValidateSpec(clusterSpec(config))
			if err == nil {
				t.Fatalf("ValidateSpec(%v) succeeded, want an error", config)
			}
			if !strings.Contains(err.Error(), `"PG"`) {
				t.Fatalf("error %q does not name the binding", err)
			}
		})
	}
	if err := cluster.ValidateSpec(clusterSpec(map[string]any{"engine": engineDSQL})); err != nil {
		t.Fatalf("ValidateSpec(engine dsql): %v", err)
	}
}

func TestDSQLURLIsPasswordlessTLSToTheAdminRole(t *testing.T) {
	attrKey := "PG." + key(TypeDSQLCluster)
	spec := resource.Spec{Binding: "api", Attributes: map[string]map[string]any{
		attrKey: {"Endpoint": "abc123.dsql.us-east-1.on.aws"},
	}}
	url, err := dsqlURL(spec, attrKey)
	if err != nil || url != "postgres://admin@abc123.dsql.us-east-1.on.aws:5432/postgres?sslmode=require" {
		t.Fatalf("dsqlURL = %q, %v", url, err)
	}
	if _, err := dsqlURL(spec, "PG.missing"); err == nil {
		t.Fatal("dsqlURL(unpublished) succeeded, want an error")
	}
}

// The two database engines apply to their own driver and never to each
// other's.
func TestDatabaseEnginesApplyToTheirOwnDriver(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg, &Client{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	vendors := map[string]string{manifest.CapabilityDatabase: Provider}
	for driver, want := range map[string]string{DriverPostgres: TypeDSQLCluster, DriverDynamoDB: TypeDynamoDBTable} {
		regs, err := reg.Resolve(manifest.CapabilityDatabase, resource.ApplicabilityContext{
			Vendors: vendors, Binding: map[string]any{"driver": driver},
		})
		if err != nil || len(regs) != 1 || regs[0].Type != want {
			t.Errorf("Resolve(driver %s) = %v, %v; want exactly %s", driver, regs, err, want)
		}
	}
}
