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

// ReadOnly reports whether ctx was marked by WithReadOnly.
func ReadOnly(ctx context.Context) bool {
	readOnly, _ := ctx.Value(readOnlyKey{}).(bool)
	return readOnly
}
