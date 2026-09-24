package cli

import (
	"context"
	"encoding/json"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/provider/aws"
)

// PolicyActions returns the IAM actions the given AWS vendor types need,
// for a manifest whose provider settings say which account and region to
// ask. assemble.AWSPolicyActions in production; a fake in tests.
type PolicyActions func(ctx context.Context, m *manifest.Manifest, vendorTypes []string) ([]string, error)

// SecretRefStatements returns one scoped IAM grant per secret reference the
// manifest's envSecrets declares. assemble.AWSSecretRefPolicyStatements in
// production; a fake in tests. Returns (nil, nil) for a manifest with none.
type SecretRefStatements func(ctx context.Context, m *manifest.Manifest) ([]aws.SecretRefGrant, error)

// SecretsStatements returns one scoped IAM grant per action a secrets
// binding's own parameters need. assemble.AWSSecretsPolicyStatements in
// production; a fake in tests. Returns (nil, nil) for a manifest with none.
type SecretsStatements func(ctx context.Context, m *manifest.Manifest, environmentName string) ([]aws.SecretRefGrant, error)

func newIAMPolicyCommand(
	assembler RegistryAssembler, resolve ManifestResolver, actions PolicyActions,
	secretRefs SecretRefStatements, secrets SecretsStatements,
) *cobra.Command {
	var (
		dir     string
		setArgs []string
	)

	cmd := &cobra.Command{
		Use:   "iam-policy <environment>",
		Short: "Print the least-privilege IAM policy the manifest needs on AWS",
		Long: "iam-policy prints, as an IAM policy document, every action a principal needs to\n" +
			"plan, apply and destroy the manifest's AWS resources for <environment>: each\n" +
			"resource type's own handler permissions, published in its CloudFormation\n" +
			"schema, plus the calls kraai makes beside them. It reads the schemas, never\n" +
			"the resources, so it needs only cloudformation:DescribeType to run.\n\n" +
			"Actions are granted on every resource: a handler publishes actions, not the\n" +
			"identifiers the provider will assign. Narrow the resource element by hand\n" +
			"where the account's naming allows it.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIAMPolicy(cmd, args[0], dir, setArgs, assembler, resolve, actions, secretRefs, secrets)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "manifest root directory")
	cmd.Flags().StringArrayVar(&setArgs, "set", nil,
		"override a manifest value (key=value); may be repeated")

	return cmd
}

func runIAMPolicy(
	cmd *cobra.Command, envName, dir string, setArgs []string,
	assembler RegistryAssembler, resolve ManifestResolver, actions PolicyActions,
	secretRefs SecretRefStatements, secrets SecretsStatements,
) error {
	if !naming.IsValidEnvironmentReference(envName) {
		return kerrors.Validation(
			"invalid environment name %q: must match kraai's ephemeral grammar (%s) "+
				"or its persistent grammar (%s)",
			envName, naming.NamePattern, naming.PersistentNamePattern)
	}
	fsys, err := manifest.NewFS(dir)
	if err != nil {
		return err
	}
	if err := env.LoadDotEnv(dir); err != nil {
		return err
	}
	ctx := cmd.Context()
	resolved, err := resolve(ctx, fsys, envName, setArgs)
	if err != nil {
		return err
	}
	defer func() { _ = resolved.Close(ctx) }()
	m := resolved.Manifest

	reg, err := assembler(ctx, m)
	if err != nil {
		return err
	}
	items, err := plan.New(reg).Expand(m, envName)
	if err != nil {
		return err
	}
	types := awsVendorTypes(items)
	if len(types) == 0 {
		return kerrors.Validation("the manifest declares no AWS resources for %q, so there is no policy to print", envName)
	}
	granted, err := actions(ctx, m, types)
	if err != nil {
		return err
	}
	grants, err := secretRefs(ctx, m)
	if err != nil {
		return err
	}
	secretsGrants, err := secrets(ctx, m, envName)
	if err != nil {
		return err
	}
	return writePolicy(cmd.OutOrStdout(), granted, append(grants, secretsGrants...))
}

// awsVendorTypes returns the distinct vendor types of the AWS resources
// among items, sorted: what the provider will actually be asked for, which
// is the type whose schema publishes the permissions.
//
// A secrets binding's parameters are excluded: PolicyActions grants "*" on
// every vendor type it is asked about, but a secrets entry's name is the
// manifest's own, the same reason a secret reference is scoped instead of
// asked for through this path (see writePolicy) — granting it here would
// widen every entry's ssm:GetParameter etc. to every parameter in the
// account.
func awsVendorTypes(items []plan.Item) []string {
	set := map[string]bool{}
	for _, it := range items {
		if it.Provider != "aws" || it.Capability == manifest.CapabilitySecrets {
			continue
		}
		typeName := it.VendorType
		if typeName == "" {
			typeName = it.Type
		}
		set[typeName] = true
	}
	out := make([]string, 0, len(set))
	for typeName := range set {
		out = append(out, typeName)
	}
	sort.Strings(out)
	return out
}

type policyDocument struct {
	Version   string            `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

type policyStatement struct {
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

// writePolicy renders one statement granting every action in actions on
// every resource ("*", as PolicyActions documents it must be), plus one
// statement per distinct action among grants, each scoped to exactly the
// resource ARNs a secret reference needs — never "*": unlike a
// provider-assigned identifier, a reference names its own resource in the
// manifest, so kraai can scope it precisely.
func writePolicy(w io.Writer, actions []string, grants []aws.SecretRefGrant) error {
	statements := []policyStatement{{Effect: "Allow", Action: actions, Resource: []string{"*"}}}
	statements = append(statements, secretRefStatements(grants)...)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(policyDocument{Version: "2012-10-17", Statement: statements})
}

// secretRefStatements groups grants by action into one statement per
// action, each listing every distinct resource ARN that action was granted
// on, sorted for a stable, diffable policy document.
func secretRefStatements(grants []aws.SecretRefGrant) []policyStatement {
	byAction := map[string]map[string]bool{}
	var actions []string
	for _, g := range grants {
		resources, ok := byAction[g.Action]
		if !ok {
			resources = map[string]bool{}
			byAction[g.Action] = resources
			actions = append(actions, g.Action)
		}
		resources[g.Resource] = true
	}
	sort.Strings(actions)

	statements := make([]policyStatement, 0, len(actions))
	for _, action := range actions {
		resources := make([]string, 0, len(byAction[action]))
		for r := range byAction[action] {
			resources = append(resources, r)
		}
		sort.Strings(resources)
		statements = append(statements, policyStatement{Effect: "Allow", Action: []string{action}, Resource: resources})
	}
	return statements
}
