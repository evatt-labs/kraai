//go:build ignore

// Package rules holds this repository's ruleguard rules, loaded by
// gocritic's ruleguard check (see .golangci.yml). Every file here carries
// a "//go:build ignore" tag: these are not compiled into kraai, only
// parsed by ruleguard itself, which is why the dsl import below is not an
// ordinary dependency (see go.mod's "tool" directive, which is what keeps
// it resolvable without go mod tidy pruning it or go build ./... trying to
// compile it).
package rules

import "github.com/quasilyte/go-ruleguard/dsl"

// getReturnsNilNilWithoutOwnershipCheck flags a method named Get, declared
// in internal/provider/aws, whose body returns (nil, nil) without the body
// also calling something named owned, OwnsBucket, or owns anywhere.
//
// AWS::S3::Bucket names are unique globally, not per account: Cloud
// Control's GetResource for a byName type resolves that global namespace
// with no ownership check of its own (see artifactbucket.go's Get, whose
// doc comment records this reproduced live against a real account —
// GetResource succeeding for a bucket a stranger owns). A Get that returns
// (nil, nil) for "not found" without also checking ownership will read a
// foreign bucket as "ours, exists, no change needed" instead of "absent,
// go create it" — the exact bug artifactbucket.go's Get and Delete both
// now guard against.
//
// This is a narrow, textual check: it flags the shape "a Get method
// returns nil, nil, and the string 'owned'/'OwnsBucket'/'owns' never
// appears anywhere in that method's source" — it cannot know whether a
// call to one of those actually gates the return, only whether the method
// is silent about ownership altogether. A Get that both returns (nil, nil)
// unconditionally AND separately calls owned() somewhere else in its body
// would not be caught by the negative check; that shape does not exist in
// this package today. It also cannot fire on a Get with no explicit
// "nil, nil" return spelled exactly that way (a named return, or a
// variable holding two nils, would not match) — those are out of reach
// for a syntactic rule.
func getReturnsNilNilWithoutOwnershipCheck(m dsl.Matcher) {
	m.Match(`func ($_ $_) Get($*_) ($_, error) { $*_ }`).
		Where(m.File().PkgPath.Matches(`internal/provider/aws$`) &&
			m["$$"].Text.Matches(`(?s)return nil, nil`) &&
			!m["$$"].Text.Matches(`(?s)(owned|OwnsBucket|owns)\(`)).
		Report(`Get returns (nil, nil) somewhere but never calls owned/OwnsBucket/owns — a byName type in this package resolves a global AWS namespace (S3 bucket names are unique account-wide), so "not found" must be gated on ownership or a foreign resource will be read as this account's own. See artifactbucket.go's Get for the pattern.`)
}
