package direct

import "slices"

// idempotencyTokenMembers lists the members of input the model marks as
// idempotency tokens that the call's templates do not set.
func idempotencyTokenMembers(input smithyShape, templates map[string]any) []string {
	var out []string
	for _, name := range sortedKeys(input.Members) {
		if _, set := templates[name]; !set && isIdempotencyToken(input.Members[name]) {
			out = append(out, name)
		}
	}
	return out
}

func isIdempotencyToken(m smithyMember) bool {
	_, ok := m.Traits["smithy.api#idempotencyToken"]
	return ok
}

// tokenFormMembers is members with the token member, when there is one,
// so the form table encodes it.
func tokenFormMembers(members []string, token string) []string {
	if token == "" {
		return members
	}
	return append(slices.Clone(members), token)
}
