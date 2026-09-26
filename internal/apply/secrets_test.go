package apply

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// --- secret handoff ---

func TestApply_SecretHandoffAcrossWaves_SameBinding(t *testing.T) {
	branch := newFakeResource()
	branch.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}, ID: "b1"}
	branchWithSecrets := &fakeSecretResource{
		fakeResource: branch,
		secretsFn: func(*resource.State) map[string]resource.Secret {
			return map[string]resource.Secret{
				"connection_uri": func(context.Context) (string, error) { return "postgres://secret", nil },
			}
		},
	}

	hyperdrive := newFakeResource()
	hyperdrive.createState = &resource.State{Ref: resource.Ref{Provider: "cf", Type: "hyperdrive", Name: "api-DB"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: branchWithSecrets},
		resource.Registration{Provider: "cf", Type: "hyperdrive", Capability: "database",
			Lookup: resource.LookupByName, Resource: hyperdrive},
	)

	// Both actions share ServiceKey "api" and Binding "DB" — the same
	// manifest binding expanding to two provider types, exactly the shape
	// planner.expandBinding produces for Neon+Hyperdrive.
	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
		action("api", "DB", "cf", "hyperdrive", 1, plan.ActionCreate),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	secret, ok := hyperdrive.LastSpec().Secrets["connection_uri"]
	if !ok {
		t.Fatalf("hyperdrive's spec carried no connection_uri secret: %+v", hyperdrive.LastSpec().Secrets)
	}
	value, err := secret(context.Background())
	if err != nil || value != "postgres://secret" {
		t.Fatalf("secret() = %q, %v, want \"postgres://secret\", nil", value, err)
	}
}

func TestApply_SecretsDoNotLeakAcrossBindings(t *testing.T) {
	branch := newFakeResource()
	branch.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}}
	branchWithSecrets := &fakeSecretResource{
		fakeResource: branch,
		secretsFn: func(*resource.State) map[string]resource.Secret {
			return map[string]resource.Secret{
				"connection_uri": func(context.Context) (string, error) { return "postgres://secret", nil },
			}
		},
	}

	// A second binding's Hyperdrive-alike, expanded independently, must
	// not see the first binding's secret.
	otherHyperdrive := newFakeResource()
	otherHyperdrive.createState = &resource.State{Ref: resource.Ref{Provider: "cf", Type: "hyperdrive", Name: "api-OTHER"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: branchWithSecrets},
		resource.Registration{Provider: "cf", Type: "hyperdrive", Capability: "database",
			Lookup: resource.LookupByName, Resource: otherHyperdrive},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
		action("api", "OTHER", "cf", "hyperdrive", 1, plan.ActionCreate),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(otherHyperdrive.LastSpec().Secrets) != 0 {
		t.Fatalf("otherHyperdrive's spec carried secrets = %+v, want none: secrets must not leak across bindings",
			otherHyperdrive.LastSpec().Secrets)
	}
}

func TestApply_NoChangeStillPopulatesOutputsAndSecrets(t *testing.T) {
	current := &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}, ID: "existing"}
	branch := newFakeResource()
	branchWithSecrets := &fakeSecretResource{
		fakeResource: branch,
		secretsFn: func(state *resource.State) map[string]resource.Secret {
			if state != current {
				t.Errorf("Secrets() called with %+v, want the action's Current state", state)
			}
			return map[string]resource.Secret{
				"connection_uri": func(context.Context) (string, error) { return "postgres://existing", nil },
			}
		},
	}

	hyperdrive := newFakeResource()
	hyperdrive.createState = &resource.State{Ref: resource.Ref{Provider: "cf", Type: "hyperdrive", Name: "api-DB"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: branchWithSecrets},
		resource.Registration{Provider: "cf", Type: "hyperdrive", Capability: "database",
			Lookup: resource.LookupByName, Resource: hyperdrive},
	)

	noChange := action("api", "DB", "neon", "branch", 0, plan.ActionNoChange)
	noChange.Current = current

	p := &plan.Plan{Actions: []plan.Action{
		noChange,
		action("api", "DB", "cf", "hyperdrive", 1, plan.ActionCreate),
	}}

	result, err := New(reg).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if branch.createCalls != 0 || branch.deleteCalls != 0 {
		t.Errorf("branch create=%d delete=%d, want 0: ActionNoChange must not call the provider", branch.createCalls, branch.deleteCalls)
	}
	if r := findResult(t, result, "api-DB"); r.Outcome != OutcomeUnchanged {
		t.Errorf("outcome for the branch = %v, want OutcomeUnchanged", r.Outcome)
	}
	if secret, ok := hyperdrive.LastSpec().Secrets["connection_uri"]; !ok {
		t.Fatalf("hyperdrive's spec carried no connection_uri secret from the unchanged branch")
	} else if v, err := secret(context.Background()); err != nil || v != "postgres://existing" {
		t.Fatalf("secret() = %q, %v", v, err)
	}
}

func TestApply_NoChangeWithNilCurrentIsInvalidPlan(t *testing.T) {
	db := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})
	noChange := action("api", "DB", "neon", "branch", 0, plan.ActionNoChange)
	// Current left nil deliberately: an invalid plan this package defends
	// against per Rule 5 (avoid silent assumptions across a package
	// boundary) rather than trusting internal/plan's own invariant blindly.
	p := &plan.Plan{Actions: []plan.Action{noChange}}

	result, err := New(reg).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeFailed || r.Err == nil {
		t.Fatalf("result = %+v, want OutcomeFailed with an error", r)
	}
}

// --- cross-binding secrets (ReadsBindings) ---

// TestApply_ComputeReadsSiblingBindingSecret_Namespaced is the gap this
// workstream fixes: a compute item's own Binding is the service key (see
// internal/plan/compute.go's expandCompute), not any one of the bindings it
// reads — so without ReadsBindings, a Lambda for service "api" could never
// see the connection_uri its own "DB" binding produced. With ReadsBindings
// set to ["DB"], it must see it namespaced as "DB.connection_uri" — never
// bare, since "DB" is not this action's own binding.
func TestApply_ComputeReadsSiblingBindingSecret_Namespaced(t *testing.T) {
	branch := newFakeResource()
	branch.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}}
	branchWithSecrets := &fakeSecretResource{
		fakeResource: branch,
		secretsFn: func(*resource.State) map[string]resource.Secret {
			return map[string]resource.Secret{
				"connection_uri": func(context.Context) (string, error) { return "postgres://secret", nil },
			}
		},
	}

	lambda := newFakeResource()
	lambda.createState = &resource.State{Ref: resource.Ref{Provider: "aws", Type: "function", Name: "api"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: branchWithSecrets},
		resource.Registration{Provider: "aws", Type: "function", Capability: "compute",
			Lookup: resource.LookupByName, Resource: lambda},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
		actionReading("api", "api", "aws", "function", 2, plan.ActionCreate, []string{"DB"}),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	secrets := lambda.LastSpec().Secrets
	if _, bare := secrets["connection_uri"]; bare {
		t.Errorf("lambda's spec carried a bare %q secret, want it namespaced: %+v", "connection_uri", secrets)
	}
	secret, ok := secrets["DB.connection_uri"]
	if !ok {
		t.Fatalf("lambda's spec carried no DB.connection_uri secret: %+v", secrets)
	}
	value, err := secret(context.Background())
	if err != nil || value != "postgres://secret" {
		t.Fatalf("secret() = %q, %v, want \"postgres://secret\", nil", value, err)
	}
}

// TestApply_TwoReadableBindingsSameSecretName_NoCollision proves the
// namespacing rule is collision-free by construction: a compute item
// reading two sibling bindings that each produce a secret named
// "connection_uri" must see both, distinguished by binding prefix, never
// one silently overwriting the other in the merged map.
func TestApply_TwoReadableBindingsSameSecretName_NoCollision(t *testing.T) {
	newBranch := func(uri string) resource.Resource {
		f := newFakeResource()
		f.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch"}}
		return &fakeSecretResource{
			fakeResource: f,
			secretsFn: func(*resource.State) map[string]resource.Secret {
				return map[string]resource.Secret{
					"connection_uri": func(context.Context) (string, error) { return uri, nil },
				}
			},
		}
	}
	primary := newBranch("postgres://primary")
	replica := newBranch("postgres://replica")

	lambda := newFakeResource()
	lambda.createState = &resource.State{Ref: resource.Ref{Provider: "aws", Type: "function", Name: "api"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: primary},
		resource.Registration{Provider: "neon", Type: "branch2", Capability: "database",
			Lookup: resource.LookupByName, Resource: replica},
		resource.Registration{Provider: "aws", Type: "function", Capability: "compute",
			Lookup: resource.LookupByName, Resource: lambda},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "PRIMARY", "neon", "branch", 0, plan.ActionCreate),
		action("api", "REPLICA", "neon", "branch2", 0, plan.ActionCreate),
		actionReading("api", "api", "aws", "function", 2, plan.ActionCreate,
			[]string{"PRIMARY", "REPLICA"}),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	secrets := lambda.LastSpec().Secrets
	if len(secrets) != 2 {
		t.Fatalf("secrets = %+v, want exactly 2 entries, not a collision-overwritten 1", secrets)
	}
	primaryURI, err := secrets["PRIMARY.connection_uri"](context.Background())
	if err != nil || primaryURI != "postgres://primary" {
		t.Errorf("PRIMARY.connection_uri = %q, %v, want \"postgres://primary\", nil", primaryURI, err)
	}
	replicaURI, err := secrets["REPLICA.connection_uri"](context.Background())
	if err != nil || replicaURI != "postgres://replica" {
		t.Errorf("REPLICA.connection_uri = %q, %v, want \"postgres://replica\", nil", replicaURI, err)
	}
}

// TestApply_NonComputeActionExplicitReadsBindings_OwnBindingOnly mirrors
// TestApply_SecretsDoNotLeakAcrossBindings but with ReadsBindings set
// explicitly to the item's own binding — the exact value
// internal/plan/binding.go's expandBinding now writes for every non-compute
// item — rather than relying on the nil-fallback path. A sibling binding's
// secret must still not leak in.
func TestApply_NonComputeActionExplicitReadsBindings_OwnBindingOnly(t *testing.T) {
	branch := newFakeResource()
	branch.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}}
	branchWithSecrets := &fakeSecretResource{
		fakeResource: branch,
		secretsFn: func(*resource.State) map[string]resource.Secret {
			return map[string]resource.Secret{
				"connection_uri": func(context.Context) (string, error) { return "postgres://secret", nil },
			}
		},
	}

	otherHyperdrive := newFakeResource()
	otherHyperdrive.createState = &resource.State{Ref: resource.Ref{Provider: "cf", Type: "hyperdrive", Name: "api-OTHER"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: branchWithSecrets},
		resource.Registration{Provider: "cf", Type: "hyperdrive", Capability: "database",
			Lookup: resource.LookupByName, Resource: otherHyperdrive},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
		actionReading("api", "OTHER", "cf", "hyperdrive", 1, plan.ActionCreate, []string{"OTHER"}),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(otherHyperdrive.LastSpec().Secrets) != 0 {
		t.Fatalf("otherHyperdrive's spec carried secrets = %+v, want none: an explicit own-binding-only "+
			"ReadsBindings must not see a sibling binding's secret", otherHyperdrive.LastSpec().Secrets)
	}
}

// TestEffectiveReadsBindings_FallsBackToOwnBinding is the unit-level pin
// for the defensive fallback apply.go's execute relies on: a plan.Action
// whose ReadsBindings is nil or empty must resolve to exactly its own
// Binding, matching what every action saw before this field existed,
// rather than emerging as "reads nothing" by accident.
func TestEffectiveReadsBindings_FallsBackToOwnBinding(t *testing.T) {
	nilCase := action("api", "DB", "neon", "branch", 0, plan.ActionCreate)
	if got := effectiveReadsBindings(nilCase); len(got) != 1 || got[0] != "DB" {
		t.Errorf("nil ReadsBindings: effectiveReadsBindings = %v, want [DB]", got)
	}

	emptyCase := actionReading("api", "DB", "neon", "branch", 0, plan.ActionCreate, []string{})
	if got := effectiveReadsBindings(emptyCase); len(got) != 1 || got[0] != "DB" {
		t.Errorf("empty ReadsBindings: effectiveReadsBindings = %v, want [DB]", got)
	}

	explicitCase := actionReading("api", "api", "aws", "function", 2, plan.ActionCreate,
		[]string{"CACHE", "DB"})
	got := effectiveReadsBindings(explicitCase)
	if len(got) != 2 || got[0] != "CACHE" || got[1] != "DB" {
		t.Errorf("explicit ReadsBindings: effectiveReadsBindings = %v, want [CACHE DB] unchanged", got)
	}
}
