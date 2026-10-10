package destroy

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/evatt-labs/kraai/internal/resource"
)

// fakeResource is a hand-written stand-in for a real provider adapter,
// mirroring internal/apply/fake_resource_test.go's fakeResource: a fake
// gives direct control over per-call results/errors and a way to observe
// concurrency (maxInFlight) and call order (Delete's own counters) that a
// generated mock's expectation API makes awkward to express.
//
// Get and Create are never called by destroy (see the package doc: destroy
// resolves what to do from the plan's already-decided Action, never by
// reading or creating state itself) but a fakeResource still has to
// satisfy resource.Resource to be registered. Tests assert getCalls and
// createCalls stay zero to prove that.
type fakeResource struct {
	deleteErr error
	// note, when set, is what Delete tells the operator.
	note string

	// delay, when set, makes Delete block briefly so a concurrency test can
	// observe overlap instead of every call finishing before the next
	// starts.
	delay time.Duration

	getCalls    int32
	createCalls int32
	updateCalls int32
	deleteCalls int32
	inFlight    int32
	maxInFlight int32
}

func newFakeResource() *fakeResource {
	return &fakeResource{}
}

func (f *fakeResource) Get(context.Context, resource.Ref) (*resource.State, error) {
	atomic.AddInt32(&f.getCalls, 1)
	return nil, nil
}

func (f *fakeResource) Create(context.Context, resource.Spec) (*resource.State, error) {
	atomic.AddInt32(&f.createCalls, 1)
	return nil, nil
}

func (f *fakeResource) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	atomic.AddInt32(&f.updateCalls, 1)
	return nil, resource.ErrImmutable
}

func (f *fakeResource) Delete(ctx context.Context, _ resource.Ref) error {
	atomic.AddInt32(&f.deleteCalls, 1)
	if f.note != "" {
		resource.Note(ctx, f.note)
	}

	cur := atomic.AddInt32(&f.inFlight, 1)
	defer atomic.AddInt32(&f.inFlight, -1)
	for {
		prevMax := atomic.LoadInt32(&f.maxInFlight)
		if cur <= prevMax {
			break
		}
		if atomic.CompareAndSwapInt32(&f.maxInFlight, prevMax, cur) {
			break
		}
	}

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return f.deleteErr
}
