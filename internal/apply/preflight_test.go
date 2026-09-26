package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// --- pre-flight gate ---

func TestApply_PreflightRefusesOnActionFailed_ZeroCalls(t *testing.T) {
	db := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	failed := action("api", "DB", "neon", "branch", 0, plan.ActionFailed)
	failed.Err = errors.New("get failed")
	ok := action("api", "OTHER", "neon", "branch", 0, plan.ActionCreate)

	p := &plan.Plan{Actions: []plan.Action{failed, ok}}

	result, err := New(reg).Apply(context.Background(), p)
	if result != nil {
		t.Fatalf("Result = %+v, want nil: nothing should be executed", result)
	}
	requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(err.Error(), "api.DB") {
		t.Errorf("error = %q, want it to name the offending binding", err.Error())
	}
	if db.createCalls != 0 || db.deleteCalls != 0 || db.getCalls != 0 {
		t.Errorf("provider calls = create:%d delete:%d get:%d, want zero: pre-flight must refuse before any mutation",
			db.createCalls, db.deleteCalls, db.getCalls)
	}
}

func TestApply_PreflightRefusesOnActionReplaceWithoutFlag_ZeroCalls(t *testing.T) {
	db := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	replace := action("api", "DB", "neon", "branch", 0, plan.ActionReplace)
	p := &plan.Plan{Actions: []plan.Action{replace}}

	result, err := New(reg).Apply(context.Background(), p) // allowReplace defaults to false
	if result != nil {
		t.Fatalf("Result = %+v, want nil", result)
	}
	requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(err.Error(), "--replace") {
		t.Errorf("error = %q, want it to mention --replace", err.Error())
	}
	if db.createCalls != 0 || db.deleteCalls != 0 {
		t.Errorf("provider calls = create:%d delete:%d, want zero", db.createCalls, db.deleteCalls)
	}
}

func TestApply_AllowReplace_DeletesThenCreates(t *testing.T) {
	db := newFakeResource()
	db.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}, ID: "new"}
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	replace := action("api", "DB", "neon", "branch", 0, plan.ActionReplace)
	p := &plan.Plan{Actions: []plan.Action{replace}}

	result, err := New(reg, WithAllowReplace(true)).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeReplaced {
		t.Errorf("Outcome = %v, want OutcomeReplaced", r.Outcome)
	}
	if db.deleteCalls != 1 || db.createCalls != 1 {
		t.Errorf("delete=%d create=%d, want exactly one of each", db.deleteCalls, db.createCalls)
	}
	if db.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0: replace must never call Update", db.updateCalls)
	}
}

func TestApply_Replace_DeleteFailureNeverCallsCreate(t *testing.T) {
	db := newFakeResource()
	db.deleteErr = errors.New("delete boom")
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	replace := action("api", "DB", "neon", "branch", 0, plan.ActionReplace)
	p := &plan.Plan{Actions: []plan.Action{replace}}

	result, err := New(reg, WithAllowReplace(true)).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeFailed || r.Err == nil {
		t.Errorf("result = %+v, want OutcomeFailed with an error", r)
	}
	if db.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0: a failed delete must never be followed by create", db.createCalls)
	}
}
