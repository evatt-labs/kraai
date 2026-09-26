package neonresource

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// TestHyperdriveCreateConsumesTheProducer exercises Spec.Secrets end to end:
// the credential arrives as a function, is used inside Create, and appears
// nowhere else.
func TestHyperdriveCreateConsumesTheProducer(t *testing.T) {
	const password = "hunter2"
	cc, seen := cfClient(t, func(c call) (int, string) {
		if c.method == "POST" {
			return cfOK(`{"id":"hd-1","name":"env-a-api-db"}`)
		}
		return cfOK(`[]`)
	})
	h := &hyperdriveResource{client: cc}

	var produced int
	state, err := h.Create(t.Context(), resource.Spec{
		Binding: "DB", Name: "env-a-api-db",
		Secrets: map[string]resource.Secret{
			SecretConnectionURI: func(context.Context) (string, error) {
				produced++
				return "postgresql://app:" + password + "@ep-x.neon.tech:5432/appdb?sslmode=verify-full", nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if produced != 1 {
		t.Fatalf("the credential producer ran %d times, want once at the point of use", produced)
	}
	if state.ID != "hd-1" {
		t.Fatalf("state = %+v", state)
	}

	// The password reaches Cloudflare in the request body, split into fields.
	origin, _ := (*seen)[0].body["origin"].(map[string]any)
	if origin["password"] != password {
		t.Fatalf("origin = %+v, want the resolved password", origin)
	}
	if origin["host"] != "ep-x.neon.tech" || origin["database"] != "appdb" || origin["user"] != "app" {
		t.Fatalf("origin fields = %+v", origin)
	}
	if origin["port"] != float64(5432) {
		t.Fatalf("port = %v, want a number", origin["port"])
	}
	mtls, _ := (*seen)[0].body["mtls"].(map[string]any)
	if mtls["sslmode"] != "verify-full" {
		t.Fatalf("sslmode = %v, want the URI's own", mtls["sslmode"])
	}

	// And nowhere in the state that reaches the lockfile.
	encoded, _ := json.Marshal(state)
	if strings.Contains(string(encoded), password) {
		t.Fatalf("the credential reached serialised state: %s", encoded)
	}
}

func TestHyperdriveCreateWithoutTheSecretFails(t *testing.T) {
	cc, seen := cfClient(t, func(call) (int, string) { return cfOK(`{}`) })
	h := &hyperdriveResource{client: cc}

	_, err := h.Create(t.Context(), resource.Spec{Binding: "DB", Name: "env-a-api-db"})
	if err == nil {
		t.Fatal("a hyperdrive config was created with no connection string")
	}
	if !strings.Contains(err.Error(), SecretConnectionURI) {
		t.Fatalf("error should name the missing credential: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatal("the API was called despite the missing credential")
	}
}

func TestHyperdriveCreateRejectsAnUnparseableURI(t *testing.T) {
	cc, seen := cfClient(t, func(call) (int, string) { return cfOK(`{}`) })
	h := &hyperdriveResource{client: cc}

	_, err := h.Create(t.Context(), resource.Spec{
		Binding: "DB", Name: "env-a-api-db",
		Secrets: map[string]resource.Secret{
			SecretConnectionURI: func(context.Context) (string, error) { return "not-a-uri", nil },
		},
	})
	if err == nil {
		t.Fatal("an unparseable connection string was accepted")
	}
	if len(*seen) != 0 {
		t.Fatal("the API was called with an unparsed connection")
	}
}

func TestHyperdriveGetDeleteAndUpdate(t *testing.T) {
	var deleted string
	cc, _ := cfClient(t, func(c call) (int, string) {
		if c.method == "DELETE" {
			deleted = c.path
			return cfOK(`{}`)
		}
		return cfOK(`[{"id":"hd-9","name":"env-a-api-db"}]`)
	})
	h := &hyperdriveResource{client: cc}

	state, err := h.Get(t.Context(), resource.Ref{Name: "env-a-api-db"})
	if err != nil || state == nil || state.ID != "hd-9" {
		t.Fatalf("Get = %+v, %v", state, err)
	}
	if err := h.Delete(t.Context(), resource.Ref{Name: "env-a-api-db"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !strings.HasSuffix(deleted, "/hd-9") {
		t.Fatalf("deleted %q, want the looked-up id", deleted)
	}

	_, err = h.Update(t.Context(), resource.Ref{Name: "n"}, resource.Spec{Binding: "DB"})
	if !errors.Is(err, resource.ErrImmutable) {
		t.Fatalf("Update returned %v, want ErrImmutable", err)
	}
}

func TestHyperdriveAbsentIsNothing(t *testing.T) {
	cc, seen := cfClient(t, func(call) (int, string) { return cfOK(`[]`) })
	h := &hyperdriveResource{client: cc}

	state, err := h.Get(t.Context(), resource.Ref{Name: "missing"})
	if err != nil || state != nil {
		t.Fatalf("Get = %+v, %v", state, err)
	}
	if err := h.Delete(t.Context(), resource.Ref{Name: "missing"}); err != nil {
		t.Fatalf("deleting an absent config failed: %v", err)
	}
	for _, c := range *seen {
		if c.method == "DELETE" {
			t.Fatal("a delete was issued for a config that does not exist")
		}
	}
}

// A producer that cannot fetch the credential must stop the create rather
// than sending an empty origin to Cloudflare.
func TestHyperdriveFailingProducerStopsCreate(t *testing.T) {
	cc, seen := cfClient(t, func(call) (int, string) { return cfOK(`{}`) })
	h := &hyperdriveResource{client: cc}

	_, err := h.Create(t.Context(), resource.Spec{
		Binding: "DB", Name: "env-a",
		Secrets: map[string]resource.Secret{
			SecretConnectionURI: func(context.Context) (string, error) {
				return "", errors.New("neon refused")
			},
		},
	})
	if err == nil {
		t.Fatal("a failing credential producer did not stop the create")
	}
	if len(*seen) != 0 {
		t.Fatal("the API was called without a connection string")
	}
}

// Creating without a derived name would produce a config nothing can find
// again, so it fails before the credential is even resolved.
func TestHyperdriveCreateRequiresADerivedName(t *testing.T) {
	cc, seen := cfClient(t, func(call) (int, string) { return cfOK(`{}`) })
	h := &hyperdriveResource{client: cc}

	var produced int
	_, err := h.Create(t.Context(), resource.Spec{
		Binding: "DB",
		Secrets: map[string]resource.Secret{
			SecretConnectionURI: func(context.Context) (string, error) {
				produced++
				return "postgresql://a:b@h:5432/d", nil
			},
		},
	})
	if err == nil {
		t.Fatal("a hyperdrive config was created with no derived name")
	}
	if produced != 0 {
		t.Fatal("the credential was fetched before the name was validated")
	}
	if len(*seen) != 0 {
		t.Fatal("the API was called despite the missing name")
	}
}

// The entry's caching block reaches the request in Cloudflare's own field
// names, and an entry declaring none sends none — Cloudflare's defaults
// are not restated.
func TestHyperdriveCreateSendsTheEntrysCaching(t *testing.T) {
	uri := func(context.Context) (string, error) {
		return "postgresql://app:pw@ep-x.neon.tech:5432/appdb?sslmode=verify-full", nil
	}
	for _, tc := range []struct {
		name   string
		config map[string]any
		want   map[string]any
	}{
		{"disabled with a max age", map[string]any{"caching": map[string]any{"disabled": true, "maxAge": 30}},
			map[string]any{"disabled": true, "max_age": float64(30)}},
		{"enabled, default age", map[string]any{"caching": map[string]any{"disabled": false}},
			map[string]any{"disabled": false}},
		{"none declared", map[string]any{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc, seen := cfClient(t, func(c call) (int, string) {
				if c.method == "POST" {
					return cfOK(`{"id":"hd-1","name":"env-a-api-db"}`)
				}
				return cfOK(`[]`)
			})
			h := &hyperdriveResource{client: cc}
			if _, err := h.Create(t.Context(), resource.Spec{
				Binding: "DB", Name: "env-a-api-db", Config: tc.config,
				Secrets: map[string]resource.Secret{SecretConnectionURI: uri},
			}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, present := (*seen)[0].body["caching"]
			if tc.want == nil {
				if present {
					t.Fatalf("caching = %v sent for an entry declaring none", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("caching = %v, want %v", got, tc.want)
			}
		})
	}
}

// A changed caching block is a replacement — Update is refused for this
// type — and an entry declaring none never reports drift against
// Cloudflare's defaults.
func TestHyperdriveDiffOnCaching(t *testing.T) {
	h := &hyperdriveResource{}
	live := &resource.State{Attributes: map[string]any{"caching": map[string]any{"disabled": false, "max_age": 60}}}

	spec := resource.Spec{Config: map[string]any{"caching": map[string]any{"disabled": false, "maxAge": 60}}}
	if d, _ := h.Diff(spec, live); d != resource.Same {
		t.Errorf("Diff of the same caching = %v, want Same", d)
	}
	spec = resource.Spec{Config: map[string]any{"caching": map[string]any{"disabled": true}}}
	if d, _ := h.Diff(spec, live); d != resource.Immutable {
		t.Errorf("Diff after disabling = %v, want Immutable", d)
	}
	spec = resource.Spec{Config: map[string]any{"caching": map[string]any{"maxAge": 5}}}
	if d, _ := h.Diff(spec, live); d != resource.Immutable {
		t.Errorf("Diff after a new max age = %v, want Immutable", d)
	}
	if d, _ := h.Diff(resource.Spec{Config: map[string]any{}}, live); d != resource.Same {
		t.Errorf("Diff with nothing declared = %v, want Same", d)
	}
}
