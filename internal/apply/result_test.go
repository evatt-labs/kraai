package apply

import "testing"

func TestResult_HasFailures(t *testing.T) {
	if (&Result{}).HasFailures() {
		t.Errorf("empty Result.HasFailures() = true, want false")
	}
	r := &Result{Results: []ActionResult{{Outcome: OutcomeCreated}, {Outcome: OutcomeFailed}}}
	if !r.HasFailures() {
		t.Errorf("HasFailures() = false, want true")
	}
	var nilResult *Result
	if nilResult.HasFailures() {
		t.Errorf("nil Result.HasFailures() = true, want false")
	}
}

func TestOutcome_String(t *testing.T) {
	cases := map[Outcome]string{
		OutcomeCreated:   "created",
		OutcomeUnchanged: "unchanged",
		OutcomeReplaced:  "replaced",
		OutcomeFailed:    "failed",
		OutcomeSkipped:   "skipped",
		Outcome(99):      "Outcome(99)",
	}
	for o, want := range cases {
		if got := o.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", int(o), got, want)
		}
	}
}
