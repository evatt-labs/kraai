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

// The same comparison for target groups, whose read takes four calls, and
// an identifier that does not exist.
//
//	go test -tags integration ./internal/provider/aws -run TestDirectGetResourceLiveTargetGroup -v
func TestDirectGetResourceLiveTargetGroup(t *testing.T) {
	const targetGroup = "AWS::ElasticLoadBalancingV2::TargetGroup"
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
	ids, err := viaCC.ListResources(ctx, targetGroup, nil)
	if err != nil {
		t.Fatal(err)
	}
	var directTime, ccTime time.Duration
	for _, id := range ids {
		start := time.Now()
		d, dFound, err := viaDirect.GetResource(ctx, targetGroup, id)
		directTime += time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		c, cFound, err := viaCC.GetResource(ctx, targetGroup, id)
		ccTime += time.Since(start)
		if err != nil || !dFound || !cFound {
			t.Fatalf("found directly %v, through Cloud Control %v, %v", dFound, cFound, err)
		}
		diffs, err := direct.Compare(targetGroup, c, d)
		if err != nil {
			t.Fatal(err)
		}
		for _, diff := range diffs {
			t.Errorf("%s differs", diff.Property)
		}
	}
	absent := "arn:aws:elasticloadbalancing:us-east-1:" + viaDirect.accountForTest(ctx, t) + ":targetgroup/kraai-absent-probe/0123456789abcdef"
	if _, found, err := viaDirect.GetResource(ctx, targetGroup, absent); err != nil || found {
		t.Fatalf("an absent target group: found %v, %v", found, err)
	}
	t.Logf("%d target groups: direct %v, Cloud Control %v", len(ids), directTime.Round(time.Millisecond), ccTime.Round(time.Millisecond))
}

func (c *Client) accountForTest(ctx context.Context, t *testing.T) string {
	t.Helper()
	out, err := c.sts.GetCallerIdentity(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return *out.Account
}
