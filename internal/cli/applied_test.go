package cli

import (
	"context"
	"io"
	"reflect"
	"testing"

	"github.com/evatt-labs/kraai/internal/apply"
	"github.com/evatt-labs/kraai/internal/destroy"
	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

func appliedRef(name string) resource.Ref {
	return resource.Ref{Provider: "aws", Type: "AWS::Logs::LogGroup", Name: name}
}

// What a create, update or replace set is recorded; an unchanged, failed
// or skipped resource keeps what it had, and one recording nothing is
// left as it was.
func TestAppliedAfter(t *testing.T) {
	prior := map[string][]string{
		appliedRef("kept").InstanceKey():       {"A"},
		appliedRef("updated").InstanceKey():    {"A", "B"},
		appliedRef("failed").InstanceKey():     {"A"},
		appliedRef("unrecorded").InstanceKey(): {"A"},
	}
	result := &apply.Result{Results: []apply.ActionResult{
		{Ref: appliedRef("kept"), Outcome: apply.OutcomeUnchanged},
		{Ref: appliedRef("updated"), Outcome: apply.OutcomeUpdated, Applied: []string{"A"}},
		{Ref: appliedRef("created"), Outcome: apply.OutcomeCreated, Applied: []string{"C"}},
		{Ref: appliedRef("failed"), Outcome: apply.OutcomeFailed},
		{Ref: appliedRef("unrecorded"), Outcome: apply.OutcomeUpdated},
	}}
	want := map[string][]string{
		appliedRef("kept").InstanceKey():       {"A"},
		appliedRef("updated").InstanceKey():    {"A"},
		appliedRef("created").InstanceKey():    {"C"},
		appliedRef("failed").InstanceKey():     {"A"},
		appliedRef("unrecorded").InstanceKey(): {"A"},
	}
	if got := appliedAfter(prior, result); !reflect.DeepEqual(got, want) {
		t.Fatalf("appliedAfter = %v\nwant %v", got, want)
	}
	if prior[appliedRef("updated").InstanceKey()][1] != "B" {
		t.Fatal("the prior record was written")
	}
}

// A destroy that did not finish forgets what it deleted, and keeps the
// rest.
func TestForgetDeleted(t *testing.T) {
	store := lock.NewMemory()
	status := lock.Status{Environment: "env", Applied: map[string][]string{
		appliedRef("gone").InstanceKey(): {"A"}, appliedRef("left").InstanceKey(): {"B"},
	}}
	if err := store.WriteStatus(t.Context(), status); err != nil {
		t.Fatal(err)
	}
	result := &destroy.Result{Results: []destroy.ActionResult{
		{Ref: appliedRef("gone"), Outcome: destroy.OutcomeDeleted},
		{Ref: appliedRef("left"), Outcome: destroy.OutcomeFailed},
	}}
	if err := forgetDeleted(t.Context(), store, "env", result); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.ReadStatus(t.Context(), "env")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{appliedRef("left").InstanceKey(): {"B"}}; !reflect.DeepEqual(got.Applied, want) {
		t.Fatalf("Applied = %v, want %v", got.Applied, want)
	}
}

// A plan reads the record of what was applied onto its context, and so
// does the lock for apply and destroy.
func TestWithAdoptionCarriesTheAppliedRecord(t *testing.T) {
	store := lock.NewMemory()
	applied := map[string][]string{appliedRef("g").InstanceKey(): {"RetentionInDays"}}
	if err := store.WriteStatus(t.Context(), lock.Status{Environment: "env", Applied: applied}); err != nil {
		t.Fatal(err)
	}
	ctx, err := withAdoption(t.Context(), store, "env")
	if err != nil {
		t.Fatal(err)
	}
	if got := resource.AppliedFrom(ctx); !reflect.DeepEqual(got, applied) {
		t.Fatalf("for plan, AppliedFrom = %v, want %v", got, applied)
	}
	stores := func(context.Context, *manifest.Manifest) (lock.Store, error) { return store, nil }
	locked, _, release, err := guard(t.Context(), io.Discard, "env", &manifest.Manifest{}, stores)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if got := resource.AppliedFrom(locked); !reflect.DeepEqual(got, applied) {
		t.Fatalf("under the lock, AppliedFrom = %v, want %v", got, applied)
	}
}

// Fingerprints follow what a create, update or replace sent: replaced when
// it sent some, dropped when it sent none, kept otherwise.
func TestFingerprintsAfter(t *testing.T) {
	prior := map[string]map[string]string{
		appliedRef("kept").InstanceKey():    {"Body": "sha256:a"},
		appliedRef("updated").InstanceKey(): {"Body": "sha256:a"},
		appliedRef("cleared").InstanceKey(): {"Body": "sha256:a"},
	}
	result := &apply.Result{Results: []apply.ActionResult{
		{Ref: appliedRef("kept"), Outcome: apply.OutcomeUnchanged},
		{Ref: appliedRef("updated"), Outcome: apply.OutcomeUpdated, Applied: []string{"Body"}, Fingerprints: map[string]string{"Body": "sha256:b"}},
		{Ref: appliedRef("cleared"), Outcome: apply.OutcomeUpdated, Applied: []string{"Name"}},
	}}
	want := map[string]map[string]string{
		appliedRef("kept").InstanceKey():    {"Body": "sha256:a"},
		appliedRef("updated").InstanceKey(): {"Body": "sha256:b"},
	}
	if got := fingerprintsAfter(prior, result); !reflect.DeepEqual(got, want) {
		t.Fatalf("fingerprintsAfter = %v\nwant %v", got, want)
	}
}
