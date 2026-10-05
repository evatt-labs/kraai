package direct

import (
	"encoding/json"
	"errors"
	"io/fs"
	"sort"
)

// LifecycleEvidence is a lifecycle harness run: each type created, updated
// one property at a time and deleted through its direct mutations, and
// read back through Cloud Control after each.
type LifecycleEvidence struct {
	Types []TypeLifecycle `json:"types"`
}

// TypeLifecycle is one type's lifecycle run.
type TypeLifecycle struct {
	Type string `json:"type"`
	Date string `json:"date"`
	// Outcome is parity when Cloud Control read every step as the direct
	// mutation made it, and differs otherwise.
	Outcome string `json:"outcome"`
	// Updated is every property the run changed and saw changed.
	Updated []string `json:"updated,omitempty"`
	// Override is the SHA-256 of the override the run mutated through.
	Override string `json:"override"`
	// Observed is every error code listed in the override's
	// undeclaredErrors that a direct call in this run answered for
	// absence, the delete of an instance already gone among them.
	Observed []string `json:"observed,omitempty"`
}

// lifecycleTypes is the types whose lifecycle evidence is parity for the
// override they have now, each with the codes the run observed.
func lifecycleTypes(files fs.FS) (map[string][]string, error) {
	raw, err := fs.ReadFile(files, "evidence/lifecycle.json")
	if errors.Is(err, fs.ErrNotExist) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var evidence LifecycleEvidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, e := range evidence.Types {
		if current, err := overrideHash(files, e.Type); e.Outcome == "parity" && err == nil && current == e.Override {
			out[e.Type] = e.Observed
		}
	}
	return out, nil
}

// MergeLifecycle replaces prior's record of each type run with the run's.
func MergeLifecycle(prior, run LifecycleEvidence) LifecycleEvidence {
	byType := map[string]TypeLifecycle{}
	for _, e := range append(prior.Types, run.Types...) {
		byType[e.Type] = e
	}
	var out LifecycleEvidence
	for _, e := range byType {
		out.Types = append(out.Types, e)
	}
	sort.Slice(out.Types, func(i, j int) bool { return out.Types[i].Type < out.Types[j].Type })
	return out
}
