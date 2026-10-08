package resource

import "context"

type readOnlyKey struct{}

// WithReadOnly marks ctx as a run that never mutates, so a provider may
// answer a lookup from an index that can lag a recent change. A run that
// mutates must never carry it: a stale miss there would create a duplicate
// or orphan a resource.
func WithReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, true)
}

type settledIndexKey struct{}

// WithSettledIndex marks ctx as a run against an environment no mutating
// run has touched for longer than an index can lag, so a provider may take
// an index's miss as absence even though the run mutates: what the index
// has not seen, nothing has created since it could.
func WithSettledIndex(ctx context.Context) context.Context {
	return context.WithValue(ctx, settledIndexKey{}, true)
}

// SettledIndex reports whether ctx was marked by WithSettledIndex.
func SettledIndex(ctx context.Context) bool {
	settled, _ := ctx.Value(settledIndexKey{}).(bool)
	return settled
}

// ReadOnly reports whether ctx was marked by WithReadOnly.
func ReadOnly(ctx context.Context) bool {
	readOnly, _ := ctx.Value(readOnlyKey{}).(bool)
	return readOnly
}

type appliedKey struct{}

// WithApplied marks ctx with the environment's record of what kraai last
// set on each resource (lock.Status.Applied), keyed by Ref.InstanceKey().
func WithApplied(ctx context.Context, applied map[string][]string) context.Context {
	return context.WithValue(ctx, appliedKey{}, applied)
}

// AppliedFrom returns the record WithApplied put on ctx, nil if none.
func AppliedFrom(ctx context.Context) map[string][]string {
	applied, _ := ctx.Value(appliedKey{}).(map[string][]string)
	return applied
}

type fingerprintsKey struct{}

// WithFingerprints marks ctx with the environment's record of the hashes
// of the write-only properties kraai last sent (lock.Status.Fingerprints),
// keyed by Ref.InstanceKey().
func WithFingerprints(ctx context.Context, fingerprints map[string]map[string]string) context.Context {
	return context.WithValue(ctx, fingerprintsKey{}, fingerprints)
}

// FingerprintsFrom returns the record WithFingerprints put on ctx.
func FingerprintsFrom(ctx context.Context) map[string]map[string]string {
	fingerprints, _ := ctx.Value(fingerprintsKey{}).(map[string]map[string]string)
	return fingerprints
}

type tagVersionKey struct{}

// WithTagVersion marks ctx as a run against an environment kraai last
// applied under generation version of its identity tagging, older than this
// kraai's: a type this kraai tags that the environment's kraai did not may
// have instances of the environment's own without the tag.
func WithTagVersion(ctx context.Context, version int) context.Context {
	return context.WithValue(ctx, tagVersionKey{}, version)
}

// AdoptsUntagged reports whether, in ctx, an instance of a type kraai began
// tagging at generation since, answering to a derived name without the tag,
// is one an earlier kraai made: adopted, tagged on this run, rather than
// refused as someone else's. Only when the environment was last applied
// before since; a type tagged since generation 0 never adopts.
func AdoptsUntagged(ctx context.Context, since int) bool {
	version, marked := ctx.Value(tagVersionKey{}).(int)
	return marked && since > 0 && version < since
}
