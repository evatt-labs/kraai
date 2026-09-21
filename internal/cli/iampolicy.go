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
)

// PolicyActions returns the IAM actions the given AWS vendor types need,
// for a manifest whose provider settings say which account and region to
// ask. assemble.AWSPolicyActions in production; a fake in tests.
type PolicyActions func(ctx context.Context, m *manifest.Manifest, vendorTypes []string) ([]string, error)

func newIAMPolicyCommand(assembler RegistryAssembler, resolve ManifestResolver, actions PolicyActions) *cobra.Command {
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
			return runIAMPolicy(cmd, args[0], dir, setArgs, assembler, resolve, actions)
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
	return writePolicy(cmd.OutOrStdout(), granted)
}

// awsVendorTypes returns the distinct vendor types of the AWS resources
// among items, sorted: what the provider will actually be asked for, which
// is the type whose schema publishes the permissions.
func awsVendorTypes(items []plan.Item) []string {
	set := map[string]bool{}
	for _, it := range items {
		if it.Provider != "aws" {
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
	Resource string   `json:"Resource"`
}

func writePolicy(w io.Writer, actions []string) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(policyDocument{
		Version:   "2012-10-17",
		Statement: []policyStatement{{Effect: "Allow", Action: actions, Resource: "*"}},
	})
}
