package plan

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// everyOptional implements Resource and every optional interface this
// package defines, so a decorated copy shows which ones the telemetry
// decorator forwards.
type everyOptional struct{}

func (everyOptional) Get(context.Context, resource.Ref) (*resource.State, error) { return nil, nil }
func (everyOptional) Create(context.Context, resource.Spec) (*resource.State, error) {
	return nil, nil
}

func (everyOptional) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	return nil, nil
}
func (everyOptional) Delete(context.Context, resource.Ref) error { return nil }
func (everyOptional) Diff(resource.Spec, *resource.State) (resource.Difference, error) {
	return resource.Same, nil
}

func (everyOptional) DiffLive(context.Context, resource.Spec, *resource.State) (resource.Difference, error) {
	return resource.Same, nil
}
func (everyOptional) ValidateSpec(resource.Spec) error                   { return nil }
func (everyOptional) Locate(resource.Spec) (string, string, bool, error) { return "", "", false, nil }
func (everyOptional) Notes(resource.Spec) []string                       { return nil }

// Every resource the planner sees is decorated, so each interface it
// asserts must survive decoration. Checked against this package's own
// interface types: a signature changed here and not in the decorator's
// forwarder fails this test, where resource's own guard, which spells the
// signatures out because it cannot import this package, would not.
func TestDecoratedResourceKeepsPlanInterfaces(t *testing.T) {
	var _ Differ = everyOptional{}
	var _ LiveDiffer = everyOptional{}
	var _ SpecValidator = everyOptional{}
	var _ Locator = everyOptional{}
	var _ Noter = everyOptional{}

	decorated := resource.Instrument(nil, nil)(resource.Registration{Provider: "p", Type: "T", Resource: everyOptional{}})
	for name, ok := range map[string]bool{
		"Differ":        is[Differ](decorated),
		"LiveDiffer":    is[LiveDiffer](decorated),
		"SpecValidator": is[SpecValidator](decorated),
		"Locator":       is[Locator](decorated),
		"Noter":         is[Noter](decorated),
	} {
		if !ok {
			t.Errorf("a decorated resource does not satisfy %s; add or fix its forwarder in resource/otel.go", name)
		}
	}
}

func is[T any](v any) bool {
	_, ok := v.(T)
	return ok
}
