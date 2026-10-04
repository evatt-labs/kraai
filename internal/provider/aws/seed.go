package aws

import (
	"maps"
	"slices"
	"strings"
)

// seedProperties are write-only properties sent when a resource is created
// and never on update. A read cannot return them, so a value changed outside
// kraai, a rotated secret most of all, is indistinguishable from the one the
// manifest holds, and re-sending the manifest's value with an unrelated
// change would revert it. Most write-only properties are configuration whose
// only way in is to be sent again, so a property is listed here by hand,
// never derived from the schema.
var seedProperties = map[string][]string{
	// The update handler regenerates the value from GenerateSecretString
	// whenever it is sent.
	"AWS::SecretsManager::Secret": {"GenerateSecretString", "SecretString"},
}

// withoutSeeds returns config less typeName's seed properties, or config
// itself when it sets none.
func withoutSeeds(typeName string, config map[string]any) map[string]any {
	seeds := seedsSet(typeName, config)
	if len(seeds) == 0 {
		return config
	}
	out := maps.Clone(config)
	for _, name := range seeds {
		delete(out, name)
	}
	return out
}

// seedNotes tells the author which seed properties config sets.
func seedNotes(typeName string, config map[string]any) []string {
	seeds := seedsSet(typeName, config)
	if len(seeds) == 0 {
		return nil
	}
	return []string{strings.Join(seeds, ", ") + " applied at create only: a later change is not sent, and a value rotated outside kraai is kept"}
}

func seedsSet(typeName string, config map[string]any) []string {
	var set []string
	for _, name := range seedProperties[typeName] {
		if _, ok := config[name]; ok {
			set = append(set, name)
		}
	}
	slices.Sort(set)
	return set
}
