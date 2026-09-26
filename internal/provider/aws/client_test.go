package aws

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
)

// testPollTimings makes the poll loop resolve near-instantly: a
// microsecond initial/max delay and a timeout generous enough for a
// handful of iterations, but never so long that a genuinely stuck test
// makes the suite hang.
func testPollTimings() Option {
	return WithPollTimings(time.Microsecond, time.Microsecond, 200*time.Millisecond)
}

// fakeCC is a hand-rolled cloudControlAPI: no AWS account, no network needed.
type fakeCC struct {
	getOut  *cloudcontrol.GetResourceOutput
	getErr  error
	listOut []*cloudcontrol.ListResourcesOutput
	listErr error
	listAt  int
	gotReq  []*cloudcontrol.GetResourceInput
	listReq []*cloudcontrol.ListResourcesInput

	createOut *cloudcontrol.CreateResourceOutput
	createErr error
	createReq []*cloudcontrol.CreateResourceInput

	updateOut *cloudcontrol.UpdateResourceOutput
	updateErr error
	updateReq []*cloudcontrol.UpdateResourceInput

	deleteOut *cloudcontrol.DeleteResourceOutput
	deleteErr error
	deleteReq []*cloudcontrol.DeleteResourceInput

	// statusOut is a sequence of responses returned in order, one per
	// GetResourceRequestStatus call, so a test can script
	// PENDING -> IN_PROGRESS -> SUCCESS/FAILED. The last entry repeats once
	// exhausted, the same pattern listOut/listAt already use.
	statusOut   []*cloudcontrol.GetResourceRequestStatusOutput
	statusErr   error
	statusAt    int
	statusCalls int
}

func (f *fakeCC) GetResource(_ context.Context, params *cloudcontrol.GetResourceInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.GetResourceOutput, error) {
	f.gotReq = append(f.gotReq, params)
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getOut, nil
}

func (f *fakeCC) ListResources(_ context.Context, params *cloudcontrol.ListResourcesInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.ListResourcesOutput, error) {
	f.listReq = append(f.listReq, params)
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := f.listOut[f.listAt]
	if f.listAt < len(f.listOut)-1 {
		f.listAt++
	}
	return out, nil
}

func (f *fakeCC) CreateResource(_ context.Context, params *cloudcontrol.CreateResourceInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.CreateResourceOutput, error) {
	f.createReq = append(f.createReq, params)
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createOut, nil
}

func (f *fakeCC) UpdateResource(_ context.Context, params *cloudcontrol.UpdateResourceInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.UpdateResourceOutput, error) {
	f.updateReq = append(f.updateReq, params)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updateOut, nil
}

func (f *fakeCC) DeleteResource(_ context.Context, params *cloudcontrol.DeleteResourceInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.DeleteResourceOutput, error) {
	f.deleteReq = append(f.deleteReq, params)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return f.deleteOut, nil
}

func (f *fakeCC) GetResourceRequestStatus(ctx context.Context, _ *cloudcontrol.GetResourceRequestStatusInput, _ ...func(*cloudcontrol.Options)) (*cloudcontrol.GetResourceRequestStatusOutput, error) {
	f.statusCalls++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if len(f.statusOut) == 0 {
		return &cloudcontrol.GetResourceRequestStatusOutput{}, nil
	}
	out := f.statusOut[f.statusAt]
	if f.statusAt < len(f.statusOut)-1 {
		f.statusAt++
	}
	// Give a stuck-forever test (no terminal status ever scripted) a chance
	// to observe context cancellation instead of spinning until the
	// suite's own test timeout.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, nil
}

type fakeCF struct {
	out *cloudformation.DescribeTypeOutput
	err error
}

func (f *fakeCF) DescribeType(context.Context, *cloudformation.DescribeTypeInput, ...func(*cloudformation.Options)) (*cloudformation.DescribeTypeOutput, error) {
	return f.out, f.err
}

func TestNew(t *testing.T) {
	t.Run("applies options over the default SDK-backed clients", func(t *testing.T) {
		cc := &fakeCC{getOut: &cloudcontrol.GetResourceOutput{
			ResourceDescription: &cctypes.ResourceDescription{Properties: aws.String(`{"ok":true}`)},
		}}
		cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{Schema: aws.String(`{}`)}}

		c, err := New(context.Background(), Settings{Region: "us-east-1"}, WithCloudControlAPI(cc), WithCloudFormationAPI(cf))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, _, err := c.GetResource(context.Background(), TypeS3Bucket, "x"); err != nil {
			t.Fatalf("GetResource through the injected fake: %v", err)
		}
		if _, err := c.DescribeType(context.Background(), TypeS3Bucket); err != nil {
			t.Fatalf("DescribeType through the injected fake: %v", err)
		}
	})

	t.Run("WithS3API and WithSTSAPI substitute their clients", func(t *testing.T) {
		fs3 := &fakeS3{}
		fsts := &fakeSTS{account: "123456789012"}

		c, err := New(context.Background(), Settings{Region: "us-east-1"}, WithS3API(fs3), WithSTSAPI(fsts))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := c.PutObject(context.Background(), "b", "k", []byte("x")); err != nil {
			t.Fatalf("PutObject through the injected fake: %v", err)
		}
		if _, err := c.AccountID(context.Background()); err != nil {
			t.Fatalf("AccountID through the injected fake: %v", err)
		}
	})

	t.Run("empty region defers to the SDK's own default chain instead of failing", func(t *testing.T) {
		// No manifest region, and nothing to force LoadDefaultConfig itself
		// to fail (unlike the malformed-shared-config subtest below): this
		// proves settings.Region == "" is a legitimate "let the SDK decide"
		// signal, not an error condition New has to reject.
		if _, err := New(context.Background(), Settings{}); err != nil {
			t.Fatalf("New with an empty region: %v", err)
		}
	})

	t.Run("a config load failure is reported, not swallowed", func(t *testing.T) {
		// A malformed shared config file is a deterministic way to make the
		// SDK's own LoadDefaultConfig fail without touching the network or
		// real credentials.
		dir := t.TempDir()
		badConfig := filepath.Join(dir, "config")
		if err := os.WriteFile(badConfig, []byte("[profile broken\nkey = value with no closing bracket above"), 0o600); err != nil {
			t.Fatalf("writing fixture: %v", err)
		}
		t.Setenv("AWS_CONFIG_FILE", badConfig)
		t.Setenv("AWS_SDK_LOAD_CONFIG", "1")
		t.Setenv("AWS_PROFILE", "broken")

		if _, err := New(context.Background(), Settings{Region: "us-east-1"}); err == nil {
			t.Fatal("expected a config-loading error to be reported")
		}
	})
}

func TestClientRegion(t *testing.T) {
	c := &Client{region: "us-west-2"}
	if got := c.Region(); got != "us-west-2" {
		t.Fatalf("Region() = %q, want %q", got, "us-west-2")
	}
}

func TestClientPollTimingsDefaults(t *testing.T) {
	// A Client built by struct literal (every existing test in this
	// package does exactly that) has zero-value poll fields; pollTimings
	// must fall back to the production defaults rather than let the poll
	// loop busy-spin with no delay at all.
	c := &Client{}
	initial, maxDelay, timeout := c.pollTimings()
	if initial != defaultPollInitialDelay || maxDelay != defaultPollMaxDelay || timeout != defaultPollTimeout {
		t.Fatalf("pollTimings() = (%v, %v, %v), want the defaults", initial, maxDelay, timeout)
	}
}
