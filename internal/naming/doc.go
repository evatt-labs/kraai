// Package naming ports kraai 0.5.0's JS naming derivation
// (src/names.mjs in the removed JavaScript CLI) to Go, byte-for-byte —
// changing any of these orphans every environment already deployed by
// 0.4.x/0.5.x, independent of the language rewrite.
//
// It also owns two things the JS implementation never had:
//
//   - The persistent environment name grammar, distinct from the frozen
//     ephemeral one.
//   - Import-reference resolution: given a
//     resources.<service>.<kind>.<binding> entry in an environment
//     overlay (internal/manifest's ResourceImports/ImportRef), the
//     declared { id | name } already *is* that resource's identity — no
//     further derivation happens. This package only validates that
//     exactly one of the two is set.
//
// Resource identity *lookup* strategy (by name, by a provider's own
// lookup API, by a unique attribute, or by a kraai-owned tag) and the
// identity cache that maps a manifest path to a provider-assigned id are
// out of scope entirely: this package owns name derivation, not lookup,
// and never conflicts with either.
package naming
