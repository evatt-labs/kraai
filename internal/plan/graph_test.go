package plan

import (
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// item builds a minimal plannedItem for graph_test.go's direct,
// white-box tests of computeWaves/buildGraph — no manifest, no registry,
// just the fields the graph actually reads: ServiceKey/Binding (the
// instance-scoping group), the item's own registry key (via ref), and
// dependsOn.
//
// DependsOn resolves only within an item's own (ServiceKey, Binding) group
// (see resource.Registration.DependsOn's own doc comment), so two items
// meant to be able to depend on each other in a test must share both —
// exactly like every registration Resolve returns for one manifest
// binding, or every compute registration expandCompute plans for one
// service (Binding == ServiceKey there).
func item(serviceKey, binding, providerType string, dependsOn ...string) plannedItem {
	provider, typ, _ := strings.Cut(providerType, "/")
	return plannedItem{
		Item:      Item{ServiceKey: serviceKey, Binding: binding, Provider: provider, Type: typ},
		ref:       resource.Ref{Provider: provider, Type: typ, Name: serviceKey + "-" + binding},
		dependsOn: dependsOn,
	}
}

// TestComputeWaves_LinearChain pins the base case: A depends on nothing, B
// depends on A, C depends on B — three waves, one item each.
func TestComputeWaves_LinearChain(t *testing.T) {
	// All three share one (ServiceKey, Binding) group — DependsOn only
	// resolves within the same group (see item's own doc comment), the
	// same way every registration Resolve returns for one manifest
	// binding shares a group in the real planner.
	items := []plannedItem{
		item("svc", "grp", "p/B", "p/A"),
		item("svc", "grp", "p/A"),
		item("svc", "grp", "p/C", "p/B"),
	}
	waves, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	want := map[string]int{"B": 1, "A": 0, "C": 2}
	for i, it := range items {
		if waves[i] != want[it.Type] {
			t.Errorf("%s wave = %d, want %d", it.Type, waves[i], want[it.Type])
		}
	}
}

// TestComputeWaves_Diamond pins the property computeWaves exists for: a
// node's wave is 1 + the LARGEST wave among its dependencies, not merely
// one more than any single one of them. D depends on both B and C; B and C
// both depend on A; if D's wave were computed from an arbitrary one of its
// two dependencies rather than their max, this would be flaky depending on
// iteration order.
func TestComputeWaves_Diamond(t *testing.T) {
	items := []plannedItem{
		item("svc", "grp", "p/A"),
		item("svc", "grp", "p/B", "p/A"),
		item("svc", "grp", "p/C", "p/A"),
		item("svc", "grp", "p/D", "p/B", "p/C"),
	}
	waves, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	byType := map[string]int{}
	for i, it := range items {
		byType[it.Type] = waves[i]
	}
	if byType["A"] != 0 || byType["B"] != 1 || byType["C"] != 1 || byType["D"] != 2 {
		t.Fatalf("waves = %+v, want A=0 B=1 C=1 D=2", byType)
	}
}

// TestComputeWaves_TypeEdgeScopedToGroup is resource.Registration.DependsOn's
// central instance-level guarantee: a dependency name is resolved only
// within the same (ServiceKey, Binding) group, never across the whole
// manifest. Two services each declare their own "function" and
// "permission"; permission depends on "function" in both, and each must
// resolve to its OWN service's function, not the other's — exactly the
// case a service's Lambda permission depending on "the function" has to
// get right when a manifest declares two services with compute.
func TestComputeWaves_TypeEdgeScopedToGroup(t *testing.T) {
	items := []plannedItem{
		item("api", "api", "aws/function"),
		item("api", "api", "aws/permission", "aws/function"),
		item("tick", "tick", "aws/function"),
		item("tick", "tick", "aws/permission", "aws/function"),
	}
	waves, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	for i, it := range items {
		want := 0
		if it.Type == "permission" {
			want = 1
		}
		if waves[i] != want {
			t.Errorf("%s/%s wave = %d, want %d", it.ServiceKey, it.Type, waves[i], want)
		}
	}
}

// TestComputeWaves_FilteredOutRegistrationContributesNoEdge pins the hard
// constraint: a dependency naming a type that was never planned for this
// group (because a registration's own conditions filtered it out before
// reaching expand) resolves to no edge at all, not an
// error and not a phantom wait. "b" depends on "missing", which no item in
// its group provides — it must still land in wave 0.
func TestComputeWaves_FilteredOutRegistrationContributesNoEdge(t *testing.T) {
	items := []plannedItem{
		item("svc", "grp", "p/A"),
		item("svc", "grp", "p/B", "p/Missing"),
	}
	waves, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	if waves[0] != 0 || waves[1] != 0 {
		t.Fatalf("waves = %v, want both 0: a dependency on an unplanned type must not block anything", waves)
	}
}

// TestComputeWaves_ServiceDependsOn pins the manifest-level escape hatch:
// every item in a service that depends_on another service waits for every
// item in that other service, even when nothing about the two services'
// own resource types relates them at all.
func TestComputeWaves_ServiceDependsOn(t *testing.T) {
	items := []plannedItem{
		item("frontend", "frontend", "p/worker"),
		item("backend", "backend", "p/worker"),
	}
	waves, err := computeWaves(items, map[string][]string{"frontend": {"backend"}})
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	if waves[1] != 0 {
		t.Fatalf("backend wave = %d, want 0", waves[1])
	}
	if waves[0] != 1 {
		t.Fatalf("frontend wave = %d, want 1: it depends_on backend", waves[0])
	}
}

// TestComputeWaves_CycleIsALoudNamedError is the other half of Kahn's
// well-known property this implementation leans on: a graph with a cycle
// cannot be fully sorted, and computeWaves must report that as an error
// naming the resources involved, never as a silent partial order or a
// panic.
func TestComputeWaves_CycleIsALoudNamedError(t *testing.T) {
	items := []plannedItem{
		item("svc", "grp", "p/A", "p/B"),
		item("svc", "grp", "p/B", "p/A"),
	}
	_, err := computeWaves(items, nil)
	if err == nil {
		t.Fatal("a cyclic graph resolved successfully")
	}
	msg := err.Error()
	for _, want := range []string{"cycle", "p/A", "p/B", "svc-grp"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// TestComputeWaves_CycleThroughServiceDependsOn pins that a cycle spanning
// a manifest-level depends_on edge is caught exactly like a type-edge
// cycle: A depends_on B and B depends_on A, both trivially, with only one
// item each.
func TestComputeWaves_CycleThroughServiceDependsOn(t *testing.T) {
	items := []plannedItem{
		item("a", "a", "p/worker"),
		item("b", "b", "p/worker"),
	}
	_, err := computeWaves(items, map[string][]string{"a": {"b"}, "b": {"a"}})
	if err == nil {
		t.Fatal("a service-level cycle resolved successfully")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("got %v, want an error naming a cycle", err)
	}
}

// TestComputeWaves_Deterministic pins that repeated calls against the same
// input always produce the same wave assignment — Kahn's layered algorithm
// derives a node's wave purely from the graph's shape (see computeWaves's
// own doc comment), never from map/slice iteration order, so this must
// hold regardless of how many times it runs.
func TestComputeWaves_Deterministic(t *testing.T) {
	items := []plannedItem{
		item("svc", "grp", "p/A"),
		item("svc", "grp", "p/B", "p/A"),
		item("svc", "grp", "p/C", "p/A"),
		item("svc", "grp", "p/D", "p/B", "p/C"),
		item("svc", "grp", "p/E"),
	}
	first, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	for i := range 50 {
		got, err := computeWaves(items, nil)
		if err != nil {
			t.Fatalf("computeWaves (run %d): %v", i, err)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d: wave[%d] = %d, want %d (first run)", i, j, got[j], first[j])
			}
		}
	}
}

// TestComputeWaves_EmptyItems pins the trivial base case: nothing to plan
// is not an error and produces no waves.
func TestComputeWaves_EmptyItems(t *testing.T) {
	waves, err := computeWaves(nil, nil)
	if err != nil {
		t.Fatalf("computeWaves(nil): %v", err)
	}
	if len(waves) != 0 {
		t.Fatalf("waves = %v, want empty", waves)
	}
}

// reader is item with ReadsBindings set — a consumer of another binding's
// credentials or attributes.
func reader(serviceKey, binding, providerType string, reads ...string) plannedItem {
	it := item(serviceKey, binding, providerType)
	it.ReadsBindings = reads
	for _, r := range reads {
		it.reads = append(it.reads, readEdge{binding: r})
	}
	return it
}

// narrowReader is item reading one type of another binding — a reference
// read, as Registration.ReadsReferences declares one.
func narrowReader(serviceKey, binding, providerType, readBinding, readType string) plannedItem {
	it := item(serviceKey, binding, providerType)
	it.ReadsBindings = []string{readBinding}
	it.reads = []readEdge{{binding: readBinding, typeKey: readType}}
	return it
}

// A reference read is an edge from the one producer the reader declared,
// not from everything in that binding: the certificate after the zone it
// validates in, never after the record set beside the zone — which is the
// edge that closed the static site into a cycle.
func TestComputeWaves_ReferenceReadOrdersAfterOneProducerOnly(t *testing.T) {
	items := []plannedItem{
		narrowReader("svc", "CERT", "aws/cert", "ZONE", "aws/zone"),
		item("svc", "ZONE", "aws/zone"),
		narrowReader("svc", "ZONE", "aws/record", "EDGE", "aws/dist"),
		narrowReader("svc", "EDGE", "aws/dist", "CERT", "aws/cert"),
	}
	waves, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v — a reference read pulled in the whole binding", err)
	}
	// zone 0, cert 1 (after the zone only), dist 2, record 3.
	if want := []int{1, 0, 3, 2}; !reflect.DeepEqual(waves, want) {
		t.Errorf("waves = %v, want %v", waves, want)
	}
}

// A referenced type the binding never planned contributes no edge, like an
// unplanned DependsOn; the reader is not held back by nothing.
func TestComputeWaves_ReferenceReadOfAnUnplannedTypeIsNoEdge(t *testing.T) {
	items := []plannedItem{
		narrowReader("svc", "CERT", "aws/cert", "ZONE", "aws/zone"),
		item("svc", "ZONE", "aws/record"),
	}
	waves, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	if waves[0] != 0 {
		t.Errorf("reader wave = %d, want 0: the type it reads was never planned", waves[0])
	}
}

// A declared read is an ordering edge. Without it a consumer can share a
// wave with the producer it reads from, snapshot an empty handoff index, and
// fail at the provider far from the cause — the race #119 describes. The
// graph already models what runs before what; a read is one more thing it
// has to know.
func TestComputeWaves_ReadEdgeOrdersConsumerAfterProducer(t *testing.T) {
	items := []plannedItem{
		// The consumer is listed first and has no DependsOn of its own, so
		// nothing but the read edge can move it off wave 0.
		reader("svc", "api", "aws/function", "DB"),
		item("svc", "DB", "neon/branch"),
	}
	waves, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	if waves[1] != 0 {
		t.Errorf("producer wave = %d, want 0", waves[1])
	}
	if waves[0] != 1 {
		t.Errorf("consumer wave = %d, want 1: it reads DB and must run after everything DB produced", waves[0])
	}
}

// Every producer in a read binding orders the consumer, not just one — a
// binding expands to several resources and the consumer cannot know which of
// them publishes what it needs.
func TestComputeWaves_ReadEdgeCoversEveryProducerInTheBinding(t *testing.T) {
	items := []plannedItem{
		reader("svc", "api", "aws/function", "DB"),
		item("svc", "DB", "neon/branch"),
		item("svc", "DB", "cloudflare/hyperdrive", "neon/branch"),
	}
	waves, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	// branch 0, hyperdrive 1 (depends on branch), function 2 (reads both).
	if waves[1] != 0 || waves[2] != 1 {
		t.Errorf("producer waves = %d/%d, want 0/1", waves[1], waves[2])
	}
	if waves[0] != 2 {
		t.Errorf("consumer wave = %d, want 2: after the last producer in DB", waves[0])
	}
}

// Reading one's own binding is the common case and must not become a
// self-edge, which would stall the sort as a cycle.
func TestComputeWaves_ReadOfOwnBindingIsNotAnEdge(t *testing.T) {
	items := []plannedItem{
		reader("svc", "DB", "neon/branch", "DB"),
	}
	waves, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	if waves[0] != 0 {
		t.Errorf("wave = %d, want 0", waves[0])
	}
}

// Read edges are scoped to the consumer's own service, like every other
// edge — a binding named DB in another service is a different resource.
func TestComputeWaves_ReadEdgeScopedToService(t *testing.T) {
	items := []plannedItem{
		reader("svc-a", "api", "aws/function", "DB"),
		item("svc-b", "DB", "neon/branch"),
	}
	waves, err := computeWaves(items, nil)
	if err != nil {
		t.Fatalf("computeWaves: %v", err)
	}
	if waves[0] != 0 {
		t.Errorf("consumer wave = %d, want 0: svc-b's DB is not svc-a's DB", waves[0])
	}
}
