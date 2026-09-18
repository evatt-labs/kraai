package plan

import (
	"sort"
	"strconv"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// groupKey identifies one expansion group: the (service, binding) pair a
// single Registry.Resolve call's results share. expandCompute resolves one
// group per service (Binding == ServiceKey there), expandBinding one group
// per manifest binding.
//
// A DependsOn key resolves only within its own item's group, never across
// the whole manifest — a service's Lambda permission depends on that
// service's own function, not on every function in the manifest.
//
// # The binding name is the coupling, including across capabilities
//
// Note what is deliberately absent from this key: the capability. Two
// entries under different capabilities that share a binding name are one
// group, which is what lets a CloudFront distribution (cdn) order behind the
// S3 bucket it fronts (objects) and the certificate it presents (tls) now
// that those are three capabilities rather than one.
//
// The converse is the trap: give them different binding names and the edges
// silently do not resolve, because a dependency naming a type the group
// never planned contributes no edge — by design, for a registration its own
// conditions filtered out. A distribution then lands in the same wave as its
// own origin. The binding name is the only thing a manifest has to say
// "these belong together", and nothing enforces that an author knew it. See
// evatt-labs/kraai#197.
type groupKey struct {
	serviceKey string
	binding    string
}

// computeWaves assigns every item a Wave: the length of the longest chain
// of dependencies that must finish first, so a node with no dependencies
// gets wave 0 and every other node gets one more than the largest wave
// among the things it depends on.
//
// Two kinds of edges go into the same graph:
//
//   - Type edges, from each item's own resource.Registration.DependsOn,
//     resolved against the other items in its own group (see groupKey).
//   - Service edges, from serviceDependsOn (manifest.Service.DependsOn,
//     validated at load time), connecting every item of a named service to
//     every item of each service it depends on.
//
// A dependency naming a type the group never planned contributes no edge:
// a registration its own conditions filtered out, or supplied by
// another provider, has no node to point at.
//
// It runs Kahn's algorithm in layers rather than one node at a time, so
// the layer a node lands in depends only on the graph's shape and not on
// visit order — wave assignment is deterministic for a given manifest
// whatever the map iteration order upstream.
//
// A cycle stalls the sort rather than being detected separately; the error
// then names every item left unresolved, which is the cycle plus anything
// transitively behind it.
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
// indegree (how many unresolved dependencies it still has).
func buildGraph(items []plannedItem, serviceDependsOn map[string][]string) (adj [][]int, indegree []int) {
	n := len(items)
	adj = make([][]int, n)
	indegree = make([]int, n)

	// byGroup resolves a DependsOn key to a concrete item index, scoped to
	// the (service, binding) group that produced it — see groupKey's doc
	// comment. byService resolves a manifest depends_on service name to
	// every item that service expanded to.
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
