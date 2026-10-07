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

type adoptUntaggedKey struct{}

// WithAdoptUntagged marks ctx as a run against an environment kraai has
// applied before it marked every resource it creates with its identity tag:
// an instance that answers to a derived name without the tag is one an
// earlier kraai made, and is adopted, tagged on this run, rather than
// refused as someone else's.
func WithAdoptUntagged(ctx context.Context) context.Context {
	return context.WithValue(ctx, adoptUntaggedKey{}, true)
}

// AdoptUntagged reports whether ctx was marked by WithAdoptUntagged.
func AdoptUntagged(ctx context.Context) bool {
	adopt, _ := ctx.Value(adoptUntaggedKey{}).(bool)
	return adopt
}
