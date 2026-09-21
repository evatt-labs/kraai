package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeDSQLCluster is AWS::DSQL::Cluster's Cloud Control TypeName.
const TypeDSQLCluster = "AWS::DSQL::Cluster"

// DriverPostgres is the driver a database binding declares to ask this
// provider for a PostgreSQL-protocol database. The engine behind it is
// Aurora DSQL: serverless, reachable without a VPC, authenticated with IAM
// rather than a password, and created in seconds, which is what an
// environment kraai makes and destroys per branch wants from a database.
const DriverPostgres = "postgres"

// engineDSQL is the one engine a postgres binding has on this provider
// today. Named so a binding can ask for it explicitly, and so an Aurora
// engine can register beside it under its own value.
const engineDSQL = "dsql"

// dsqlPort is the port every DSQL cluster listens on; dsqlDatabase and
// dsqlAdminRole are the one database a cluster holds and the role DSQL
// creates in it.
const (
	dsqlPort      = "5432"
	dsqlDatabase  = "postgres"
	dsqlAdminRole = "admin"
)

// dsqlClusterResource provisions one Aurora DSQL cluster per database
// binding declaring driver postgres.
//
// Found by kraai's identity tag: a cluster's identifier is assigned by
// DSQL, and the type has no name property at all.
type dsqlClusterResource struct {
	*resourceType
}

func newDSQLClusterResource(client ccAPI) *dsqlClusterResource {
	engine := taggedLookup(client, TypeDSQLCluster)
	engine.translate = dsqlClusterTranslate
	return &dsqlClusterResource{resourceType: engine}
}

// dsqlClusterTranslate builds the cluster's desired state. Deletion
// protection off, explicitly: DSQL turns it on by default, and an
// environment kraai created is one it must be able to destroy.
func dsqlClusterTranslate(_ context.Context, spec resource.Spec) (resource.Spec, error) {
	if err := validateDSQLSpec(spec); err != nil {
		return resource.Spec{}, err
	}
	translated := spec
	translated.Config = map[string]any{"DeletionProtectionEnabled": false}
	return translated, nil
}

// validateDSQLSpec refuses a binding that carries another engine's keys or
// asks for an engine this driver does not have. The binding schema accepts
// partitionKey and sortKey because the same `databases:` entry shape
// serves DynamoDB; on a cluster they mean nothing, and a manifest writing
// them has confused its engines.
func validateDSQLSpec(spec resource.Spec) error {
	for _, field := range []string{"partitionKey", "sortKey"} {
		if _, present := spec.Config[field]; present {
			return kerrors.Validation(
				"database binding %q declares %s, which is a DynamoDB key and means nothing on a postgres database",
				spec.Binding, field)
		}
	}
	if engine, _ := spec.Config["engine"].(string); engine != "" && engine != engineDSQL {
		return kerrors.Validation(
			"database binding %q asks for engine %q, which this provider does not offer for driver postgres",
			spec.Binding, engine)
	}
	return nil
}

// ValidateSpec implements plan.SpecValidator, so a confused binding fails on
// a fresh environment rather than only once a cluster exists to compare.
func (d *dsqlClusterResource) ValidateSpec(spec resource.Spec) error {
	return validateDSQLSpec(spec)
}

// dsqlURL builds the connection URL a client opens to the cluster from the
// endpoint it published. No password: DSQL authenticates a connection with
// an IAM token the client generates for the endpoint at connect time, which
// the execution role's grant (iamrole.go) authorizes.
func dsqlURL(spec resource.Spec, attributeKey string) (string, error) {
	endpoint, err := spec.Attribute(attributeKey, "Endpoint")
	if err != nil {
		return "", err
	}
	return "postgres://" + dsqlAdminRole + "@" + endpoint + ":" + dsqlPort + "/" + dsqlDatabase + "?sslmode=require", nil
}

// dsqlClusterPattern is the resource a connect grant names: every cluster
// in the region, narrowed by a condition on kraai's identity tag, because
// the cluster's own ARN carries an identifier DSQL assigns and the grant is
// built before the cluster exists.
func dsqlClusterPattern(region, account string) string {
	return "arn:aws:dsql:" + region + ":" + account + ":cluster/*"
}
