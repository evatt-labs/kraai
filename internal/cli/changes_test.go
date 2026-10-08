package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/redact"
	"github.com/evatt-labs/kraai/internal/resource"
)

func changedPlan(changes []resource.Change, notes []string, err error) *plan.Plan {
	kind := plan.ActionUpdate
	if err != nil {
		kind = plan.ActionFailed
	}
	return &plan.Plan{Actions: []plan.Action{{
		Item: plan.Item{ServiceKey: "api", Binding: "LOGS", Provider: "aws", Type: "AWS::Logs::LogGroup", VendorType: "AWS::Logs::LogGroup"},
		Ref:  resource.Ref{Provider: "aws", Type: "AWS::Logs::LogGroup", Name: "env-api-logs"},
		Kind: kind, Changes: changes, Notes: notes, Err: err,
	}}}
}

// Each change prints under its action, and in the JSON document.
func TestPlanShowsItsChanges(t *testing.T) {
	p := changedPlan([]resource.Change{
		{Property: "KmsKeyId", Kind: resource.ChangeAdd, After: "k"},
		{Property: "RetentionInDays", Kind: resource.ChangeUpdate, Before: 14.0, After: 30.0},
		{Property: "Tags", Kind: resource.ChangeRemove, Before: []any{map[string]any{"Key": "team", "Value": "core"}}},
	}, nil, nil)
	var text bytes.Buffer
	if err := writePlanText(context.Background(), &text, "env", p); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`+ KmsKeyId: "k"`, "~ RetentionInDays: 14 -> 30", `- Tags: [{"Key":"team","Value":"core"}]`} {
		if !strings.Contains(text.String(), line) {
			t.Errorf("the plan lacks %q:\n%s", line, text.String())
		}
	}
	doc := toPlanDocument(context.Background(), "env", p)
	want := []planChangeJSON{
		{Property: "KmsKeyId", Kind: "add", After: `"k"`},
		{Property: "RetentionInDays", Kind: "change", Before: "14", After: "30"},
		{Property: "Tags", Kind: "remove", Before: `[{"Key":"team","Value":"core"}]`},
	}
	got := doc.Actions[0].Changes
	if len(got) != len(want) {
		t.Fatalf("changes = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A long value is cut, marked so.
func TestALongValueIsCut(t *testing.T) {
	got := shownValue(context.Background(), strings.Repeat("a", 500))
	if len(got) != maxShownValue || !strings.HasSuffix(got, "...") {
		t.Fatalf("shownValue = %q (%d bytes)", got, len(got))
	}
}

// Credential shapes are scrubbed from everything shown, though nothing
// marked them sensitive.
func TestTokenShapesAreScrubbed(t *testing.T) {
	for name, c := range map[string]struct{ in, leak, want string }{
		"an AWS access key":       {"key AKIAIOSFODNN7EXAMPLE used", "AKIAIOSFODNN7EXAMPLE", "[aws access key]"},
		"an STS access key":       {"ASIAQWERTYUIOPASDFGH", "ASIAQWERTYUIOPASDFGH", "[aws access key]"},
		"a GitHub token":          {"token ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdefghij", "[github token]"},
		"a fine-grained token":    {"github_pat_11ABCDEFG0123456789_abcdefghijklmnop", "github_pat_11", "[github token]"},
		"a connection password":   {"postgres://app:s3cr3t-pw@db.example.com/app", "s3cr3t-pw", "postgres://app:[password]@db.example.com"},
		"a private key":           {"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----", "MIIEow", "[private key]"},
		"a truncated private key": {"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNz", "b3BlbnNz", "[private key]"},
	} {
		t.Run(name, func(t *testing.T) {
			got := shown(context.Background(), c.in)
			if strings.Contains(got, c.leak) || !strings.Contains(got, c.want) {
				t.Fatalf("shown(%q) = %q", c.in, got)
			}
		})
	}
	if got := shown(context.Background(), "https://example.com/path"); got != "https://example.com/path" {
		t.Fatalf("a URL with no password was changed: %q", got)
	}
}

// Whatever its characters, and wherever it sits in a change's values, a
// note or an error, a value the command must not print never appears in
// the text plan or the JSON document.
func TestASensitiveValueNeverLeavesThePlan(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		secret := rapid.StringMatching(`[A-Za-z0-9"\\<>&!@#%^*()'/:-]{12,40}`).Draw(t, "secret")
		prefix := rapid.StringMatching(`[a-z ]{0,8}`).Draw(t, "prefix")
		embedded := prefix + secret + rapid.StringMatching(`[a-z ]{0,8}`).Draw(t, "suffix")
		value := rapid.SampledFrom([]any{
			embedded,
			[]any{"x", embedded},
			map[string]any{"Variables": map[string]any{"TOKEN": embedded}},
			map[string]any{embedded: 1.0},
		}).Draw(t, "value")
		where := rapid.IntRange(0, 3).Draw(t, "where")

		set := &redact.Set{}
		set.Add(secret, "terraform.base.token")
		ctx := redact.With(context.Background(), set)
		var changes []resource.Change
		var notes []string
		var failure error
		var text, doc bytes.Buffer
		switch where {
		case 0:
			// The live value is the earlier one of the same secret, which
			// nothing marked sensitive.
			earlier := rapid.StringMatching(`[A-Za-z0-9]{16,24}`).Draw(t, "earlier")
			changes = []resource.Change{{Property: "Environment", Kind: resource.ChangeUpdate, Before: earlier, After: value}}
			defer func() {
				for _, out := range []string{text.String(), doc.String()} {
					if strings.Contains(out, earlier) {
						t.Fatalf("the earlier value %q escaped into:\n%s", earlier, out)
					}
				}
			}()
		case 1:
			changes = []resource.Change{{Property: "Environment", Kind: resource.ChangeAdd, After: value}}
		case 2:
			notes = []string{"note " + embedded}
		case 3:
			failure = errors.New("failed: " + embedded)
		}
		p := changedPlan(changes, notes, failure)

		if err := writePlanText(ctx, &text, "env", p); err != nil {
			t.Fatal(err)
		}
		if err := writePlanJSON(ctx, &doc, "env", p, nil); err != nil {
			t.Fatal(err)
		}
		escaped, _ := json.Marshal(secret)
		for _, out := range []string{text.String(), doc.String()} {
			if strings.Contains(out, secret) || strings.Contains(out, string(escaped[1:len(escaped)-1])) {
				t.Fatalf("the secret %q escaped into:\n%s", secret, out)
			}
		}
	})
}
