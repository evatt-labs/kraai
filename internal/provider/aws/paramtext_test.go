package aws

import "testing"

// A number or boolean a manifest writes matches the text a service keeps it
// as; a different value, or text that is no number, still differs.
func TestCoversScalarAgainstItsText(t *testing.T) {
	for _, c := range []struct {
		desired, current any
		want             bool
	}{
		{map[string]any{"max_connections": 100.0}, map[string]any{"max_connections": "100"}, true},
		{map[string]any{"ssl": true}, map[string]any{"ssl": "true"}, true},
		{map[string]any{"max_connections": 100.0}, map[string]any{"max_connections": "200"}, false},
		{map[string]any{"max_connections": 100.0}, map[string]any{"max_connections": "many"}, false},
		{map[string]any{"name": "100"}, map[string]any{"name": 100.0}, false},
	} {
		if got := covers(c.desired, c.current, "/properties/Parameters", listRules{}); got != c.want {
			t.Errorf("covers(%v, %v) = %v, want %v", c.desired, c.current, got, c.want)
		}
	}
}
