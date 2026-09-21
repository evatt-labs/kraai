package resource

import "errors"

// ErrImmutable is returned by Update on a resource type that cannot be
// changed in place. Most of what kraai provisions is: a D1 database cannot
// be renamed, an R2 bucket is its name. The fields that identify them are
// the ones that cannot change, so a difference means replace.
//
// A sentinel rather than a silent no-op, so the planner has to decide,
// replace or refuse, instead of believing it reconciled a difference it
// never touched.
var ErrImmutable = errors.New("resource type cannot be updated in place")

// SecretProducer is implemented by a Resource whose state includes values
// that must not be stored. Optional rather than a method on Resource: most
// types have no credentials. The provider keeps the knowledge of which
// values are sensitive, and the applier the knowledge of when they are
// needed.
type SecretProducer interface {
	// Secrets returns producers for the credentials this resource's state
	// implies, keyed by name. Each is called at the point of use.
	Secrets(state *State) map[string]Secret
}
