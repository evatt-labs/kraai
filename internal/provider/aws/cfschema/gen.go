//go:build ignore

// gen fetches every AWS-published provisionable resource type's schema,
// read-only, derives its Facts and writes the gzipped JSON index the
// package embeds. Raw schemas are cached on disk so a re-run is offline.
//
//	go run gen.go -out index.json.gz [-cache DIR] [-region us-east-1]
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"golang.org/x/sync/errgroup"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		out     = flag.String("out", "index.json.gz", "index file to write")
		cache   = flag.String("cache", "", "directory of raw schemas, one <Type>.json per type; defaults to the user cache directory")
		region  = flag.String("region", "us-east-1", "region whose public registry to read")
		workers = flag.Int("workers", 6, "concurrent DescribeType calls")
		accept  = flag.Bool("accept-identity-changes", false,
			"write the index even though it finds an already-indexed type differently")
	)
	flag.Parse()
	if *cache == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		*cache = filepath.Join(dir, "kraai", "cfschema", *region)
	}
	if err := os.MkdirAll(*cache, 0o755); err != nil {
		return err
	}

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region),
		config.WithRetryMode(aws.RetryModeAdaptive), config.WithRetryMaxAttempts(10))
	if err != nil {
		return err
	}
	cf := cloudformation.NewFromConfig(cfg)

	names, err := listTypes(ctx, cf)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d AWS-published provisionable types\n", len(names))

	var mu sync.Mutex
	index := map[string]cfschema.Facts{}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(*workers)
	for _, name := range names {
		g.Go(func() error {
			raw, err := schemaFor(gctx, cf, *cache, name)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			doc, err := cfschema.Parse(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			facts := cfschema.Derive(doc)
			mu.Lock()
			index[name] = facts
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// A regenerated index that finds an existing type differently can leave
	// an environment an older kraai created unplannable and undestroyable,
	// so it is a reviewed decision, never a side effect.
	if previous, err := os.ReadFile(*out); err == nil {
		prev, err := cfschema.DecodeIndex(previous)
		if err != nil {
			return fmt.Errorf("reading the current index %s: %w", *out, err)
		}
		if changes := cfschema.IdentityChanges(prev, index); len(changes) > 0 {
			for _, change := range changes {
				fmt.Fprintln(os.Stderr, "identity change:", change)
			}
			if !*accept {
				return fmt.Errorf("%d indexed types would be found differently; review them, then rerun with -accept-identity-changes", len(changes))
			}
		}
	}

	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(zw)
	if err := enc.Encode(index); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s: %d types, %d bytes\n", *out, len(index), buf.Len())
	return nil
}

// listTypes returns the AWS:: public resource types Cloud Control can
// provision: FULLY_MUTABLE and IMMUTABLE. NON_PROVISIONABLE types are
// read-only in Cloud Control and have no place in a plan.
func listTypes(ctx context.Context, cf *cloudformation.Client) ([]string, error) {
	seen := map[string]bool{}
	for _, pt := range []cftypes.ProvisioningType{cftypes.ProvisioningTypeFullyMutable, cftypes.ProvisioningTypeImmutable} {
		p := cloudformation.NewListTypesPaginator(cf, &cloudformation.ListTypesInput{
			Type:             cftypes.RegistryTypeResource,
			Visibility:       cftypes.VisibilityPublic,
			ProvisioningType: pt,
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, s := range page.TypeSummaries {
				if name := aws.ToString(s.TypeName); strings.HasPrefix(name, "AWS::") {
					seen[name] = true
				}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// schemaFor returns the raw schema for name from the cache, fetching and
// caching it on a miss.
func schemaFor(ctx context.Context, cf *cloudformation.Client, cache, name string) ([]byte, error) {
	path := filepath.Join(cache, strings.ReplaceAll(name, "::", "--")+".json")
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		return raw, nil
	}
	out, err := cf.DescribeType(ctx, &cloudformation.DescribeTypeInput{
		Type:     cftypes.RegistryTypeResource,
		TypeName: aws.String(name),
	})
	if err != nil {
		return nil, err
	}
	if out.Schema == nil {
		return nil, fmt.Errorf("DescribeType returned no schema")
	}
	raw := []byte(*out.Schema)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return nil, err
	}
	return raw, nil
}
