package direct

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const clusterType = "AWS::ECS::Cluster"

// fakeCluster is one ECS cluster, answered by operation. Each change to its
// capacity providers leaves it busy for the next few reads, as ECS attaches
// them after answering, and a change while busy is refused.
type fakeCluster struct {
	mu      sync.Mutex
	cluster map[string]any
	busy    int
	// refuse is how many further changes are refused as busy with a change
	// made elsewhere.
	refuse int
	// lower stores the name lowercased, as some services do.
	lower bool
	calls map[string][]map[string]any
}

func (f *fakeCluster) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(raw, &in)
		op := r.Header.Get("X-Amz-Target")
		op = op[strings.LastIndex(op, ".")+1:]
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[op] = append(f.calls[op], in)
		if op != "DescribeClusters" && op != "CreateCluster" && (f.busy > 0 || f.refuse > 0) {
			if f.refuse > 0 {
				f.refuse--
			}
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"__type":"UpdateInProgressException","message":"busy"}`)
			return
		}
		describe := func() {
			c := map[string]any{"clusterArn": "arn:aws:ecs:us-east-1:1:cluster/kraai-e-cluster", "status": "ACTIVE", "attachmentsStatus": "UPDATE_COMPLETE"}
			for k, v := range f.cluster {
				c[k] = v
			}
			if f.busy > 0 {
				c["attachmentsStatus"] = "UPDATE_IN_PROGRESS"
				f.busy--
			}
			body, _ := json.Marshal(map[string]any{"clusters": []any{c}, "cluster": c})
			_, _ = w.Write(body)
		}
		switch op {
		case "CreateCluster":
			name, _ := in["clusterName"].(string)
			if f.lower {
				name = strings.ToLower(name)
			}
			f.cluster = map[string]any{"clusterName": name}
			for _, k := range []string{"capacityProviders", "defaultCapacityProviderStrategy", "settings", "tags"} {
				if v, ok := in[k]; ok {
					f.cluster[k] = v
				}
			}
			f.busy = 6
		case "DeleteCluster":
			f.cluster["status"] = "INACTIVE"
		case "PutClusterCapacityProviders":
			f.cluster["capacityProviders"], f.cluster["defaultCapacityProviderStrategy"] = in["capacityProviders"], in["defaultCapacityProviderStrategy"]
			f.busy = 6
		}
		describe()
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

var clusterNameTag = map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-cluster"}

// A create returns only once the capacity providers have attached, and
// sends a write-only property it can never read back by its one member,
// without waiting to see it.
func TestCreateClusterWaitsForAttachments(t *testing.T) {
	f := &fakeCluster{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), clusterType, map[string]any{
		"CapacityProviders":      []any{"FARGATE"},
		"ServiceConnectDefaults": map[string]any{"Namespace": "kraai"},
		"Tags":                   []any{clusterNameTag},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "kraai-e-cluster" {
		t.Fatalf("id = %q", id)
	}
	if f.busy != 0 {
		t.Fatalf("Create returned with the cluster still busy for %d reads", f.busy)
	}
	sent := f.calls["CreateCluster"][0]
	if got, want := sent["serviceConnectDefaults"], map[string]any{"namespace": "kraai"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("serviceConnectDefaults = %v, want %v", got, want)
	}
}

// A service that lowercases the name it is sent answers with the name the
// instance has; the create returns that, and its wait compares with it.
func TestCreateTakesTheNameTheServiceAnswers(t *testing.T) {
	f := &fakeCluster{lower: true}
	client := f.serve(t)
	id, err := client.Create(context.Background(), clusterType, map[string]any{
		"Tags": []any{map[string]any{"Key": "kraai:resource-name", "Value": "Kraai-E-Cluster"}},
	})
	if err != nil || id != "kraai-e-cluster" {
		t.Fatalf("Create = %q, %v; want the lowercased name", id, err)
	}
}

// A property path not being set leaves its enclosing structure out.
func TestCreateClusterLeavesOutAnUnsetPath(t *testing.T) {
	f := &fakeCluster{}
	client := f.serve(t)
	if _, err := client.Create(context.Background(), clusterType, map[string]any{"Tags": []any{clusterNameTag}}); err != nil {
		t.Fatal(err)
	}
	if _, sent := f.calls["CreateCluster"][0]["serviceConnectDefaults"]; sent {
		t.Fatal("serviceConnectDefaults sent with no namespace set")
	}
}

// PutClusterCapacityProviders replaces both properties, so a change to one
// sends the other as it was read; the update returns once attached.
func TestUpdateClusterSendsCapacityProvidersTogether(t *testing.T) {
	strategy := []any{map[string]any{"capacityProvider": "FARGATE", "weight": float64(1)}}
	f := &fakeCluster{cluster: map[string]any{"clusterName": "kraai-e-cluster", "capacityProviders": []any{"FARGATE"}, "defaultCapacityProviderStrategy": strategy}}
	client := f.serve(t)
	current := map[string]any{
		"CapacityProviders":               []any{"FARGATE"},
		"DefaultCapacityProviderStrategy": []any{map[string]any{"CapacityProvider": "FARGATE", "Weight": 1}},
	}
	if err := client.Update(context.Background(), clusterType, "kraai-e-cluster", current, map[string]any{"CapacityProviders": []any{"FARGATE", "FARGATE_SPOT"}}); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"cluster": "kraai-e-cluster", "capacityProviders": []any{"FARGATE", "FARGATE_SPOT"}, "defaultCapacityProviderStrategy": strategy}}
	if got := f.calls["PutClusterCapacityProviders"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("PutClusterCapacityProviders calls = %v, want %v", got, want)
	}
	if f.busy != 0 {
		t.Fatalf("Update returned with the cluster still busy for %d reads", f.busy)
	}
}

// A cluster with providers but no default strategy reads it as an empty
// list, which the call requires, so it is sent empty rather than left out.
func TestUpdateClusterSendsARequiredEmptyStrategy(t *testing.T) {
	f := &fakeCluster{cluster: map[string]any{"clusterName": "kraai-e-cluster"}}
	client := f.serve(t)
	current := map[string]any{"CapacityProviders": []any{"FARGATE"}, "DefaultCapacityProviderStrategy": []any{}}
	if err := client.Update(context.Background(), clusterType, "kraai-e-cluster", current, map[string]any{"CapacityProviders": []any{"FARGATE", "FARGATE_SPOT"}}); err != nil {
		t.Fatal(err)
	}
	strategy, sent := f.calls["PutClusterCapacityProviders"][0]["defaultCapacityProviderStrategy"]
	if !sent || !reflect.DeepEqual(strategy, []any{}) {
		t.Fatalf("defaultCapacityProviderStrategy = %v, sent %v; want an empty list sent", strategy, sent)
	}
}

// A property sent together with a change but never read is refused, not
// left out of a call that would then fail or clear it.
func TestUpdateClusterRefusesAnUnreadTogetherProperty(t *testing.T) {
	f := &fakeCluster{cluster: map[string]any{"clusterName": "kraai-e-cluster"}}
	client := f.serve(t)
	err := client.Update(context.Background(), clusterType, "kraai-e-cluster", map[string]any{}, map[string]any{"CapacityProviders": []any{"FARGATE"}})
	if err == nil || !strings.Contains(err.Error(), "sends DefaultCapacityProviderStrategy with what changed, but it was not read") {
		t.Fatalf("Update = %v", err)
	}
	if n := len(f.calls["PutClusterCapacityProviders"]); n != 0 {
		t.Fatalf("PutClusterCapacityProviders called %d times, want 0", n)
	}
}

// A cluster busy with a change made elsewhere is waited out, not failed.
func TestDeleteClusterRetriesWhileBusy(t *testing.T) {
	f := &fakeCluster{cluster: map[string]any{"clusterName": "kraai-e-cluster"}, refuse: 2}
	client := f.serve(t)
	if err := client.Delete(context.Background(), clusterType, "kraai-e-cluster"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls["DeleteCluster"]); n != 3 {
		t.Fatalf("DeleteCluster called %d times, want 3", n)
	}
}

func TestCompileRefusesABadClusterMutation(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]Override{}
	for _, o := range all {
		base[o.Type] = o
	}
	withCreateInput := func(member string, v any) func(*Override) {
		return func(o *Override) {
			c := *o.Create
			c.Input = map[string]any{}
			for k, x := range o.Create.Input {
				c.Input[k] = x
			}
			c.Input[member] = v
			o.Create = &c
		}
	}
	cases := map[string]struct {
		typeName string
		edit     func(*Override)
		want     string
	}{
		"together with one property": {clusterType, func(o *Override) {
			u := o.Update[0]
			u.Together = true
			o.Update = append([]UpdateCall{u}, o.Update[1:]...)
		}, "sets its properties together, but has only 1"},
		"a path the schema lacks": {clusterType, withCreateInput("serviceConnectDefaults", map[string]any{"namespace": "{ServiceConnectDefaults.Nope}"}),
			"which is not a path through"},
		"a path through a list": {clusterType, withCreateInput("capacityProviders", "{DefaultCapacityProviderStrategy.Weight}"),
			"which is not a path through"},
		"busy on a member the resource lacks": {clusterType, func(o *Override) {
			o.Read.Busy = map[string][]string{"nope": {"X"}}
		}, "busy names nope"},
		"busy on a member an XML resource lacks": {"AWS::ElastiCache::SubnetGroup", func(o *Override) {
			o.Read.Busy = map[string][]string{"nope": {"X"}}
		}, "busy names nope"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := base[c.typeName]
			o.Update = append([]UpdateCall(nil), o.Update...)
			c.edit(&o)
			if _, errs := compileOne(files, lock, o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}
