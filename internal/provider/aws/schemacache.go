package aws

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// DefaultSchemaCacheTTL is how long a fetched schema is reused before it is
// fetched again. AWS revises a type's schema on its own schedule, and a day
// bounds how stale a printed IAM policy or a replace decision can be.
const DefaultSchemaCacheTTL = 24 * time.Hour

// WithSchemaCache keeps fetched schemas under dir, one file per region and
// type, and reuses one younger than ttl instead of calling DescribeType. The
// layout is the one cfschema's generator reads, so the two share a cache.
func WithSchemaCache(dir string, ttl time.Duration) Option {
	return func(c *Client) {
		c.schemaCacheDir = dir
		c.schemaCacheTTL = ttl
	}
}

// schemaDocument returns typeName's raw schema, from the on-disk cache when
// a fresh copy is there and from DescribeType otherwise.
func (c *Client) schemaDocument(ctx context.Context, typeName string) ([]byte, error) {
	path := c.schemaCachePath(typeName)
	if path != "" {
		if raw, ok := readFresh(path, c.schemaCacheTTL); ok {
			return raw, nil
		}
	}
	raw, err := c.fetchSchema(ctx, typeName)
	if err != nil {
		return nil, err
	}
	if path != "" {
		// A cache that cannot be written costs the next run one
		// DescribeType call and nothing else, so it does not fail this one.
		_ = writeAtomic(path, raw)
	}
	return raw, nil
}

// cacheableType is the shape of an AWS-published type name. Anything else
// is never cached: a third-party type's schema depends on which version this
// account activated, and a name that is not three plain segments must never
// become part of a file path.
var cacheableType = regexp.MustCompile(`^AWS::[A-Za-z0-9]+::[A-Za-z0-9]+$`)

// cacheableRegion is the shape of an AWS region name, for the same reason.
var cacheableRegion = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// schemaCachePath is where typeName's schema is cached, or "" when it is not.
func (c *Client) schemaCachePath(typeName string) string {
	if c.schemaCacheDir == "" || !cacheableRegion.MatchString(c.region) || !cacheableType.MatchString(typeName) {
		return ""
	}
	return filepath.Join(c.schemaCacheDir, c.region, strings.ReplaceAll(typeName, "::", "--")+".json")
}

// readFresh returns path's contents when it exists, is younger than ttl and
// decodes as a schema. Anything else is a miss, so a truncated or corrupt
// file is fetched again and overwritten rather than trusted.
func readFresh(path string, ttl time.Duration) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) >= ttl {
		return nil, false
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: schemaCachePath admits only a validated type and region
	if err != nil {
		return nil, false
	}
	if _, err := cfschema.Parse(raw); err != nil {
		return nil, false
	}
	return raw, true
}

// writeAtomic replaces path with data through a temporary file and a
// rename, so a concurrent reader sees the old file or the new one, never a
// partial write.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".schema-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// PropertySchema returns a validator for typeName's properties, built from
// its full resource provider schema.
func (c *Client) PropertySchema(ctx context.Context, typeName string) (*resource.Schema, error) {
	raw, err := c.schemaDocument(ctx, typeName)
	if err != nil {
		return nil, err
	}
	doc, err := cfschema.PropertiesSchema(raw)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding schema for %s", typeName)
	}
	schema := resource.NewVendorSchema(typeName+" properties", doc)
	if err := schema.Compile(); err != nil {
		return nil, err
	}
	return schema, nil
}
