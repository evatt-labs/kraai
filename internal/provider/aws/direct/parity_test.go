package direct

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	cases := map[string]struct {
		typeName   string
		cc, direct string
		want       []string
	}{
		"identical": {"AWS::CodeDeploy::DeploymentConfig",
			`{"ComputePlatform":"Server","MinimumHealthyHosts":{"Type":"HOST_COUNT","Value":1}}`,
			`{"ComputePlatform":"Server","MinimumHealthyHosts":{"Type":"HOST_COUNT","Value":1}}`, nil},
		"numbers by value": {"AWS::AppConfig::DeploymentStrategy", `{"GrowthFactor":100}`, `{"GrowthFactor":100.0}`, nil},
		"a different number": {"AWS::AppConfig::DeploymentStrategy", `{"GrowthFactor":100}`, `{"GrowthFactor":100.5}`,
			[]string{"GrowthFactor"}},
		"a skipped property": {"AWS::XRay::Group", `{"GroupName":"Default","Tags":[]}`, `{"GroupName":"Default"}`, nil},
		"a property only Cloud Control has": {"AWS::XRay::Group", `{"GroupName":"Default","FilterExpression":"x"}`, `{"GroupName":"Default"}`,
			[]string{"FilterExpression"}},
		"a property only the direct read has": {"AWS::XRay::Group", `{"GroupName":"Default"}`, `{"GroupName":"Default","FilterExpression":"x"}`,
			[]string{"FilterExpression"}},
		"a nested difference": {"AWS::CodeDeploy::DeploymentConfig",
			`{"MinimumHealthyHosts":{"Type":"HOST_COUNT","Value":1}}`, `{"MinimumHealthyHosts":{"Type":"FLEET_PERCENT","Value":1}}`,
			[]string{"MinimumHealthyHosts.Type"}},
		"a list element": {"AWS::Bedrock::IntelligentPromptRouter",
			`{"Models":[{"ModelArn":"a"},{"ModelArn":"b"}]}`, `{"Models":[{"ModelArn":"a"},{"ModelArn":"c"}]}`,
			[]string{"Models[1].ModelArn"}},
		"a list of another length": {"AWS::Bedrock::IntelligentPromptRouter",
			`{"Models":[{"ModelArn":"a"}]}`, `{"Models":[{"ModelArn":"a"},{"ModelArn":"b"}]}`, []string{"Models"}},
		"a string that looks like a number": {"AWS::XRay::Group", `{"GroupName":"1"}`, `{"GroupName":1}`, []string{"GroupName"}},
		"an unordered list in another order": {"AWS::ECS::TaskDefinition",
			`{"ContainerDefinitions":[{"Environment":[{"Name":"A","Value":"1"},{"Name":"B","Value":"2"}]}]}`,
			`{"ContainerDefinitions":[{"Environment":[{"Name":"B","Value":"2"},{"Name":"A","Value":"1"}]}]}`, nil},
		"an unordered list with another element": {"AWS::ECS::TaskDefinition",
			`{"ContainerDefinitions":[{"Environment":[{"Name":"A","Value":"1"},{"Name":"B","Value":"2"}]}]}`,
			`{"ContainerDefinitions":[{"Environment":[{"Name":"B","Value":"2"},{"Name":"A","Value":"9"}]}]}`,
			[]string{"ContainerDefinitions[0].Environment[0].Value"}},
		"an ordered list in another order": {"AWS::ECS::TaskDefinition",
			`{"ContainerDefinitions":[{"Command":["a","b"]}]}`, `{"ContainerDefinitions":[{"Command":["b","a"]}]}`,
			[]string{"ContainerDefinitions[0].Command[0]", "ContainerDefinitions[0].Command[1]"}},
		"an empty list and an empty map against absent": {"AWS::ECS::TaskDefinition",
			`{"InferenceAccelerators":[],"ContainerDefinitions":[{"Name":"x","DockerLabels":{}}]}`,
			`{"ContainerDefinitions":[{"Name":"x"}]}`, nil},
		"a list against absent": {"AWS::ECS::TaskDefinition",
			`{"ContainerDefinitions":[{"Name":"x","Links":["y"]}]}`, `{"ContainerDefinitions":[{"Name":"x"}]}`,
			[]string{"ContainerDefinitions[0].Links"}},
		"an empty string against absent": {"AWS::XRay::Group", `{"GroupName":"Default","FilterExpression":""}`, `{"GroupName":"Default"}`,
			[]string{"FilterExpression"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// Decoded as each side decodes: the SDK's plain Unmarshal for
			// Cloud Control, UseNumber for the direct client, which keeps
			// 100.0 as it was written.
			var cc, direct map[string]any
			if err := json.Unmarshal([]byte(c.cc), &cc); err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(strings.NewReader(c.direct))
			dec.UseNumber()
			if err := dec.Decode(&direct); err != nil {
				t.Fatal(err)
			}
			diffs, err := Compare(c.typeName, cc, direct)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range diffs {
				got = append(got, d.Property)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("differences = %v, want %v", got, c.want)
			}
		})
	}
	if _, err := Compare("AWS::Nope::Thing", nil, nil); err == nil {
		t.Fatal("a type with no override was compared")
	}
}

// The repository is public: recorded evidence names types, properties and
// outcomes, never an account, an ARN or a value.
func TestEvidenceCarriesNothingFromTheAccount(t *testing.T) {
	raw, err := os.ReadFile("evidence/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []*regexp.Regexp{regexp.MustCompile(`\b\d{12}\b`), regexp.MustCompile(`arn:aws`)} {
		if leak.Match(raw) {
			t.Fatalf("evidence/parity.json matches %s", leak)
		}
	}
	var evidence Evidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evidence.Types {
		if _, ok := readers[e.Type]; !ok {
			t.Errorf("evidence for %s, which has no reader", e.Type)
		}
		if e.SmithyCommit != lock.SmithyCommit {
			t.Errorf("evidence for %s was taken at %s; the lock is at %s", e.Type, e.SmithyCommit, lock.SmithyCommit)
		}
	}
}

func TestMergeEvidence(t *testing.T) {
	rec := func(typeName, outcome, date string) TypeEvidence {
		return TypeEvidence{Type: typeName, Outcome: outcome, Date: date}
	}
	prior := Evidence{Types: []TypeEvidence{
		rec("AWS::A::Kept", "parity", "d1"),
		rec("AWS::B::Rerun", "parity", "d1"),
		rec("AWS::C::Empty", "parity", "d1"),
		rec("AWS::D::Gone", "parity", "d1"),
		rec("AWS::E::WasEmpty", "no-instances", "d1"),
		rec("AWS::F::Regressed", "parity", "d1"),
		{Type: "AWS::H::Edited", Outcome: "parity", Date: "d1", Override: "old"},
	}}
	run := Evidence{Types: []TypeEvidence{
		rec("AWS::B::Rerun", "parity", "d2"),
		rec("AWS::C::Empty", "no-instances", "d2"),
		rec("AWS::E::WasEmpty", "unlisted", "d2"),
		rec("AWS::F::Regressed", "differs", "d2"),
		rec("AWS::G::New", "no-instances", "d2"),
		{Type: "AWS::H::Edited", Outcome: "no-instances", Date: "d2", Override: "new"},
	}}
	readers := map[string]bool{}
	for _, name := range []string{"AWS::A::Kept", "AWS::B::Rerun", "AWS::C::Empty", "AWS::E::WasEmpty", "AWS::F::Regressed", "AWS::G::New", "AWS::H::Edited"} {
		readers[name] = true
	}
	got := map[string]string{}
	for _, e := range MergeEvidence(prior, run, readers).Types {
		got[e.Type] = e.Outcome + "@" + e.Date
	}
	want := map[string]string{
		"AWS::A::Kept":      "parity@d1",       // not run
		"AWS::B::Rerun":     "parity@d2",       // rerun
		"AWS::C::Empty":     "parity@d1",       // inconclusive does not undo parity
		"AWS::E::WasEmpty":  "unlisted@d2",     // inconclusive replaces inconclusive
		"AWS::F::Regressed": "differs@d2",      // a regression replaces parity
		"AWS::G::New":       "no-instances@d2", // first record
		"AWS::H::Edited":    "no-instances@d2", // parity for an earlier override proves nothing
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %v\nwant     %v", got, want)
	}
}
