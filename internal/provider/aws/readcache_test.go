package aws

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
)

// A plan reads the same resource once per lookup that could be it; Cloud
// Control hears about it once, and no two callers share the map.
func TestClientMemoizesReadsForTheProcess(t *testing.T) {
	cc := &fakeCC{getOut: &cloudcontrol.GetResourceOutput{
		ResourceDescription: &cctypes.ResourceDescription{
			Identifier: aws.String("subnet-1"),
			Properties: aws.String(`{"SubnetId":"subnet-1","CidrBlock":"10.90.1.0/25"}`),
		},
	}}
	c := &Client{cc: cc}

	first, found, err := c.GetResource(context.Background(), TypeSubnet, "subnet-1")
	if err != nil || !found {
		t.Fatalf("GetResource: found=%v err=%v", found, err)
	}
	first["CidrBlock"] = "mutated by a caller"
	second, found, err := c.GetResource(context.Background(), TypeSubnet, "subnet-1")
	if err != nil || !found {
		t.Fatalf("GetResource again: found=%v err=%v", found, err)
	}
	if second["CidrBlock"] != "10.90.1.0/25" {
		t.Fatalf("second read = %v, want Cloud Control's answer, not the first caller's mutation", second)
	}
	if len(cc.gotReq) != 1 {
		t.Fatalf("Cloud Control GetResource called %d times, want 1", len(cc.gotReq))
	}
}

func TestClientMemoizesAbsence(t *testing.T) {
	cc := &fakeCC{getErr: &cctypes.ResourceNotFoundException{Message: aws.String("gone")}}
	c := &Client{cc: cc}
	for range 3 {
		if _, found, err := c.GetResource(context.Background(), TypeSubnet, "missing"); err != nil || found {
			t.Fatalf("GetResource: found=%v err=%v; want absence", found, err)
		}
	}
	if len(cc.gotReq) != 1 {
		t.Fatalf("Cloud Control GetResource called %d times, want 1", len(cc.gotReq))
	}
}

// A throttled or failed read is not remembered: the next lookup asks again.
func TestClientDoesNotMemoizeAFailedRead(t *testing.T) {
	cc := &fakeCC{getErr: &cctypes.ThrottlingException{Message: aws.String("slow down")}}
	c := &Client{cc: cc}
	for range 2 {
		if _, _, err := c.GetResource(context.Background(), TypeSubnet, "subnet-1"); err == nil {
			t.Fatal("GetResource succeeded, want the throttling error")
		}
	}
	if len(cc.gotReq) != 2 {
		t.Fatalf("Cloud Control GetResource called %d times, want 2: a failure must not be cached", len(cc.gotReq))
	}
}

func TestClientMemoizesListsPerModel(t *testing.T) {
	cc := &fakeCC{listOut: []*cloudcontrol.ListResourcesOutput{{
		ResourceDescriptions: []cctypes.ResourceDescription{{Identifier: aws.String("subnet-1")}},
	}}}
	c := &Client{cc: cc}
	for range 3 {
		ids, err := c.ListResources(context.Background(), TypeSubnet, nil)
		if err != nil || len(ids) != 1 || ids[0] != "subnet-1" {
			t.Fatalf("ListResources = %v, %v", ids, err)
		}
	}
	if _, err := c.ListResources(context.Background(), TypeSubnet, map[string]any{"VpcId": "vpc-1"}); err != nil {
		t.Fatalf("ListResources(scoped): %v", err)
	}
	if len(cc.listReq) != 2 {
		t.Fatalf("Cloud Control ListResources called %d times, want 2: once unscoped, once for the scoped model", len(cc.listReq))
	}
}

// A mutation of a type forgets what was read of it, and nothing else: a
// read after a create reaches Cloud Control, a read of another type does
// not.
func TestClientForgetsATypeOnMutation(t *testing.T) {
	cache := &readCache{}
	cache.putGet(TypeSubnet, "subnet-1", `{}`, true)
	cache.putList(TypeSubnet, "", []string{"subnet-1"})
	cache.putGet(TypeVPC, "vpc-1", `{}`, true)

	cache.forget(TypeSubnet)
	if _, _, hit := cache.get(TypeSubnet, "subnet-1"); hit {
		t.Fatal("subnet read survived the type being forgotten")
	}
	if _, hit := cache.list(TypeSubnet, ""); hit {
		t.Fatal("subnet list survived the type being forgotten")
	}
	if _, _, hit := cache.get(TypeVPC, "vpc-1"); !hit {
		t.Fatal("VPC read was forgotten along with the subnet type")
	}
}

func TestClientDeleteForgetsTheType(t *testing.T) {
	cc := &fakeCC{
		getOut: &cloudcontrol.GetResourceOutput{ResourceDescription: &cctypes.ResourceDescription{
			Identifier: aws.String("subnet-1"), Properties: aws.String(`{"SubnetId":"subnet-1"}`),
		}},
		deleteOut: &cloudcontrol.DeleteResourceOutput{ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("token-1")}},
		statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{ProgressEvent: &cctypes.ProgressEvent{
			OperationStatus: cctypes.OperationStatusSuccess, Identifier: aws.String("subnet-1"),
		}}},
	}
	c := &Client{cc: cc}
	testPollTimings()(c)
	if _, _, err := c.GetResource(context.Background(), TypeSubnet, "subnet-1"); err != nil {
		t.Fatalf("GetResource: %v", err)
	}
	if err := c.DeleteResource(context.Background(), TypeSubnet, "subnet-1"); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if _, _, err := c.GetResource(context.Background(), TypeSubnet, "subnet-1"); err != nil {
		t.Fatalf("GetResource after delete: %v", err)
	}
	if len(cc.gotReq) != 2 {
		t.Fatalf("Cloud Control GetResource called %d times, want 2: the delete must forget the read", len(cc.gotReq))
	}
}

// Reads run concurrently within a plan wave; the memo must be safe under
// the race detector. The fake behind it is not, so it is warmed once
// serially and the goroutines then hit only the memo.
func TestClientReadsAreSafeConcurrently(t *testing.T) {
	cc := &fakeCC{
		getOut: &cloudcontrol.GetResourceOutput{ResourceDescription: &cctypes.ResourceDescription{
			Identifier: aws.String("subnet-1"), Properties: aws.String(`{"SubnetId":"subnet-1"}`),
		}},
		listOut: []*cloudcontrol.ListResourcesOutput{{ResourceDescriptions: []cctypes.ResourceDescription{{Identifier: aws.String("subnet-1")}}}},
	}
	c := &Client{cc: cc}
	if _, _, err := c.GetResource(context.Background(), TypeSubnet, "subnet-1"); err != nil {
		t.Fatalf("GetResource: %v", err)
	}
	if _, err := c.ListResources(context.Background(), TypeSubnet, nil); err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = c.GetResource(context.Background(), TypeSubnet, "subnet-1")
			_, _ = c.ListResources(context.Background(), TypeSubnet, nil)
		}()
	}
	wg.Wait()
	if len(cc.gotReq) != 1 || len(cc.listReq) != 1 {
		t.Fatalf("Cloud Control called %d gets and %d lists, want 1 each: every goroutine should have hit the memo", len(cc.gotReq), len(cc.listReq))
	}
}
