package aws

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
)

// countingCF answers DescribeType with one schema and counts the calls.
type countingCF struct {
	schema string
	calls  int
}

func (f *countingCF) DescribeType(context.Context, *cloudformation.DescribeTypeInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeTypeOutput, error) {
	f.calls++
	return &cloudformation.DescribeTypeOutput{Schema: aws.String(f.schema)}, nil
}

const cachedQueueSchema = `{"typeName":"AWS::SQS::Queue","properties":{"QueueName":{"type":"string"}},"primaryIdentifier":["/properties/QueueName"],"handlers":{"update":{}}}`

func newCachingClient(t *testing.T, cf *countingCF) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	c := &Client{cf: cf, region: "us-east-1"}
	WithSchemaCache(dir, time.Hour)(c)
	return c, filepath.Join(dir, "us-east-1", "AWS--SQS--Queue.json")
}

func readCached(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a path under the test's temp dir
	if err != nil {
		t.Fatalf("reading the cache: %v", err)
	}
	return string(raw)
}

func TestSchemaCacheServesAFreshCopyWithoutACall(t *testing.T) {
	cf := &countingCF{schema: cachedQueueSchema}
	c, path := newCachingClient(t, cf)

	for range 3 {
		facts, err := c.DescribeType(context.Background(), TypeSQSQueue)
		if err != nil || facts.IdentityProperty != "QueueName" {
			t.Fatalf("DescribeType = %+v, %v", facts, err)
		}
	}
	if cf.calls != 1 {
		t.Fatalf("DescribeType was called %d times, want 1", cf.calls)
	}
	if raw := readCached(t, path); raw != cachedQueueSchema {
		t.Fatalf("cache file = %q", raw)
	}

	// A second client, a second run, reads the same file.
	next := &countingCF{schema: cachedQueueSchema}
	c2 := &Client{cf: next, region: "us-east-1"}
	WithSchemaCache(filepath.Dir(filepath.Dir(path)), time.Hour)(c2)
	if _, err := c2.DescribeType(context.Background(), TypeSQSQueue); err != nil || next.calls != 0 {
		t.Fatalf("second run: calls=%d, err=%v", next.calls, err)
	}
}

func TestSchemaCacheRefetchesAStaleOrCorruptCopy(t *testing.T) {
	for name, spoil := range map[string]func(t *testing.T, path string){
		"stale": func(t *testing.T, path string) {
			t.Helper()
			old := time.Now().Add(-2 * time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		},
		"dated in the future": func(t *testing.T, path string) {
			t.Helper()
			future := time.Now().Add(10 * 365 * 24 * time.Hour)
			if err := os.Chtimes(path, future, future); err != nil {
				t.Fatal(err)
			}
		},
		"another type's schema": func(t *testing.T, path string) {
			t.Helper()
			other := `{"typeName":"AWS::SNS::Topic","properties":{}}`
			if err := os.WriteFile(path, []byte(other), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"corrupt": func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(`{"typeName":`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cf := &countingCF{schema: cachedQueueSchema}
			c, path := newCachingClient(t, cf)
			if _, err := c.DescribeType(context.Background(), TypeSQSQueue); err != nil {
				t.Fatal(err)
			}
			spoil(t, path)

			if _, err := c.DescribeType(context.Background(), TypeSQSQueue); err != nil {
				t.Fatalf("DescribeType: %v", err)
			}
			if cf.calls != 2 {
				t.Fatalf("DescribeType was called %d times, want a refetch", cf.calls)
			}
			if raw := readCached(t, path); raw != cachedQueueSchema {
				t.Fatalf("cache file was not rewritten: %q", raw)
			}
		})
	}
}

func TestSchemaCacheSkipsWhatItMustNotKeep(t *testing.T) {
	t.Run("third-party type", func(t *testing.T) {
		cf := &countingCF{schema: `{"typeName":"MongoDB::Atlas::Cluster"}`}
		c, _ := newCachingClient(t, cf)
		for range 2 {
			if _, err := c.DescribeType(context.Background(), "MongoDB::Atlas::Cluster"); err != nil {
				t.Fatal(err)
			}
		}
		if cf.calls != 2 {
			t.Fatalf("a third-party schema was cached: %d calls", cf.calls)
		}
	})
	// A name that is not a plain AWS type or region never becomes a path.
	t.Run("traversal", func(t *testing.T) {
		c := &Client{region: "us-east-1"}
		WithSchemaCache(t.TempDir(), time.Hour)(c)
		for _, typeName := range []string{"AWS::../../x::y", "AWS::SQS::Queue/../../x", "AWS::SQS", "../AWS::SQS::Queue"} {
			if path := c.schemaCachePath(typeName); path != "" {
				t.Errorf("schemaCachePath(%q) = %q", typeName, path)
			}
		}
		c.region = "../../etc"
		if path := c.schemaCachePath(TypeSQSQueue); path != "" {
			t.Errorf("a traversing region became %q", path)
		}
	})
	t.Run("no cache configured", func(t *testing.T) {
		cf := &countingCF{schema: cachedQueueSchema}
		c := &Client{cf: cf, region: "us-east-1"}
		for range 2 {
			if _, err := c.DescribeType(context.Background(), TypeSQSQueue); err != nil {
				t.Fatal(err)
			}
		}
		if cf.calls != 2 {
			t.Fatalf("calls = %d, want every call to fetch", cf.calls)
		}
	})
}

func TestPropertySchemaValidatesTheTypesProperties(t *testing.T) {
	cf := &countingCF{schema: cachedQueueSchema}
	c, _ := newCachingClient(t, cf)
	schema, err := c.PropertySchema(context.Background(), TypeSQSQueue)
	if err != nil {
		t.Fatalf("PropertySchema: %v", err)
	}
	if err := schema.Validate(map[string]any{"QueueName": "q"}); err != nil {
		t.Errorf("valid properties rejected: %v", err)
	}
	if err := schema.Validate(map[string]any{"QueueNme": "q"}); err == nil {
		t.Error("an undefined property was accepted")
	}
	if _, err := c.DescribeType(context.Background(), TypeSQSQueue); err != nil || cf.calls != 1 {
		t.Fatalf("validation and facts did not share one fetch: calls=%d, err=%v", cf.calls, err)
	}
}
