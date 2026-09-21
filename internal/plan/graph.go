package plan

import (
	"sort"
	"strconv"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// groupKey identifies one expansion group: the (service, binding) pair one
// Registry.Resolve call's results share. A DependsOn key resolves only
// within its own item's group, never across the manifest: a service's
// Lambda permission depends on that service's own function.
//
// The capability is deliberately absent, so a DependsOn across capabilities
// resolves only when binding names happen to coincide and contributes
// nothing otherwise. A relationship between two bindings is a read, declared
// by the entry that names the other binding, whatever either is called.
type groupKey struct {
	serviceKey string
	binding    string
}

// computeWaves assigns every item a Wave: the length of the longest chain of
// dependencies that must finish first. Three kinds of edge go into one
// graph: type edges from each item's DependsOn, resolved within its group;
// read edges from each item's reads; and service edges from
// serviceDependsOn, connecting every item of a service to every item of each
// service it depends on. A dependency naming a type the group never planned
// contributes no edge.
//
// Kahn's algorithm runs in layers rather than one node at a time, so the
// layer a node lands in depends only on the graph's shape and not on visit
// order. A cycle stalls the sort; the error names every item left
// unresolved, which is the cycle plus anything transitively behind it.
func computeWaves(items []plannedItem, serviceDependsOn map[string][]string) ([]int, error) {
	n := len(items)
	waves := make([]int, n)
	if n == 0 {
		return waves, nil
	}

	adj, indegree := buildGraph(items, serviceDependsOn)

	var frontier []int
	for i, d := range indegree {
		if d == 0 {
			frontier = append(frontier, i)
		}
	}
	sort.Ints(frontier)

	assigned := 0
	wave := 0
	for len(frontier) > 0 {
		var next []int
		for _, i := range frontier {
			waves[i] = wave
			assigned++
			for _, j := range adj[i] {
				indegree[j]--
				if indegree[j] == 0 {
					next = append(next, j)
				}
			}
		}
		sort.Ints(next)
		frontier = next
		wave++
	}

	if assigned < n {
		return nil, cycleError(items, indegree)
	}
	return waves, nil
}

// buildGraph resolves every edge computeWaves needs into an adjacency list
// (adj[i] is every node that depends directly on i) and each node's
// indegree.
func buildGraph(items []plannedItem, serviceDependsOn map[string][]string) (adj [][]int, indegree []int) {
	n := len(items)
	adj = make([][]int, n)
	indegree = make([]int, n)

	// byGroup resolves a DependsOn key to an item index within its
	// (service, binding) group; byService resolves a manifest depends_on
	// service name to every item that service expanded to.
	byGroup := make(map[groupKey]map[string]int, n)
	byService := make(map[string][]int, n)
	for i, it := range items {
		gk := groupKey{it.ServiceKey, it.Binding}
		byType, ok := byGroup[gk]
		if !ok {
			byType = make(map[string]int)
			byGroup[gk] = byType
		}
		byType[it.ref.Key()] = i
		byService[it.ServiceKey] = append(byService[it.ServiceKey], i)
	}

	addEdge := func(from, to int) {
		if from == to {
			return
		}
		adj[from] = append(adj[from], to)
		indegree[to]++
	}

	for i, it := range items {
		gk := groupKey{it.ServiceKey, it.Binding}
		for _, depKey := range it.dependsOn {
			if depIdx, ok := byGroup[gk][depKey]; ok {
				addEdge(depIdx, i)
			}
		}
	}

	// A consumer runs after every producer in each binding it reads. apply
	// hands a consumer what its read bindings have published so far, and
	// within one wave "so far" is a race; the edge means the pair can never
	// share a wave. Only bindings other than the consumer's own: same-binding
	// producers are ordered by DependsOn already, and reading one's own
	// binding must not become a self-edge. A referenced type the binding
	// never planned contributes no edge; the reader's own translate then
	// fails naming what never published.
	for i, it := range items {
		for _, read := range it.reads {
			if read.binding == it.Binding {
				continue
			}
			producers := byGroup[groupKey{it.ServiceKey, read.binding}]
			if read.typeKey != "" {
				if producerIdx, ok := producers[read.typeKey]; ok {
					addEdge(producerIdx, i)
				}
				continue
			}
			for _, producerIdx := range producers {
				addEdge(producerIdx, i)
			}
		}
	}

	for svc, deps := range serviceDependsOn {
		for _, dep := range deps {
			for _, from := range byService[dep] {
				for _, to := range byService[svc] {
					addEdge(from, to)
				}
			}
		}
	}

	return adj, indegree
}

// cycleError names every item a stalled topological sort left unresolved,
// sorted so the message is identical across runs.
func cycleError(items []plannedItem, indegree []int) error {
	var names []string
	for i, d := range indegree {
		if d > 0 {
			it := items[i]
			names = append(names, it.ref.Key()+" "+strconv.Quote(it.ref.Name)+
				" (service "+it.ServiceKey+", binding "+it.Binding+")")
		}
	}
	sort.Strings(names)
	return kerrors.Validation(
		"dependency cycle detected among %d resource(s): %s — check resource.Registration.DependsOn "+
			"and any depends_on entries naming these services",
		len(names), strings.Join(names, "; "))
}
