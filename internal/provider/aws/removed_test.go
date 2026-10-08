package aws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
	"github.com/evatt-labs/kraai/internal/resource"
)

// A property kraai set (Spec.Applied) that the manifest stopped declaring
// plans an update while the instance still carries it, unless only a
// replace or nothing could reset it.
func TestDiffPlansARemovedProperty(t *testing.T) {
	for name, c := range map[string]struct {
		typeName string
		applied  []string
		config   map[string]any
		current  map[string]any
		tagged   bool
		want     resource.Difference
	}{
		"removed and still set": {typeName: "AWS::Logs::LogGroup", applied: []string{"LogGroupName", "RetentionInDays"},
			config: map[string]any{"LogGroupName": "g"}, current: map[string]any{"LogGroupName": "g", "RetentionInDays": float64(14)}, want: resource.Mutable},
		"removed and already gone": {typeName: "AWS::Logs::LogGroup", applied: []string{"LogGroupName", "RetentionInDays"},
			config: map[string]any{"LogGroupName": "g"}, current: map[string]any{"LogGroupName": "g"}, want: resource.Same},
		"never applied": {typeName: "AWS::Logs::LogGroup", applied: nil,
			config: map[string]any{"LogGroupName": "g"}, current: map[string]any{"LogGroupName": "g", "RetentionInDays": float64(14)}, want: resource.Same},
		"still declared": {typeName: "AWS::Logs::LogGroup", applied: []string{"RetentionInDays"},
			config: map[string]any{"RetentionInDays": 14}, current: map[string]any{"RetentionInDays": float64(14)}, want: resource.Same},
		"create-only": {typeName: "AWS::Logs::LogGroup", applied: []string{"LogGroupClass"},
			config: map[string]any{}, current: map[string]any{"LogGroupClass": "STANDARD"}, want: resource.Same},
		"the identity tag property": {typeName: "AWS::Logs::LogGroup", applied: []string{"Tags"}, tagged: true,
			config: map[string]any{}, current: map[string]any{"Tags": identityTags("g")}, want: resource.Same},
	} {
		t.Run(name, func(t *testing.T) {
			r := realTypeFixture(t, c.typeName)
			if c.tagged {
				r.tags = &tagPlacement{property: "Tags", shape: cfschema.TagShapeArray}
			}
			spec := specWith(c.config)
			spec.Applied = c.applied
			got, err := r.compareDeclared(spec, stateWith(c.current))
			if err != nil || got != c.want {
				t.Fatalf("compare = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// A type with no update handler cannot reset a property without a replace,
// so a removal leaves it alone.
func TestDiffLeavesARemovalAnUpdateCannotMake(t *testing.T) {
	r := realTypeFixture(t, realTypeLambdaPermission)
	if r.schema.HasUpdate {
		t.Skip("the permission's schema gained an update handler")
	}
	spec := specWith(map[string]any{})
	spec.Applied = []string{"SourceAccount"}
	if got, err := r.compareDeclared(spec, stateWith(map[string]any{"SourceAccount": "123456789012"})); err != nil || got != resource.Same {
		t.Fatalf("compare = %v, %v; want Same", got, err)
	}
}

// The update removes what was removed, and both create and update record
// what they set.
func TestUpdateRemovesAndRecords(t *testing.T) {
	fc := &fakeClient{byIdentifier: map[string]map[string]any{"env-api": {
		"RoleName": "env-api", "MaxSessionDuration": float64(7200), "Tags": identityTags("env-api"),
	}}}
	rt := roleOver(fc)
	spec := resource.Spec{Name: "env-api", Config: map[string]any{"Description": "d"}, Applied: []string{"Description", "MaxSessionDuration"}}
	state, err := rt.Update(context.Background(), resource.Ref{Name: "env-api"}, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.updatePatches) != 1 || !strings.Contains(string(fc.updatePatches[0]), `{"op":"remove","path":"/MaxSessionDuration"}`) {
		t.Fatalf("patches = %s, want MaxSessionDuration removed", fc.updatePatches)
	}
	if !reflect.DeepEqual(state.Applied, []string{"Description"}) {
		t.Fatalf("Applied = %v, want what the update declared", state.Applied)
	}

	created, err := roleOver(&fakeClient{createID: "env-api", createProps: map[string]any{"RoleName": "env-api"},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/RoleName"}}}).
		Create(context.Background(), resource.Spec{Name: "env-api", Config: map[string]any{"Description": "d", "Path": "/"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(created.Applied, []string{"Description", "Path"}) {
		t.Fatalf("Applied = %v, want what the create declared, not the derived name or tag kraai added", created.Applied)
	}
}

// A patch removing a property goes to Cloud Control, even for a type the
// direct calls mutate: they only set.
func TestARemovalIsCloudControls(t *testing.T) {
	var directCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directCalls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	done := &cctypes.ProgressEvent{OperationStatus: cctypes.OperationStatusSuccess, Identifier: aws.String("q"), ResourceModel: aws.String(`{"QueueUrl":"q"}`)}
	cc := &fakeCC{
		updateOut: &cloudcontrol.UpdateResourceOutput{ProgressEvent: &cctypes.ProgressEvent{RequestToken: aws.String("tok")}},
		statusOut: []*cloudcontrol.GetResourceRequestStatusOutput{{ProgressEvent: done}},
	}
	c := &Client{cc: cc, direct: &direct.Client{HTTP: srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region:      "us-east-1", Endpoint: func(string) string { return srv.URL }},
		canMutate: func(string, map[string]any) bool { return true }}
	testPollTimings()(c)
	patch := `[{"op":"remove","path":"/DelaySeconds"}]`
	if _, err := c.UpdateResource(context.Background(), TypeSQSQueue, "https://sqs.us-east-1.amazonaws.com/1/q", []byte(patch)); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	if len(cc.updateReq) != 1 || directCalls.Load() != 0 {
		t.Fatalf("Cloud Control updates %d, direct calls %d; want the removal sent to Cloud Control alone", len(cc.updateReq), directCalls.Load())
	}
}
