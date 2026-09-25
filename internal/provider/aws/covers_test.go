package aws

import "testing"

func TestCoversUnorderedArrays(t *testing.T) {
	env := func(pairs ...string) []any {
		var out []any
		for len(pairs) >= 2 {
			out = append(out, map[string]any{"Name": pairs[0], "Value": pairs[1]})
			pairs = pairs[2:]
		}
		return out
	}
	unordered := map[string]bool{"/properties/Env": true, "/properties/Containers/*/Env": true}
	cases := map[string]struct {
		pointer          string
		desired, current any
		want             bool
	}{
		"an ordered array in another order":           {"/properties/Command", []any{"a", "b"}, []any{"b", "a"}, false},
		"an ordered array in order":                   {"/properties/Command", []any{"a", "b"}, []any{"a", "b"}, true},
		"an unordered array in another order":         {"/properties/Env", env("A", "1", "B", "2"), env("B", "2", "A", "1"), true},
		"an unordered array with another element":     {"/properties/Env", env("A", "1", "B", "2"), env("B", "2", "A", "9"), false},
		"an unordered array of another length":        {"/properties/Env", env("A", "1"), env("A", "1", "B", "2"), false},
		"an unordered array nested in an ordered one": {"/properties/Containers", []any{map[string]any{"Env": env("A", "1", "B", "2")}}, []any{map[string]any{"Env": env("B", "2", "A", "1")}}, true},
		// Partial elements: the first desired element fits both current
		// ones, the second only the first. Pairing greedily in order would
		// take the first for the first and leave the second unmatched.
		"a matching a greedy pairing misses": {"/properties/Env",
			[]any{map[string]any{"Name": "A"}, map[string]any{"Name": "A", "Value": "2"}},
			[]any{map[string]any{"Name": "A", "Value": "2"}, map[string]any{"Name": "A", "Value": "3"}}, true},
		"no matching at all": {"/properties/Env",
			[]any{map[string]any{"Name": "A", "Value": "2"}, map[string]any{"Name": "A", "Value": "2"}},
			[]any{map[string]any{"Name": "A", "Value": "2"}, map[string]any{"Name": "A", "Value": "3"}}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := covers(c.desired, c.current, c.pointer, unordered); got != c.want {
				t.Fatalf("covers = %v, want %v", got, c.want)
			}
		})
	}
}
