package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// --- direct unit tests for the pure rendering helpers ---

func TestActionSymbol(t *testing.T) {
	cases := []struct {
		kind plan.ActionKind
		want string
	}{
		{plan.ActionCreate, "+"},
		{plan.ActionReplace, "~"},
		{plan.ActionFailed, "!"},
		{plan.ActionNoChange, "="},
		{plan.ActionKind(99), "?"}, // unknown kind
	}
	for _, tc := range cases {
		if got := actionSymbol(tc.kind); got != tc.want {
			t.Errorf("actionSymbol(%v) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// twoWavePlan builds a *plan.Plan directly (bypassing Planner) spanning
// two waves with every ActionKind, so writePlanText/toPlanDocument are
// exercised over the "wave changes mid-list" branch and every action-kind
// branch without needing a real manifest/registry for each case.
func twoWavePlan() *plan.Plan {
	return &plan.Plan{
		Actions: []plan.Action{
			{
				Item: plan.Item{ServiceKey: "api", Binding: "DB", Capability: manifest.CapabilityDatabase,
					Provider: "neon", Type: "branch", Wave: 0},
				Ref:  resource.Ref{Provider: "neon", Type: "branch", Name: "env-api-db"},
				Kind: plan.ActionCreate,
			},
			{
				Item: plan.Item{ServiceKey: "api", Binding: "DB", Capability: manifest.CapabilityDatabase,
					Provider: "neon", Type: "branch", Wave: 0},
				Ref:  resource.Ref{Provider: "neon", Type: "branch", Name: "env-api-db2"},
				Kind: plan.ActionReplace,
			},
			{
				Item: plan.Item{ServiceKey: "api", Binding: "CACHE", Capability: manifest.CapabilityKeyValue,
					Provider: "fake", Type: "kv", Wave: 1},
				Ref:  resource.Ref{Provider: "fake", Type: "kv", Name: "env-api-cache"},
				Kind: plan.ActionNoChange,
			},
			{
				Item: plan.Item{ServiceKey: "api", Binding: "QUEUE", Capability: manifest.CapabilityQueues,
					Provider: "fake", Type: "queue", Wave: 1},
				Ref:  resource.Ref{Provider: "fake", Type: "queue", Name: "env-api-queue"},
				Kind: plan.ActionFailed,
				Err:  errors.New("queue api down"),
			},
		},
	}
}

func TestWritePlanText_MultiWaveAndEveryKind(t *testing.T) {
	var buf bytes.Buffer
	if err := writePlanText(&buf, "env", twoWavePlan()); err != nil {
		t.Fatalf("writePlanText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"1 to create, 0 to update, 1 to replace, 1 unchanged, 1 failed (4 total)",
		"wave 0:", "wave 1:",
		"+", "~", "=", "!",
		"queue api down",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
}

func TestToPlanDocument_EveryKindAndWave(t *testing.T) {
	doc := toPlanDocument("env", twoWavePlan())

	if doc.Summary != (planSummaryJSON{Create: 1, Replace: 1, NoChange: 1, Failed: 1, Total: 4, HasChanges: true, HasFailures: true}) {
		t.Errorf("Summary = %+v", doc.Summary)
	}
	if len(doc.Actions) != 4 {
		t.Fatalf("Actions = %+v", doc.Actions)
	}
	if doc.Actions[0].Wave != 0 || doc.Actions[2].Wave != 1 {
		t.Errorf("Actions waves = %d, %d", doc.Actions[0].Wave, doc.Actions[2].Wave)
	}
	if doc.Actions[3].Kind != "failed" || doc.Actions[3].Error != "queue api down" {
		t.Errorf("Actions[3] = %+v, want the failed action with its error", doc.Actions[3])
	}
}

func TestToPlanDocument_NilPlan(t *testing.T) {
	doc := toPlanDocument("env", nil)
	if doc.Environment != "env" || doc.Summary.Total != 0 || len(doc.Actions) != 0 {
		t.Errorf("toPlanDocument(nil) = %+v, want an empty document", doc)
	}
}

func TestCountActions_NilPlanIsZero(t *testing.T) {
	c := countActions(nil)
	if c != (actionCounts{}) {
		t.Errorf("countActions(nil) = %+v, want zero value", c)
	}
}

func TestSummaryLine_Format(t *testing.T) {
	c := actionCounts{Create: 1, Replace: 2, NoChange: 3, Failed: 4}
	got := summaryLine("env", c)
	want := `plan for "env": 1 to create, 0 to update, 2 to replace, 3 unchanged, 4 failed (10 total)`
	if got != want {
		t.Errorf("summaryLine() = %q, want %q", got, want)
	}
}

func TestWritePlanText_WriterFailurePropagates(t *testing.T) {
	err := writePlanText(failingWriter{}, "env", nil)
	if err == nil {
		t.Fatalf("writePlanText with a failing writer = nil error, want one")
	}
}

func TestWritePlanJSON_WriterFailurePropagates(t *testing.T) {
	err := writePlanJSON(failingWriter{}, "env", nil, nil)
	if err == nil {
		t.Fatalf("writePlanJSON with a failing writer = nil error, want one")
	}
}

func TestWritePlanText_NilPlan(t *testing.T) {
	var buf bytes.Buffer
	if err := writePlanText(&buf, "env", nil); err != nil {
		t.Fatalf("writePlanText: %v", err)
	}
	if !strings.Contains(buf.String(), "no resources declared") {
		t.Errorf("output = %q, want the empty-plan message", buf.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write boom") }

// rolePlan has one action whose registry key diverges from the vendor's own
// type name and one where they agree — the two cases the output has to tell
// apart.
func rolePlan() *plan.Plan {
	return &plan.Plan{Actions: []plan.Action{
		{
			Item: plan.Item{
				ServiceKey: "api", Binding: "api", Capability: "compute",
				Provider: "aws", Type: "AWS::S3::Bucket::ArtifactBucket",
				VendorType: "AWS::S3::Bucket", Wave: 0,
			},
			Ref:  resource.Ref{Provider: "aws", Type: "AWS::S3::Bucket", Name: "env-api-artifacts"},
			Kind: plan.ActionCreate,
		},
		{
			Item: plan.Item{
				ServiceKey: "api", Binding: "api", Capability: "compute",
				Provider: "aws", Type: "AWS::Lambda::Function",
				VendorType: "AWS::Lambda::Function", Wave: 0,
			},
			Ref:  resource.Ref{Provider: "aws", Type: "AWS::Lambda::Function", Name: "env-api"},
			Kind: plan.ActionCreate,
		},
	}}
}

// A role key names nothing an operator can find in a vendor console, so the
// text output names the type they can — and only then, because printing it on
// every row would repeat the type verbatim for nearly every resource.
func TestWritePlanText_NamesTheVendorTypeOnlyWhenItDiffers(t *testing.T) {
	var buf bytes.Buffer
	if err := writePlanText(&buf, "env", rolePlan()); err != nil {
		t.Fatalf("writePlanText: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "aws/AWS::S3::Bucket::ArtifactBucket (AWS::S3::Bucket)") {
		t.Errorf("a diverging type did not name its vendor type:\n%s", out)
	}
	if strings.Contains(out, "AWS::Lambda::Function (AWS::Lambda::Function)") {
		t.Errorf("a type equal to its vendor type was printed twice:\n%s", out)
	}
}

// vendor_type is always present, equal to type in the common case, so a
// consumer reads one field rather than branching on whether it diverges.
func TestPlanJSON_CarriesVendorTypeOnEveryAction(t *testing.T) {
	doc := toPlanDocument("env", rolePlan())

	got := map[string]string{}
	for _, a := range doc.Actions {
		got[a.Type] = a.VendorType
	}
	want := map[string]string{
		"AWS::S3::Bucket::ArtifactBucket": "AWS::S3::Bucket",
		"AWS::Lambda::Function":           "AWS::Lambda::Function",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("vendor_type by type = %v, want %v", got, want)
	}
}

// A resource's notes print under its action, and are carried in the JSON
// document; an action without notes carries none.
func TestPlanNotesAreRendered(t *testing.T) {
	p := twoWavePlan()
	p.Actions[0].Notes = []string{"found by RouteKey: changing it leaves the old one unmanaged"}

	var buf bytes.Buffer
	if err := writePlanText(&buf, "env", p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "note: found by RouteKey: changing it leaves the old one unmanaged") {
		t.Fatalf("text output = %q", buf.String())
	}

	buf.Reset()
	if err := writePlanJSON(&buf, "env", p, nil); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Actions []struct {
			Notes []string `json:"notes"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Actions[0].Notes) != 1 || doc.Actions[1].Notes != nil {
		t.Fatalf("JSON notes = %v, %v", doc.Actions[0].Notes, doc.Actions[1].Notes)
	}
}
