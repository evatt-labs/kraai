//go:build integration

package aws

import (
	"context"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

// GetResource through the direct reader against GetResource through Cloud
// Control alone, on live task definitions, read-only: the same answer for
// active revisions under direct.Compare's declared normalizations, and
// not-found for deregistered ones on both.
//
//	go test -tags integration ./internal/provider/aws -run TestDirectGetResourceLive -v
func TestDirectGetResourceLive(t *testing.T) {
	ctx := context.Background()
	viaDirect, err := New(ctx, Settings{Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	viaCC, err := New(ctx, Settings{Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	viaCC.direct = nil

	active, err := viaDirect.direct.List(ctx, taskDefinition)
	if err != nil {
		t.Fatal(err)
	}
	inactive, err := viaDirect.direct.Probe(ctx, taskDefinition)
	if err != nil {
		t.Fatal(err)
	}
	const n = 10
	active, inactive = active[:min(n, len(active))], inactive[:min(n, len(inactive))]

	var directTime, ccTime time.Duration
	read := func(c *Client, id string, spent *time.Duration) (map[string]any, bool) {
		start := time.Now()
		props, found, err := c.GetResource(ctx, taskDefinition, id)
		*spent += time.Since(start)
		if err != nil {
			t.Fatalf("GetResource: %v", err)
		}
		return props, found
	}
	for _, id := range active {
		d, dFound := read(viaDirect, id, &directTime)
		c, cFound := read(viaCC, id, &ccTime)
		if !dFound || !cFound {
			t.Fatalf("an active revision: found directly %v, through Cloud Control %v", dFound, cFound)
		}
		diffs, err := direct.Compare(taskDefinition, c, d)
		if err != nil {
			t.Fatal(err)
		}
		for _, diff := range diffs {
			// Printed to this run's log only.
			t.Errorf("%s differs", diff.Property)
		}
	}
	for _, id := range inactive {
		_, dFound := read(viaDirect, id, &directTime)
		_, cFound := read(viaCC, id, &ccTime)
		if dFound || cFound {
			t.Fatalf("a deregistered revision: found directly %v, through Cloud Control %v", dFound, cFound)
		}
	}
	t.Logf("%d active and %d deregistered revisions: direct %v, Cloud Control %v", len(active), len(inactive), directTime.Round(time.Millisecond), ccTime.Round(time.Millisecond))
}
