package resource

import (
	"context"

	"github.com/evatt-labs/kraai/internal/secretref"
)

// SecretRefResolver is implemented by a Resource whose Spec may carry
// secret references (internal/secretref) among otherwise ordinary values,
// and that knows how to resolve them with its own provider's credentials.
// Optional: most types accept no reference at all.
//
// Split from SecretProducer on purpose. A SecretProducer publishes a
// credential a resource's own state implies, for a sibling binding to
// consume; a SecretRefResolver reads a reference the manifest wrote and
// fetches it from an external store the resource itself has no other
// relationship to.
type SecretRefResolver interface {
	// SecretRefs returns every secret reference spec's Config declares, so
	// a caller can resolve each exactly once before any resource is
	// mutated. A Config with none returns a nil slice and no error.
	SecretRefs(spec Spec) ([]secretref.Ref, error)

	// ResolveSecretRef returns a producer for ref's value. Resolving
	// nothing itself: the returned Secret performs the provider call, so a
	// caller controls exactly when, and how many times, the network is
	// touched.
	ResolveSecretRef(ctx context.Context, ref secretref.Ref) (Secret, error)
}
