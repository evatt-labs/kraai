package direct

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// The readers_<service>.go files are exactly what the checked-in inputs
// generate, and no other such file exists.
func TestReadersAreGenerated(t *testing.T) {
	want, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := filepath.Glob("readers_*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range onDisk {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is generated for no service; run go generate ./internal/provider/aws/direct", name)
		}
	}
	dir := os.DirFS(".")
	for name, src := range want {
		got, err := fs.ReadFile(dir, name)
		if err != nil || !bytes.Equal(got, src) {
			t.Errorf("%s is not what the overrides generate; run go generate ./internal/provider/aws/direct", name)
		}
	}
	if len(Readers()) != len(readers) || len(readers) == 0 {
		t.Fatalf("Readers() = %d, readers = %d", len(Readers()), len(readers))
	}
}

// The generated readers carry every field Compile produces: a field the generator
// does not write would compile clean and be silently absent at run time.
func TestGeneratedReadersMatchCompiled(t *testing.T) {
	compiled, err := Compile()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range compiled {
		got := readers[want.Type]
		got.Production, got.Mutable = false, false
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the generated reader differs from Compile()\ngot  %#v\nwant %#v", want.Type, got, want)
		}
	}
}

// The compiled readers carry what each protocol needs to address the call.
func TestCompiledReadersAddressTheirCall(t *testing.T) {
	cases := map[string]struct{ protocol, target, method, uri, location string }{
		"AWS::AppConfig::DeploymentStrategy":       {"restJson1", "", "GET", "/deploymentstrategies/{DeploymentStrategyId}", "label"},
		"AWS::XRay::Group":                         {"restJson1", "", "POST", "/GetGroup", "body"},
		"AWS::CodeDeploy::DeploymentConfig":        {"awsJson1_1", "CodeDeploy_20141006.GetDeploymentConfig", "", "", "body"},
		"AWS::AppRunner::AutoScalingConfiguration": {"awsJson1_0", "AppRunner.DescribeAutoScalingConfiguration", "", "", "body"},
	}
	for typeName, c := range cases {
		r := readers[typeName]
		if r.Protocol != c.protocol || r.Target != c.target || r.Method != c.method || r.URI != c.uri ||
			len(r.Identifier) != 1 || r.Identifier[0].Location != c.location {
			t.Errorf("%s = %+v", typeName, r)
		}
	}
}

// edit replaces old with new in one override file of a copy of the inputs.
func edit(t *testing.T, file, old, replacement string) fstest.MapFS {
	t.Helper()
	m := copyFS(t)
	f := m["overrides/"+file]
	if !strings.Contains(string(f.Data), old) {
		t.Fatalf("%s does not contain %q", file, old)
	}
	m["overrides/"+file] = &fstest.MapFile{Data: []byte(strings.Replace(string(f.Data), old, replacement, 1))}
	return m
}

func TestCompileRefuses(t *testing.T) {
	const xray, codedeploy = "AWS--XRay--Group.yaml", "AWS--CodeDeploy--DeploymentConfig.yaml"
	cases := map[string]struct {
		files fstest.MapFS
		want  string
	}{
		"an unmapped property": {edit(t, xray, "  FilterExpression: FilterExpression\n", ""),
			"FilterExpression is neither mapped nor skipped"},
		"a member the response lacks": {edit(t, xray, "FilterExpression: FilterExpression", "FilterExpression: Filter"),
			"maps to Filter, which com.amazonaws.xray#Group does not have"},
		"a type that does not fit": {edit(t, xray, "GroupName: GroupName", "GroupName: InsightsConfiguration"),
			"GroupName is [string] in the schema, but InsightsConfiguration is structure"},
		"a skip nobody reviewed": {edit(t, xray, "Tags: tags are not", "Tags: TODO tags are not"),
			"Tags is skipped without a reviewed reason"},
		"a skip for a property the response carries": {withSkip(edit(t, xray,
			"  FilterExpression: FilterExpression\n", ""), xray, "FilterExpression", "not needed"),
			"FilterExpression is skipped, but member FilterExpression carries it"},
		"a property both mapped and skipped": {withSkip(copyFS(t), xray, "GroupName", "no reason"),
			"GroupName is both mapped and skipped"},
		"a property the schema lacks": {edit(t, xray, "  GroupName: GroupName\n", "  GroupName: GroupName\n  Colour: GroupName\n"),
			"Colour is mapped, but the schema has no such readable property"},
		"an identifier that is not the primary identifier": {edit(t, xray, "    GroupARN: GroupARN\n", "    GroupName: GroupName\n"),
			"identifier binds GroupName, which is not the primary identifier"},
		"a required input member left unbound": {edit(t, codedeploy, "    DeploymentConfigName: deploymentConfigName\n  response", "  response"),
			"the input requires deploymentConfigName, which the identifier does not bind"},
		"a response path that goes nowhere": {edit(t, xray, "response: Group", "response: Groups"),
			"response path Groups"},
		"a structure with no nested mapping": {edit(t, codedeploy,
			"  MinimumHealthyHosts:\n    member: minimumHealthyHosts\n    properties:\n      Type: type\n      Value: value\n",
			"  MinimumHealthyHosts: minimumHealthyHosts\n"),
			"MinimumHealthyHosts.Type is neither mapped nor skipped"},
		"a nested property left out": {edit(t, codedeploy, "      Type: type\n      Value: value\n  TrafficRoutingConfig", "      Type: type\n  TrafficRoutingConfig"),
			"MinimumHealthyHosts.Value is neither mapped nor skipped"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compileAll(c.files)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("compile = %v\nwant an error containing %q", err, c.want)
			}
		})
	}
}

// A service declaring two protocols compiles to the same one every time,
// the JSON protocol its SDKs send.
func TestCompilePicksOneProtocol(t *testing.T) {
	m := widgetModel("Widgets", "widgets")
	svc := m["shapes"].(map[string]any)["com.example#Widgets"].(map[string]any)["traits"].(map[string]any)
	delete(svc, "aws.protocols#awsJson1_1")
	svc["aws.protocols#awsQuery"] = map[string]any{}
	svc["aws.protocols#awsJson1_0"] = map[string]any{}
	svc["aws.protocols#awsQueryCompatible"] = map[string]any{}
	for range 20 {
		r, errs := compileWidget(t, m, widgetOverride("Widget"))
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		if r.Protocol != "awsJson1_0" {
			t.Fatalf("Protocol = %q, want awsJson1_0", r.Protocol)
		}
	}
}
