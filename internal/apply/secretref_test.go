package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
	"github.com/evatt-labs/kraai/internal/secretref"
)

// TestApply_SecretRefFailureRefusesBeforeAnyMutation is the fail-before-
// mutate property: a secret reference that fails to resolve aborts the
// whole run before wave 0 starts, exactly like the pre-flight gate's own
// ActionFailed and unpermitted-replace cases above. A sibling action with
// no secret reference at all must still see zero calls: the failure is
// global to the run, not scoped to the action that declared the reference.
func TestApply_SecretRefFailureRefusesBeforeAnyMutation(t *testing.T) {
	boom := errors.New("access denied")
	refFailure := &fakeSecretRefResource{
		fakeResource: newFakeResource(),
		refs:         []secretref.Ref{{Scheme: "aws-ssm", Path: "/a"}},
		resolve:      func(secretref.Ref) (resource.Secret, error) { return nil, boom },
	}
	sibling := newFakeResource()

	reg := newRegistry(t,
		resource.Registration{Provider: "aws", Type: "fn", Capability: "compute", Lookup: resource.LookupByName, Resource: refFailure},
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database", Lookup: resource.LookupByName, Resource: sibling},
	)

	refAction := action("api", "API", "aws", "fn", 0, plan.ActionCreate)
	siblingAction := action("api", "DB", "neon", "branch", 0, plan.ActionCreate)
	p := &plan.Plan{Actions: []plan.Action{refAction, siblingAction}}

	result, err := New(reg).Apply(context.Background(), p)
	if result != nil {
		t.Fatalf("Result = %+v, want nil: nothing should be executed", result)
	}
	if err == nil {
		t.Fatal("Apply error = nil, want the secret reference failure")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the resolver's own failure", err)
	}
	if refFailure.createCalls != 0 || sibling.createCalls != 0 {
		t.Errorf("create calls = ref:%d sibling:%d, want zero: a secret reference failure must refuse before any mutation",
			refFailure.createCalls, sibling.createCalls)
	}
}

// TestApply_SecretRefResolvedOnceAcrossActions proves resolveSecretRefs
// deduplicates by reference: two actions naming the same reference resolve
// it once between them, not once each, so a manifest with the same
// GITHUB_CLIENT_SECRET ref on two functions does not double the calls to
// the secret store.
func TestApply_SecretRefResolvedOnceAcrossActions(t *testing.T) {
	ref := secretref.Ref{Scheme: "aws-ssm", Path: "/shared"}
	first := &fakeSecretRefResource{fakeResource: newFakeResource(), refs: []secretref.Ref{ref}}
	second := &fakeSecretRefResource{fakeResource: newFakeResource(), refs: []secretref.Ref{ref}}

	reg := newRegistry(t,
		resource.Registration{Provider: "aws", Type: "fn-a", Capability: "compute", Lookup: resource.LookupByName, Resource: first},
		resource.Registration{Provider: "aws", Type: "fn-b", Capability: "compute", Lookup: resource.LookupByName, Resource: second},
	)

	a := action("api", "A", "aws", "fn-a", 0, plan.ActionCreate)
	b := action("api", "B", "aws", "fn-b", 0, plan.ActionCreate)
	p := &plan.Plan{Actions: []plan.Action{a, b}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	total := first.resolveCalls + second.resolveCalls
	if total != 1 {
		t.Errorf("ResolveSecretRef was called %d times across both actions, want exactly 1", total)
	}
}

// TestApply_NoSecretRefResolverIsFine proves an ordinary resource — one
// that does not implement resource.SecretRefResolver — applies exactly as
// it did before this feature existed: resolveSecretRefs must not require
// every registered type to answer for secret references.
func TestApply_NoSecretRefResolverIsFine(t *testing.T) {
	db := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})
	p := &plan.Plan{Actions: []plan.Action{action("api", "DB", "neon", "branch", 0, plan.ActionCreate)}}

	result, err := New(reg).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(result.Results) != 1 || result.Results[0].Outcome != OutcomeCreated {
		t.Fatalf("Result = %+v, want one OutcomeCreated", result)
	}
}

// TestApply_SecretRefsErrorRefusesBeforeAnyMutation covers the case where
// naming a resource's own references fails — a malformed envSecrets entry
// decodeLambdaSettings would normally have already caught at plan time, but
// apply must not trust that and call SecretRefs unguarded.
func TestApply_SecretRefsErrorRefusesBeforeAnyMutation(t *testing.T) {
	boom := errors.New("malformed settings")
	refFailure := &fakeSecretRefResource{fakeResource: newFakeResource(), refsErr: boom}
	reg := newRegistry(t, resource.Registration{
		Provider: "aws", Type: "fn", Capability: "compute", Lookup: resource.LookupByName, Resource: refFailure,
	})
	p := &plan.Plan{Actions: []plan.Action{action("api", "API", "aws", "fn", 0, plan.ActionCreate)}}

	_, err := New(reg).Apply(context.Background(), p)
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap SecretRefs' own failure", err)
	}
	if refFailure.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", refFailure.createCalls)
	}
}

// TestApply_ProducerFailureRefusesBeforeAnyMutation covers the case where
// ResolveSecretRef itself succeeds (the reference is well-formed and a
// backend is registered for its scheme) but calling the producer it
// returns — the actual provider read — fails.
func TestApply_ProducerFailureRefusesBeforeAnyMutation(t *testing.T) {
	boom := errors.New("parameter not found")
	refFailure := &fakeSecretRefResource{
		fakeResource: newFakeResource(),
		refs:         []secretref.Ref{{Scheme: "aws-ssm", Path: "/a"}},
		resolve: func(secretref.Ref) (resource.Secret, error) {
			return func(context.Context) (string, error) { return "", boom }, nil
		},
	}
	reg := newRegistry(t, resource.Registration{
		Provider: "aws", Type: "fn", Capability: "compute", Lookup: resource.LookupByName, Resource: refFailure,
	})
	p := &plan.Plan{Actions: []plan.Action{action("api", "API", "aws", "fn", 0, plan.ActionCreate)}}

	_, err := New(reg).Apply(context.Background(), p)
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the producer's own failure", err)
	}
	if refFailure.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", refFailure.createCalls)
	}
}

// TestApply_SecretRefFailureIsValidationCode proves the pre-flight-style
// refusal surfaces as kerrors.CodeValidation, matching preflight's own
// ActionFailed and unpermitted-replace refusals above, so cmd/kraai exits
// the same way for every "refused before touching anything" reason.
func TestApply_SecretRefFailureIsValidationCode(t *testing.T) {
	refFailure := &fakeSecretRefResource{
		fakeResource: newFakeResource(),
		refs:         []secretref.Ref{{Scheme: "aws-ssm", Path: "/a"}},
		resolve:      func(secretref.Ref) (resource.Secret, error) { return nil, kerrors.Validation("no such parameter") },
	}
	reg := newRegistry(t, resource.Registration{
		Provider: "aws", Type: "fn", Capability: "compute", Lookup: resource.LookupByName, Resource: refFailure,
	})
	p := &plan.Plan{Actions: []plan.Action{action("api", "API", "aws", "fn", 0, plan.ActionCreate)}}

	_, err := New(reg).Apply(context.Background(), p)
	requireCode(t, err, kerrors.CodeValidation)
}
