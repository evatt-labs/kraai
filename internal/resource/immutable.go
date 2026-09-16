package resource

import "errors"

// ErrImmutable is returned by Update on a resource type that cannot be
// changed in place.
//
// Most of what kraai provisions is immutable in the only sense that matters
// here: a D1 database cannot be renamed, a KV namespace's title is fixed at
// creation, an R2 bucket is its name. The manifest fields that identify them
// are precisely the ones that cannot change, so "the desired state differs"
// means replace, not modify.
//
// Returning a sentinel rather than silently succeeding is deliberate. A
// no-op Update would let a planner believe it had reconciled a difference it
// had not touched, and the difference would persist invisibly across every
// subsequent run. Failing loudly means the planner has to decide — replace,
// or refuse — rather than being allowed not to notice.
var ErrImmutable = errors.New("resource type cannot be updated in place")

// SecretProducer is implemented by a Resource whose state includes values
// that must not be stored.
//
// An optional interface rather than a method on Resource: most types have no
// credentials, and requiring every one of them to return an empty map would
// put the concept everywhere it is not needed. The applier type-asserts, so a
// provider keeps the knowledge of which of its values are sensitive, and the
// applier keeps the knowledge of when they are needed.
type SecretProducer interface {
	// Secrets returns producers for the credentials this resource's state
	// implies, keyed by name. Each is called at the point of use, so a
	// credential exists only inside the call that needs it.
	Secrets(state *State) map[string]Secret
}
