package aws

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
)

// gatedCC holds every read open until release is closed, so concurrent
// callers are genuinely in flight together, and counts what reached it.
type gatedCC struct {
	cloudControlAPI
	release chan struct{}
	gets    atomic.Int32
	lists   atomic.Int32
	fail    atomic.Bool
}

func (g *gatedCC) GetResource(context.Context, *cloudcontrol.GetResourceInput, ...func(*cloudcontrol.Options)) (*cloudcontrol.GetResourceOutput, error) {
	g.gets.Add(1)
	<-g.release
	if g.fail.Load() {
		return nil, errors.New("throttled")
	}
	return &cloudcontrol.GetResourceOutput{ResourceDescription: &cctypes.ResourceDescription{
		Identifier: aws.String("subnet-1"), Properties: aws.String(`{"SubnetId":"subnet-1"}`),
	}}, nil
}

func (g *gatedCC) ListResources(context.Context, *cloudcontrol.ListResourcesInput, ...func(*cloudcontrol.Options)) (*cloudcontrol.ListResourcesOutput, error) {
	g.lists.Add(1)
	<-g.release
	if g.fail.Load() {
		return nil, errors.New("throttled")
	}
	return &cloudcontrol.ListResourcesOutput{ResourceDescriptions: []cctypes.ResourceDescription{{Identifier: aws.String("subnet-1")}}}, nil
}

// runConcurrently starts n calls of read together, waits until the fake has
// seen a request start, gives the rest time to join it, then lets every
// request finish. The settle only has to be long enough for goroutines that
// are already running to reach the read; a read that coalesces nothing
// sends n requests however long it is.
func runConcurrently(t *testing.T, g *gatedCC, n int, started func() int32, read func()) {
	t.Helper()
	var ready, done sync.WaitGroup
	gate := make(chan struct{})
	for range n {
		ready.Add(1)
		done.Go(func() {
			ready.Done()
			<-gate
			read()
		})
	}
	ready.Wait()
	close(gate)
	for started() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(g.release)
	done.Wait()
}

// A wave resolves several registrations of one type at once, and every one
// misses the cache together. They must share one request per read, or the
// burst is what Cloud Control throttles.
func TestConcurrentReadsShareOneRequest(t *testing.T) {
	g := &gatedCC{release: make(chan struct{})}
	c := &Client{cc: g}

	maps := make([]map[string]any, 16)
	var i atomic.Int32
	runConcurrently(t, g, 16, g.gets.Load, func() {
		props, found, err := c.GetResource(t.Context(), TypeSubnet, "subnet-1")
		if err != nil || !found {
			t.Error(err)
			return
		}
		maps[i.Add(1)-1] = props
	})
	if got := g.gets.Load(); got != 1 {
		t.Fatalf("16 concurrent reads sent %d GetResource requests, want 1", got)
	}
	maps[0]["SubnetId"] = "mutated"
	if maps[1]["SubnetId"] != "subnet-1" {
		t.Fatal("callers joined on one read share a map")
	}

	g2 := &gatedCC{release: make(chan struct{})}
	c2 := &Client{cc: g2}
	runConcurrently(t, g2, 16, g2.lists.Load, func() {
		ids, err := c2.ListResources(t.Context(), TypeSubnet, nil)
		if err != nil || len(ids) != 1 {
			t.Error(ids, err)
		}
	})
	if got := g2.lists.Load(); got != 1 {
		t.Fatalf("16 concurrent lists sent %d ListResources requests, want 1", got)
	}
}

// A failed read is shared by the callers waiting on it, and not
// remembered: the next read asks again.
func TestAFailedSharedReadIsNotRemembered(t *testing.T) {
	g := &gatedCC{release: make(chan struct{})}
	g.fail.Store(true)
	c := &Client{cc: g}
	runConcurrently(t, g, 4, g.gets.Load, func() {
		if _, _, err := c.GetResource(t.Context(), TypeSubnet, "subnet-1"); err == nil {
			t.Error("a failed read reported success")
		}
	})
	g.fail.Store(false)
	if _, found, err := c.GetResource(t.Context(), TypeSubnet, "subnet-1"); err != nil || !found {
		t.Fatalf("the read after a failure = %v, %v", found, err)
	}
	if got := g.gets.Load(); got != 2 {
		t.Fatalf("GetResource requests = %d, want the failure then one retry", got)
	}
}
