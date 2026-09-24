package apply

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evatt-labs/kraai/internal/resource"
	"github.com/evatt-labs/kraai/internal/secretref"
)

// fakeResource is a hand-written stand-in for a real provider adapter,
// mirroring internal/plan/fake_resource_test.go's fakeResource: a fake
// gives direct control over per-call results/errors and a way to observe
// concurrency (maxInFlight) that a generated mock's expectation API makes
// awkward to express, which is exactly what the bounded-concurrency and
// sibling-survives-a-failure tests below need.
type fakeResource struct {
	createState *resource.State
	createErr   error
	deleteErr   error

	// delay, when set, makes Create block briefly so a concurrency test can
	// observe overlap instead of every call finishing before the next
	// starts.
	delay time.Duration

	getCalls    int32
	createCalls int32
	updateCalls int32
	deleteCalls int32
	// updateState, when set, is what Update returns. Nil keeps the fake's
	// historical answer — ErrImmutable, "this cannot be updated" — which
	// every replace test relies on to prove Update was never the path taken.
	updateState *resource.State
	inFlight    int32
	maxInFlight int32

	// lastSpec captures the Spec passed to the most recent Create call, so
	// a test can assert what secrets apply actually wired in. Guarded by
	// specMu rather than left to the same lock-free approach as the
	// counters above: a struct value can't be updated atomically, and a
	// concurrency test genuinely calls Create from multiple goroutines
	// against the one shared fakeResource.
	specMu   sync.Mutex
	lastSpec resource.Spec
}

// LastSpec returns the Spec passed to the most recent Create call.
func (f *fakeResource) LastSpec() resource.Spec {
	f.specMu.Lock()
	defer f.specMu.Unlock()
	return f.lastSpec
}

func newFakeResource() *fakeResource {
	return &fakeResource{}
}

// Get is never called by apply (see the package doc: apply resolves what
// to do from the plan's already-decided Action, not by reading state
// again), but a fakeResource still has to satisfy resource.Resource to be
// registered. Tests assert getCalls stays zero to prove that.
func (f *fakeResource) Get(context.Context, resource.Ref) (*resource.State, error) {
	atomic.AddInt32(&f.getCalls, 1)
	return nil, nil
}

func (f *fakeResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	atomic.AddInt32(&f.createCalls, 1)
	f.specMu.Lock()
	f.lastSpec = spec
	f.specMu.Unlock()

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
			return nil, ctx.Err()
		}
	}

	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createState, nil
}

func (f *fakeResource) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	atomic.AddInt32(&f.updateCalls, 1)
	if f.updateState != nil {
		return f.updateState, nil
	}
	return nil, resource.ErrImmutable
}

func (f *fakeResource) Delete(context.Context, resource.Ref) error {
	atomic.AddInt32(&f.deleteCalls, 1)
	return f.deleteErr
}

// fakeSecretResource wraps a fakeResource to also implement
// resource.SecretProducer, mirroring internal/plan/fake_resource_test.go's
// fakeDiffer: an optional interface should not force every fake to have an
// opinion on it.
type fakeSecretResource struct {
	*fakeResource
	secretsFn func(state *resource.State) map[string]resource.Secret
}

func (f *fakeSecretResource) Secrets(state *resource.State) map[string]resource.Secret {
	return f.secretsFn(state)
}

// fakeSecretRefResource wraps a fakeResource to also implement
// resource.SecretRefResolver, mirroring fakeSecretResource above: an
// optional interface a fake should not be forced to have an opinion on.
type fakeSecretRefResource struct {
	*fakeResource
	refs []secretref.Ref
	// refsErr, when set, is what SecretRefs itself returns, before any
	// resolution is attempted.
	refsErr error
	// resolve, when set, builds the producer for a ref; the default
	// returns resolveErr from every producer, so a test that only cares
	// about the resolve-time failure need not set this.
	resolve func(ref secretref.Ref) (resource.Secret, error)
	// resolveCalls counts calls to ResolveSecretRef, so a test can prove a
	// ref already resolved is not resolved again within one apply.
	resolveCalls int32
}

func (f *fakeSecretRefResource) SecretRefs(resource.Spec) ([]secretref.Ref, error) {
	if f.refsErr != nil {
		return nil, f.refsErr
	}
	return f.refs, nil
}

func (f *fakeSecretRefResource) ResolveSecretRef(_ context.Context, ref secretref.Ref) (resource.Secret, error) {
	atomic.AddInt32(&f.resolveCalls, 1)
	if f.resolve != nil {
		return f.resolve(ref)
	}
	return func(context.Context) (string, error) { return "", nil }, nil
}
