package neonresource

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/cloudflare"
	"github.com/evatt-labs/kraai/internal/provider/neon"
	"github.com/evatt-labs/kraai/internal/resource"
)

type call struct {
	method string
	path   string
	query  string
	body   map[string]any
}

func fakeAPI(t *testing.T, handler func(call) (int, string)) (*httptest.Server, *[]call) {
	t.Helper()
	var seen []call
	// httptest serves each request on its own goroutine, and several tests
	// here drive concurrent calls, so recording one is a concurrent append.
	var seenMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		if r.Body != nil {
			var decoded map[string]any
			_ = json.NewDecoder(r.Body).Decode(&decoded)
			c.body = decoded
		}
		seenMu.Lock()
		seen = append(seen, c)
		seenMu.Unlock()
		status, payload := handler(c)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func settings() BranchSettings {
	return BranchSettings{Project: "my-project", Database: "appdb", Role: "app", OrgID: "org-1"}
}

// neonClient serves the project and branch endpoints a branch adapter walks.
//
// WithRetryTimings is set to millisecond bounds rather than
// neon.Client's production defaults (up to a 2-minute retry ceiling per
// call — see internal/provider/neon/client.go). Several tests in this
// file deliberately return error statuses to exercise this package's
// error-propagation paths; several of those statuses (423, 429, and 5xx
// for idempotent calls) are exactly what neon.Client now retries. Without
// this, a single such test would spend up to two real minutes retrying a
// failure it wants to observe immediately — this package's own suite took
// over 480s under -race before this override was added, almost entirely
// spent in that retry loop.
func neonClient(t *testing.T, handler func(call) (int, string)) (*neon.Client, *[]call) {
	t.Helper()
	srv, seen := fakeAPI(t, handler)
	return neon.New("key",
		neon.WithBaseURL(srv.URL), neon.WithHTTPClient(srv.Client()),
		neon.WithRetryTimings(time.Millisecond, 2*time.Millisecond, 20*time.Millisecond),
	), seen
}

func cfClient(t *testing.T, handler func(call) (int, string)) (*cloudflare.Client, *[]call) {
	t.Helper()
	srv, seen := fakeAPI(t, handler)
	return cloudflare.New("tok", "acct", cloudflare.WithBaseURL(srv.URL), cloudflare.WithHTTPClient(srv.Client())), seen
}

func cfOK(result string) (int, string) {
	return 200, `{"success":true,"errors":[],"result":` + result + `}`
}

// standardNeon answers the project lookup and a branch listing.
func standardNeon(branches string) func(call) (int, string) {
	return func(c call) (int, string) {
		switch {
		case strings.HasSuffix(c.path, "/projects"):
			return 200, `{"projects":[{"id":"p-1","name":"my-project","org_id":"org-1"}],"pagination":{"cursor":""}}`
		case strings.HasSuffix(c.path, "/branches") && c.method == "GET":
			return 200, `{"branches":` + branches + `,"pagination":{"cursor":""}}`
		}
		return 200, `{}`
	}
}

func TestRegistrationsCoverTheCapability(t *testing.T) {
	nc, _ := neonClient(t, standardNeon(`[]`))
	cc, _ := cfClient(t, func(call) (int, string) { return cfOK(`[]`) })

	reg := resource.NewRegistry()
	if err := Register(reg, nc, cc, settings()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	branch, ok := reg.Lookup("neon/branch")
	if !ok || branch.Capability != Capability {
		t.Fatalf("branch registration = %+v", branch)
	}
	hyper, ok := reg.Lookup("cloudflare/hyperdrive")
	if !ok || hyper.Capability != Capability || len(hyper.DependsOn) != 1 || hyper.DependsOn[0] != "neon/branch" {
		t.Fatalf("hyperdrive registration = %+v", hyper)
	}

	// One capability expanding to two types, in registration order — but
	// only when the compute side is Cloudflare. Hyperdrive is a Workers
	// connection pooler: a Lambda connects to the branch directly over the
	// Postgres wire and would never route through it, so planning one for an
	// AWS application demands a Cloudflare account that deployment has no
	// reason to hold, to create something nothing will ever connect through.
	onWorkers := map[string]string{
		manifest.CapabilityDatabase: "neon",
		manifest.CapabilityCompute:  "cloudflare",
	}
	resolved, err := reg.Resolve(Capability, resource.ApplicabilityContext{Vendors: onWorkers})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("on Workers, vendor=neon resolved to %d type(s), want the branch and the "+
			"hyperdrive config fronting it", len(resolved))
	}
	if resolved[0].Type != TypeBranch || resolved[1].Type != TypeHyperdrive {
		t.Fatalf("resolved out of registration order: %s then %s", resolved[0].Type, resolved[1].Type)
	}

	// The same database on AWS is the branch alone.
	onLambda := map[string]string{
		manifest.CapabilityDatabase: "neon",
		manifest.CapabilityCompute:  "aws",
	}
	elsewhere, err := reg.Resolve(Capability, resource.ApplicabilityContext{Vendors: onLambda})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(elsewhere) != 1 || elsewhere[0].Type != TypeBranch {
		t.Fatalf("on AWS, vendor=neon resolved to %+v — a Lambda has no use for a Workers pooler",
			elsewhere)
	}

	// Hyperdrive is not independently selectable by its own provider name.
	if _, err := reg.Resolve(Capability, resource.ApplicabilityContext{Vendors: map[string]string{Capability: "cloudflare"}}); err == nil {
		t.Fatal("naming cloudflare as the database vendor resolved to something")
	}
}

func TestRegisterIsAllOrNothing(t *testing.T) {
	nc, _ := neonClient(t, standardNeon(`[]`))
	cc, _ := cfClient(t, func(call) (int, string) { return cfOK(`[]`) })

	reg := resource.NewRegistry()
	if err := Register(reg, nc, cc, settings()); err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, nc, cc, settings()); err == nil {
		t.Fatal("registering twice succeeded")
	}
	if len(reg.All()) != 2 {
		t.Fatalf("registry holds %d types, want 2", len(reg.All()))
	}
}

// Every API failure along a multi-step path must surface rather than being
// read as absence — teardown treating an outage as "already deleted" would
// clear a lockfile whose resources still exist.
func TestAPIFailuresSurfaceAtEveryStep(t *testing.T) {
	t.Run("branch listing fails", func(t *testing.T) {
		nc, _ := neonClient(t, func(c call) (int, string) {
			if strings.HasSuffix(c.path, "/projects") {
				return 200, `{"projects":[{"id":"p-1","name":"my-project"}],"pagination":{"cursor":""}}`
			}
			return 503, `{"code":"UNAVAILABLE","message":"down"}`
		})
		b := &branchResource{client: nc, settings: settings()}

		if _, err := b.Get(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
			t.Error("an outage was reported as an absent branch")
		}
		if err := b.Delete(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
			t.Error("an outage during teardown was reported as success")
		}
		if _, err := b.Create(t.Context(), resource.Spec{Binding: "DB", Name: "env-a"}); err == nil {
			t.Error("create succeeded despite a failing default-branch lookup")
		}
	})

	t.Run("branch create fails", func(t *testing.T) {
		nc, _ := neonClient(t, func(c call) (int, string) {
			switch {
			case strings.HasSuffix(c.path, "/projects"):
				return 200, `{"projects":[{"id":"p-1","name":"my-project"}],"pagination":{"cursor":""}}`
			case c.method == "POST":
				return 409, `{"code":"CONFLICT","message":"exists"}`
			}
			return 200, `{"branches":[{"id":"br-main","name":"main","default":true}],"pagination":{"cursor":""}}`
		})
		b := &branchResource{client: nc, settings: settings()}

		if _, err := b.Create(t.Context(), resource.Spec{Binding: "DB", Name: "env-a"}); err == nil {
			t.Error("a failed create reported success")
		}
	})

	t.Run("branch delete fails", func(t *testing.T) {
		nc, _ := neonClient(t, func(c call) (int, string) {
			switch {
			case strings.HasSuffix(c.path, "/projects"):
				return 200, `{"projects":[{"id":"p-1","name":"my-project"}],"pagination":{"cursor":""}}`
			case c.method == "DELETE":
				return 500, `{"code":"ERR","message":"nope"}`
			}
			return 200, `{"branches":[{"id":"br-1","name":"env-a"}],"pagination":{"cursor":""}}`
		})
		b := &branchResource{client: nc, settings: settings()}

		if err := b.Delete(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
			t.Error("a failed delete reported success")
		}
	})

	t.Run("hyperdrive lookups fail", func(t *testing.T) {
		cc, _ := cfClient(t, func(call) (int, string) {
			return 503, `{"success":false,"errors":[{"code":1,"message":"down"}]}`
		})
		h := &hyperdriveResource{client: cc}

		if _, err := h.Get(t.Context(), resource.Ref{Name: "n"}); err == nil {
			t.Error("an outage was reported as an absent config")
		}
		if err := h.Delete(t.Context(), resource.Ref{Name: "n"}); err == nil {
			t.Error("an outage during teardown was reported as success")
		}
	})

	t.Run("hyperdrive delete fails", func(t *testing.T) {
		cc, _ := cfClient(t, func(c call) (int, string) {
			if c.method == "DELETE" {
				return 500, `{"success":false,"errors":[{"code":1,"message":"nope"}]}`
			}
			return cfOK(`[{"id":"hd-1","name":"env-a"}]`)
		})
		h := &hyperdriveResource{client: cc}

		if err := h.Delete(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
			t.Error("a failed delete reported success")
		}
	})

	t.Run("hyperdrive create fails", func(t *testing.T) {
		cc, _ := cfClient(t, func(call) (int, string) {
			return 400, `{"success":false,"errors":[{"code":1,"message":"bad origin"}]}`
		})
		h := &hyperdriveResource{client: cc}

		_, err := h.Create(t.Context(), resource.Spec{
			Binding: "DB", Name: "env-a",
			Secrets: map[string]resource.Secret{
				SecretConnectionURI: func(context.Context) (string, error) {
					return "postgresql://a:b@h:5432/d", nil
				},
			},
		})
		if err == nil {
			t.Error("a failed create reported success")
		}
	})
}

// TestRegistrationsOmitTheCompanionWithoutAClient: a caller with no
// Cloudflare client has no Cloudflare credentials, which happens precisely
// when nothing in the manifest names Cloudflare — and the companion would be
// conditioned out anyway. Registering it against a nil client would leave a
// resource that panics if anything ever did reach it, in exchange for
// nothing.
func TestRegistrationsOmitTheCompanionWithoutAClient(t *testing.T) {
	nc, _ := neonClient(t, standardNeon(`[]`))

	withClient := Registrations(nc, &cloudflare.Client{}, settings())
	if len(withClient) != 2 {
		t.Fatalf("with a Cloudflare client: %d registrations, want branch and hyperdrive", len(withClient))
	}

	without := Registrations(nc, nil, settings())
	if len(without) != 1 || without[0].Type != TypeBranch {
		t.Fatalf("with no Cloudflare client: %+v, want the branch alone", without)
	}

	// And Register agrees, so an assembler wiring only Neon gets a usable
	// registry rather than one holding an unusable entry.
	reg := resource.NewRegistry()
	if err := Register(reg, nc, nil, settings()); err != nil {
		t.Fatalf("Register with no Cloudflare client: %v", err)
	}
	if _, ok := reg.Lookup("cloudflare/hyperdrive"); ok {
		t.Fatal("a Hyperdrive config was registered with no client to create it")
	}
}
