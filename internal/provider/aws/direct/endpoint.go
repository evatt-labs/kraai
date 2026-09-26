package direct

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// partitionValues are the aws partition's values for the rule set
// variables a standard endpoint URL uses.
var partitionValues = strings.NewReplacer(
	"{PartitionResult#dnsSuffix}", "amazonaws.com",
	"{PartitionResult#dualStackDnsSuffix}", "api.aws",
	"{PartitionResult#implicitGlobalRegion}", "us-east-1",
	"{Region}", "{region}",
)

// otherPartitionRegions prefix the regions of partitions other than aws,
// which a literal host under amazonaws.com can still name.
var otherPartitionRegions = []string{"us-gov", "cn-", "us-iso", "eu-iso", "eusc-"}

// endpointOf reads the one standard endpoint a service's rule set gives
// for the aws partition from its URLs as written, rather than evaluating
// its conditions. It returns the host, {region} standing for the client's
// region when regional, and the region a global endpoint is signed for; or
// why no single endpoint is there to form.
//
// A URL is set aside when it is FIPS, in another partition, or depends on
// a parameter this client never sets. An amazonaws.com host is preferred
// to an api.aws one, which some services give only in dual-stack form; a
// literal host for one region that the regional form produces anyway is
// not a second endpoint.
func endpointOf(ruleSet json.RawMessage, static map[string]any) (host, signingRegion, reason string) {
	var tree any
	if json.Unmarshal(ruleSet, &tree) != nil || tree == nil {
		return "", "", "the model has no endpoint rule set"
	}
	// An operation parameter, one no client setting fills, has only the
	// value the operation's staticContextParams give it, or its default.
	var parameters struct {
		Parameters map[string]struct {
			BuiltIn string `json:"builtIn"`
			Default any    `json:"default"`
		} `json:"parameters"`
	}
	_ = json.Unmarshal(ruleSet, &parameters)
	operationParam := func(argv any) (value any, known, isOperation bool) {
		ref, _ := argv.(map[string]any)["ref"].(string)
		p, ok := parameters.Parameters[ref]
		if !ok || p.BuiltIn != "" {
			return nil, false, false
		}
		if v, ok := static[ref]; ok {
			return v, true, true
		}
		return p.Default, p.Default != nil, true
	}
	// unreachable reports a rule gated on an operation parameter this
	// operation cannot give the value the rule requires.
	unreachable := func(rule map[string]any) bool {
		conditions, _ := rule["conditions"].([]any)
		for _, c := range conditions {
			cond, _ := c.(map[string]any)
			argv, _ := cond["argv"].([]any)
			if len(argv) == 0 {
				continue
			}
			if _, isRef := argv[0].(map[string]any); !isRef {
				continue
			}
			value, known, isOperation := operationParam(argv[0])
			if !isOperation {
				continue
			}
			switch cond["fn"] {
			case "isSet":
				if !known {
					return true
				}
			case "booleanEquals":
				if len(argv) == 2 && (!known || value != argv[1]) {
					return true
				}
			}
		}
		return false
	}
	type candidate struct{ host, signingRegion string }
	tiers := map[string]map[candidate]bool{"amazonaws.com": {}, "api.aws": {}}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if unreachable(t) {
				return
			}
			if t["type"] == "endpoint" {
				endpoint, _ := t["endpoint"].(map[string]any)
				url, _ := endpoint["url"].(string)
				var signing string
				if props, ok := endpoint["properties"].(map[string]any); ok {
					if schemes, ok := props["authSchemes"].([]any); ok && len(schemes) > 0 {
						scheme, _ := schemes[0].(map[string]any)
						signing, _ = scheme["signingRegion"].(string)
					}
				}
				h, ok := strings.CutPrefix(partitionValues.Replace(url), "https://")
				if ok && !strings.ContainsAny(strings.ReplaceAll(h, "{region}", ""), "{}/") &&
					!fips(h) && !otherPartition(h) {
					c := candidate{host: h}
					if !strings.Contains(h, "{region}") {
						c.signingRegion = partitionValues.Replace(signing)
					}
					for suffix, tier := range tiers {
						if strings.HasSuffix(h, "."+suffix) {
							tier[c] = true
						}
					}
				}
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(tree)
	found := tiers["amazonaws.com"]
	if len(found) == 0 {
		found = tiers["api.aws"]
	}
	// A regional form accounts for any literal it produces for one region.
	for c := range found {
		prefix, suffix, regional := strings.Cut(c.host, ".{region}.")
		if !regional {
			continue
		}
		for other := range found {
			rest, ok := strings.CutPrefix(other.host, prefix+".")
			if ok && strings.HasSuffix(rest, "."+suffix) && !strings.Contains(strings.TrimSuffix(rest, "."+suffix), ".") && other != c {
				delete(found, other)
			}
		}
	}
	if len(found) != 1 {
		hosts := make([]string, 0, len(found))
		for c := range found {
			hosts = append(hosts, c.host)
		}
		sort.Strings(hosts)
		return "", "", fmt.Sprintf("the rule set gives %d standard endpoints %v", len(found), hosts)
	}
	for c := range found {
		host, signingRegion = c.host, c.signingRegion
	}
	if !strings.Contains(host, "{region}") && signingRegion != "us-east-1" {
		return "", "", fmt.Sprintf("the global endpoint %s is signed for %q, not us-east-1", host, signingRegion)
	}
	return host, signingRegion, ""
}

// fips reports whether any label of host names a FIPS endpoint.
func fips(host string) bool {
	for label := range strings.SplitSeq(host, ".") {
		if label == "fips" || strings.HasSuffix(label, "-fips") {
			return true
		}
	}
	return false
}

// otherPartition reports whether any label of host is another
// partition's region.
func otherPartition(host string) bool {
	for label := range strings.SplitSeq(host, ".") {
		for _, prefix := range otherPartitionRegions {
			if strings.HasPrefix(label, prefix) {
				return true
			}
		}
	}
	return false
}
