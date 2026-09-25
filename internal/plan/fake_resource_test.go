package plan

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/evatt-labs/kraai/internal/resource"
)

// fakeResource is a hand-written stand-in for a real provider adapter
// (internal/provider/cfresource, internal/provider/neonresource): the task
// explicitly allows go.uber.org/mock or a hand-written fake, and a fake
// gives direct control over per-name state/error and a way to observe
// concurrency (maxInFlight) that a generated mock's expectation API makes
// awkward to express.
//
// It also proves Plan never mutates: Create/Update/Delete each record that
// they were called at all, so a test can assert zero calls across a full
// run including a Replace outcome.
type fakeResource struct {
	states map[string]*resource.State
	errs   map[string]error
	// delay, when set, makes Get block briefly so a concurrency test can
	// observe overlap instead of every call finishing before the next
	// starts.
	delay time.Duration

	getCalls    int32
	inFlight    int32
	maxInFlight int32
	createCalls int32
	updateCalls int32
	deleteCalls int32
}

func newFakeResource() *fakeResource {
	return &fakeResource{states: map[string]*resource.State{}, errs: map[string]error{}}
}

func (f *fakeResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	atomic.AddInt32(&f.getCalls, 1)

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

	if err, ok := f.errs[ref.Name]; ok {
		return nil, err
	}
	return f.states[ref.Name], nil
}

func (f *fakeResource) Create(context.Context, resource.Spec) (*resource.State, error) {
	atomic.AddInt32(&f.createCalls, 1)
	return nil, nil
}

func (f *fakeResource) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	atomic.AddInt32(&f.updateCalls, 1)
	return nil, nil
}

func (f *fakeResource) Delete(context.Context, resource.Ref) error {
	atomic.AddInt32(&f.deleteCalls, 1)
	return nil
}

func (f *fakeResource) mutatingCalls() int32 {
	return atomic.LoadInt32(&f.createCalls) + atomic.LoadInt32(&f.updateCalls) + atomic.LoadInt32(&f.deleteCalls)
}

// fakeDiffer wraps a fakeResource to also implement Differ, so tests can
// exercise the Replace and Update paths and the "comparison itself failed"
// path without every fakeResource needing an opinion on drift it doesn't
// have — mirroring why Differ is optional in the first place.
type fakeDiffer struct {
	*fakeResource
	diff func(spec resource.Spec, state *resource.State) (resource.Difference, error)
}

func (f *fakeDiffer) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	return f.diff(spec, state)
}

// fakeLiveDiffer implements LiveDiffer, and Differ too so a test can prove
// decide prefers the live one and never calls the plain one when both are
// present. diffLiveCalls and diffCalls count each separately.
type fakeLiveDiffer struct {
	*fakeResource
	diffLive func(ctx context.Context, spec resource.Spec, state *resource.State) (resource.Difference, error)

	diffLiveCalls int32
	diffCalls     int32
}

func (f *fakeLiveDiffer) DiffLive(ctx context.Context, spec resource.Spec, state *resource.State) (resource.Difference, error) {
	atomic.AddInt32(&f.diffLiveCalls, 1)
	return f.diffLive(ctx, spec, state)
}

// Diff must never run when DiffLive is present; it panics rather than
// returning a plausible answer, so a bug that reaches it fails loudly
// instead of merely planning the wrong action.
func (f *fakeLiveDiffer) Diff(resource.Spec, *resource.State) (resource.Difference, error) {
	atomic.AddInt32(&f.diffCalls, 1)
	panic("plan.decide must call LiveDiffer.DiffLive, never Differ.Diff, when a type implements both")
}

// fakeValidator wraps a fakeResource to also implement SpecValidator, so
// tests can exercise decide's unconditional validation pass — including on
// the ActionCreate path, which is the exact case that was broken before
// SpecValidator existed (see validate.go's own doc comment) — without every
// fakeResource needing an opinion on validity it doesn't have.
type fakeValidator struct {
	*fakeResource
	validate func(spec resource.Spec) error
}

func (f *fakeValidator) ValidateSpec(spec resource.Spec) error {
	return f.validate(spec)
}
