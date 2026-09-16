// Command gen-lambda-schema fetches every AWS::Lambda::* CloudFormation
// resource provider schema from the live registry and writes a generated
// Go source file for internal/spike/awsl1.
//
// This is a build-time generator, not part of kraai's own CLI
// (internal/cli, out of scope for this workstream) — a go:generate-able
// tool under internal/generate/, matching the brief's own framing of that
// directory. It is a `package main` outside cmd/kraai, so .golangci.yml's
// forbidigo rules (os.Exit, fmt.Print*, os.Getenv — D18/D19, scoped to
// cmd/kraai and internal/env) apply here unless excluded; see the nolint
// comments below and SPIKE.md's own note on this — a real (non-spike)
// version of this tool would add internal/generate/ to that exclusion list
// the same way cmd/kraai and internal/env already are, rather than carrying
// per-line nolint comments indefinitely.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/evatt-labs/kraai/internal/generate/awsschema"
)

// lambdaTypes is every AWS::Lambda::* resource type CloudFormation's public
// registry knows about in us-east-1 as of 2026-09-16 (`aws cloudformation
// list-types --visibility PUBLIC --type RESOURCE`, all three provisioning
// types, filtered to the AWS::Lambda:: prefix) — eleven FULLY_MUTABLE, three
// IMMUTABLE (LayerVersion, LayerVersionPermission, Permission — no update
// handler), one NON_PROVISIONABLE (DurableExecution, read-only surface with
// no create/update/delete handlers of its own). "Every AWS::Lambda::* type,
// not only the three kraai registers" is the brief's own instruction:
// "since 'snapshot of the whole surface' is the point."
var lambdaTypes = []string{
	"AWS::Lambda::Alias",
	"AWS::Lambda::CapacityProvider",
	"AWS::Lambda::CodeSigningConfig",
	"AWS::Lambda::DurableExecution",
	"AWS::Lambda::EventInvokeConfig",
	"AWS::Lambda::EventSourceMapping",
	"AWS::Lambda::Function",
	"AWS::Lambda::LayerVersion",
	"AWS::Lambda::LayerVersionPermission",
	"AWS::Lambda::MicrovmImage",
	"AWS::Lambda::NetworkConnector",
	"AWS::Lambda::Permission",
	"AWS::Lambda::ResourcePolicy",
	"AWS::Lambda::Url",
	"AWS::Lambda::Version",
}

func main() {
	out := flag.String("out", "internal/spike/awsl1/lambda_gen.go", "output path for the generated Go source")
	pkg := flag.String("pkg", "awsl1", "package name for the generated file")
	flag.Parse()

	if err := run(*out, *pkg); err != nil {
		fmt.Fprintln(os.Stderr, "gen-lambda-schema:", err)
		os.Exit(1) //nolint:forbidigo // build-time generator CLI, not cmd/kraai — see this file's own doc comment.
	}
}

func run(out, pkg string) error {
	ctx := context.Background()

	fetcher, err := awsschema.NewFetcher(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "gen-lambda-schema: fetching %d AWS::Lambda::* schemas from CloudFormation (us-east-1)\n", len(lambdaTypes))
	types, err := fetcher.FetchAll(ctx, lambdaTypes)
	if err != nil {
		return err
	}

	src, err := awsschema.Generate(pkg, types)
	if err != nil {
		return err
	}

	if err := os.WriteFile(out, src, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}
	fmt.Fprintf(os.Stderr, "gen-lambda-schema: wrote %s (%d types, %d bytes)\n", out, len(types), len(src))
	return nil
}
