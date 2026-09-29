package direct

import (
	cryptorand "crypto/rand"

	smithyrand "github.com/aws/smithy-go/rand"
)

// tokens make the random UUIDs the SDK fills an idempotency member with.
var tokens = smithyrand.NewUUIDIdempotencyToken(cryptorand.Reader)

// tokenBindings is the request bindings for a fresh token of m's token
// member. It is made once per logical call, so every attempt carries the
// same token.
func tokenBindings(protocol string, m MutationCall) ([]Binding, error) {
	token, err := tokens.GetIdempotencyToken()
	if err != nil {
		return nil, err
	}
	if !isQuery(protocol) {
		return []Binding{{Member: m.TokenMember, Location: "body", Structured: token}}, nil
	}
	return formBindings(protocol, m.Form, m.TokenMember, token)
}

// createPolicy is which failures a create may be sent again after. One
// carrying an idempotency token, or named so that a repeat is idempotent,
// makes no second instance; any other might, so only a throttle is retried.
func createPolicy(m MutationCall) retryPolicy {
	if m.TokenMember != "" || m.Idempotent {
		return retryTransient
	}
	return retryThrottled
}
