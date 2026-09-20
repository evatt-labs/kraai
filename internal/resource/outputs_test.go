package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestOutputsCarryStateBetweenPhases(t *testing.T) {
	out := NewOutputs()
	ref := Ref{Provider: "neon", Type: "branch", Name: "env-a"}

	out.Put(&State{Ref: ref, ID: "br-1", Attributes: map[string]any{"host": "ep-x.neon.tech"}})

	state, ok := out.Get(ref)
	if !ok || state.ID != "br-1" {
		t.Fatalf("state = %+v, ok = %v", state, ok)
	}
	host, ok := out.Attribute(ref, "host")
	if !ok || host != "ep-x.neon.tech" {
		t.Fatalf("host = %v, ok = %v", host, ok)
	}
}

func TestOutputsScopeByRefNotJustType(t *testing.T) {
	out := NewOutputs()
	a := Ref{Provider: "cloudflare", Type: "kv_namespace", Name: "env-a-api-kv"}
	b := Ref{Provider: "cloudflare", Type: "kv_namespace", Name: "env-a-web-kv"}

	out.Put(&State{Ref: a, ID: "ns-a"})
	out.Put(&State{Ref: b, ID: "ns-b"})

	got, _ := out.Get(a)
	if got.ID != "ns-a" {
		t.Fatalf("two resources of the same type collided: %+v", got)
	}
	if _, ok := out.Get(Ref{Provider: "cloudflare", Type: "kv_namespace", Name: "absent"}); ok {
		t.Fatal("an unrecorded ref resolved")
	}
}

func TestOutputsMissingAttributeAndState(t *testing.T) {
	out := NewOutputs()
	ref := Ref{Provider: "p", Type: "t", Name: "n"}

	if _, ok := out.Attribute(ref, "host"); ok {
		t.Fatal("an attribute resolved for an unrecorded resource")
	}
	out.Put(&State{Ref: ref, ID: "id"})
	if _, ok := out.Attribute(ref, "host"); ok {
		t.Fatal("an absent attribute resolved")
	}
	// A nil state is ignored rather than panicking: a verb that failed
	// returns one, and the applier should not have to special-case it.
	out.Put(nil)
}

// TestSecretsAreNeverStoredInTheStruct is Q3's whole point. A credential is a
// producer called at the moment of use, so nothing that is logged,
// serialised, written to the lockfile or included in an error has ever held
// one — because the struct never did.
//
//nolint:gosec // G101: a fabricated credential, which is precisely what this test keeps contained
func TestSecretsAreNeverStoredInTheStruct(t *testing.T) {
	const credential = "postgresql://app:hunter2@ep-x.neon.tech/appdb"

	out := NewOutputs()
	ref := Ref{Provider: "neon", Type: "branch", Name: "env-a"}
	out.Put(&State{Ref: ref, ID: "br-1", Attributes: map[string]any{"host": "ep-x.neon.tech"}})
	out.PutSecret(ref, "connection_uri", func(context.Context) (string, error) {
		return credential, nil
	})

	// Anything that walks the struct must not find it. %#v is the deepest
	// reflection-based rendering a debug print or panic dump would produce.
	for _, rendering := range []string{
		fmt.Sprintf("%v", out),
		fmt.Sprintf("%+v", out),
		fmt.Sprintf("%#v", out),
	} {
		if strings.Contains(rendering, "hunter2") {
			t.Fatalf("the credential is reachable by formatting the struct: %s", rendering)
		}
	}

	// And by serialising the state that does get written to the lockfile.
	state, _ := out.Get(ref)
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "hunter2") {
		t.Fatalf("the credential survives into serialised state: %s", encoded)
	}

	// It is still reachable by the consumer that needs it.
	got, err := out.Secret(t.Context(), ref, "connection_uri")
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if got != credential {
		t.Fatalf("Secret returned %q", got)
	}
}

// Resolving twice calls the producer twice, deliberately: caching would put
// the credential back into the struct this design keeps it out of.
func TestSecretIsNotCached(t *testing.T) {
	var calls int
	out := NewOutputs()
	ref := Ref{Provider: "neon", Type: "branch", Name: "env-a"}
	out.PutSecret(ref, "uri", func(context.Context) (string, error) {
		calls++
		return "value", nil
	})

	for range 3 {
		if _, err := out.Secret(t.Context(), ref, "uri"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("producer ran %d times, want one per resolution", calls)
	}
}

func TestSecretPropagatesProducerFailure(t *testing.T) {
	out := NewOutputs()
	ref := Ref{Provider: "neon", Type: "branch", Name: "env-a"}
	sentinel := errors.New("neon refused")
	out.PutSecret(ref, "uri", func(context.Context) (string, error) { return "", sentinel })

	_, err := out.Secret(t.Context(), ref, "uri")
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the producer's failure", err)
	}
}

// The error names the resource and the secret, never a value — there is none
// here to leak, which is the point.
func TestMissingSecretErrorNamesWhatWasAskedFor(t *testing.T) {
	out := NewOutputs()
	ref := Ref{Provider: "neon", Type: "branch", Name: "env-a"}

	_, err := out.Secret(t.Context(), ref, "connection_uri")
	if err == nil {
		t.Fatal("an unregistered secret resolved")
	}
	for _, want := range []string{"connection_uri", "neon/branch", "env-a"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}

// Resources within a wave run concurrently under a bounded errgroup, so
// concurrent writes here must be safe.
func TestOutputsAreConcurrencySafe(t *testing.T) {
	out := NewOutputs()
	var wg sync.WaitGroup

	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := Ref{Provider: "cloudflare", Type: "kv_namespace", Name: fmt.Sprintf("ns-%d", i)}
			out.Put(&State{Ref: ref, ID: fmt.Sprintf("id-%d", i)})
			out.PutSecret(ref, "token", func(context.Context) (string, error) { return "t", nil })
			_, _ = out.Get(ref)
			_, _ = out.Attribute(ref, "missing")
			_, _ = out.Secret(context.Background(), ref, "token")
		}(i)
	}
	wg.Wait()

	for i := range 64 {
		ref := Ref{Provider: "cloudflare", Type: "kv_namespace", Name: fmt.Sprintf("ns-%d", i)}
		if state, ok := out.Get(ref); !ok || state.ID != fmt.Sprintf("id-%d", i) {
			t.Fatalf("lost the write for %s", ref.Name)
		}
	}
}

// TestSpecSecretResolvesAndFails: a resource consuming a credential from an
// earlier phase gets a producer, not a value — the same guarantee Outputs
// makes, carried through to the resource rather than stopping at the applier.
func TestSpecSecretResolvesAndFails(t *testing.T) {
	spec := Spec{
		Binding: "HYPERDRIVE",
		Secrets: map[string]Secret{
			"connection_uri": func(context.Context) (string, error) { return "postgresql://x", nil },
			"broken":         func(context.Context) (string, error) { return "", errors.New("producer failed") },
		},
	}

	got, err := spec.Secret(t.Context(), "connection_uri")
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if got != "postgresql://x" {
		t.Fatalf("got %q", got)
	}

	if _, err := spec.Secret(t.Context(), "broken"); err == nil {
		t.Fatal("a failing producer was reported as success")
	}

	_, err = spec.Secret(t.Context(), "absent")
	if err == nil {
		t.Fatal("an unsupplied credential resolved")
	}
	// Names what was asked for and which binding needed it, never a value.
	for _, want := range []string{"absent", "HYPERDRIVE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}
