// Package naming derives every name kraai assigns: ephemeral environment
// names, and the resource and service names within an environment. The
// ephemeral grammar and the derivation are byte-for-byte the JavaScript
// CLI's, because changing either orphans every environment already
// deployed. The persistent environment grammar is this package's own.
//
// This package owns derivation, not lookup: how a resource is found once
// named is the registry's per-type strategy.
package naming
