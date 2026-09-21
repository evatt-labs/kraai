package plan

import (
	"errors"
	"testing"
)

func TestActionKind_String(t *testing.T) {
	cases := map[ActionKind]string{
		ActionCreate:   "create",
		ActionNoChange: "no-change",
		ActionReplace:  "replace",
		ActionFailed:   "failed",
		ActionKind(99): "ActionKind(99)",
	}
	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Errorf("ActionKind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
}

func TestPlan_HasChanges(t *testing.T) {
	cases := []struct {
		name    string
		actions []Action
		want    bool
	}{
		{"empty", nil, false},
		{"only no-change", []Action{{Kind: ActionNoChange}}, false},
		{"only failed", []Action{{Kind: ActionFailed}}, false},
		{"has create", []Action{{Kind: ActionNoChange}, {Kind: ActionCreate}}, true},
		{"has replace", []Action{{Kind: ActionReplace}}, true},
		{"has update", []Action{{Kind: ActionUpdate}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Plan{Actions: c.actions}
			if got := p.HasChanges(); got != c.want {
				t.Errorf("HasChanges() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPlan_HasFailures(t *testing.T) {
	cases := []struct {
		name    string
		actions []Action
		want    bool
	}{
		{"empty", nil, false},
		{"no failures", []Action{{Kind: ActionCreate}, {Kind: ActionNoChange}}, false},
		{"has failure", []Action{{Kind: ActionCreate}, {Kind: ActionFailed, Err: errors.New("x")}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Plan{Actions: c.actions}
			if got := p.HasFailures(); got != c.want {
				t.Errorf("HasFailures() = %v, want %v", got, c.want)
			}
		})
	}
}
