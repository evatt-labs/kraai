package aws

import (
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// AWS writes "${...}" itself: IAM policy variables and Cognito identity
// variables must pass through untouched with no escaping, which is why a
// colon or a slash can never be part of a reference. API Gateway's
// ${stageVariables.foo} has a reference's shape; it is read as one and the
// planner refuses it by name, naming the escape.
func TestReferenceGrammar(t *testing.T) {
	literal := []string{
		"arn:aws:s3:::bucket/${aws:username}/*",
		"${aws:PrincipalTag/team}",
		"${cognito-identity.amazonaws.com:sub}",
		"$${DLQ.Arn}",
		"${DLQ}",
		"${DLQ.}",
		"${.Arn}",
		"${1DLQ.Arn}",
		"no reference at all",
	}
	for _, s := range literal {
		names, err := nativeReferences(map[string]any{nativePropertiesKey: map[string]any{"P": s}})
		if err != nil || len(names) != 0 {
			t.Errorf("%q read as references %v, %v", s, names, err)
		}
	}

	names, err := nativeReferences(map[string]any{nativePropertiesKey: map[string]any{
		"A": "${DLQ.Arn}",
		"B": []any{map[string]any{"Value": "x-${JOBS.QueueName}-${DLQ.Arn}"}},
		"C": "${stageVariables.foo}",
		"D": 3,
	}})
	if err != nil || !reflect.DeepEqual(names, []string{"DLQ", "JOBS", "stageVariables"}) {
		t.Fatalf("nativeReferences = %v, %v", names, err)
	}
}

func referencingSpec(attrs map[string]map[string]any) resource.Spec {
	return resource.Spec{
		Binding:    "ALARM",
		References: map[string]string{"DLQ": "aws/AWS::SQS::Queue::Native", "JOBS": "aws/AWS::SQS::Queue"},
		Attributes: attrs,
	}
}

func TestResolveReferences(t *testing.T) {
	published := map[string]map[string]any{
		"DLQ.aws/AWS::SQS::Queue::Native": {"Arn": "arn:dlq", "QueueName": "dlq", "RedriveAllowPolicy": map[string]any{"x": 1},
			"Delay": 5, "Tags": []any{"a"}},
	}
	properties := map[string]any{
		"Whole":   "${DLQ.Arn}",
		"List":    "${DLQ.Tags}",
		"Number":  "${DLQ.Delay}",
		"Mixed":   "queue ${DLQ.QueueName} waits ${DLQ.Delay}s",
		"Nested":  map[string]any{"Deep": []any{"${DLQ.RedriveAllowPolicy.x}"}},
		"Escaped": "$${DLQ.Arn} and ${aws:username}",
	}

	res, err := resolveReferences(referencingSpec(published), properties, true)
	if err != nil {
		t.Fatalf("resolveReferences: %v", err)
	}
	want := map[string]any{
		"Whole":   "arn:dlq",
		"List":    []any{"a"},
		"Number":  5,
		"Mixed":   "queue dlq waits 5s",
		"Nested":  map[string]any{"Deep": []any{1}},
		"Escaped": "${DLQ.Arn} and ${aws:username}",
	}
	if !reflect.DeepEqual(res.properties, want) {
		t.Fatalf("resolved = %#v\nwant %#v", res.properties, want)
	}
	if properties["Whole"] != "${DLQ.Arn}" {
		t.Fatal("resolution wrote the entry's own properties")
	}

	for name, c := range map[string]struct {
		properties map[string]any
		strict     bool
		wantErr    string
	}{
		"unpublished, at apply":       {properties: map[string]any{"A": "${JOBS.Arn}"}, strict: true, wantErr: "published nothing"},
		"published, attribute absent": {properties: map[string]any{"A": "${DLQ.Nope}"}, wantErr: "DLQ published no Nope"},
		"not resolved by the plan":    {properties: map[string]any{"A": "${OTHER.Arn}"}, wantErr: "the plan did not resolve"},
		"a map inside a string":       {properties: map[string]any{"A": "x ${DLQ.RedriveAllowPolicy}"}, wantErr: "cannot be written inside a string"},
	} {
		if _, err := resolveReferences(referencingSpec(published), c.properties, c.strict); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: error = %v, want %q", name, err, c.wantErr)
		}
	}

	// At plan, a producer that has not published leaves its values as
	// written and says where they are.
	res, err = resolveReferences(referencingSpec(published), map[string]any{
		"Known":   "${DLQ.Arn}",
		"Pending": []any{map[string]any{"Value": "${JOBS.QueueName}"}},
	}, false)
	if err != nil {
		t.Fatalf("lenient: %v", err)
	}
	if !reflect.DeepEqual(res.unknown, [][]string{{"Pending", "0", "Value"}}) || res.properties["Known"] != "arn:dlq" {
		t.Fatalf("lenient resolution = %+v", res)
	}
	if got := res.unknownProperties(); !reflect.DeepEqual(got, []string{"Pending"}) {
		t.Fatalf("unknownProperties = %v", got)
	}
}

func TestAWSVendorType(t *testing.T) {
	for key, want := range map[string]string{
		"aws/AWS::SQS::Queue":                 "AWS::SQS::Queue",
		"aws/AWS::SQS::Queue::Native":         "AWS::SQS::Queue",
		"aws/AWS::S3::Bucket::ArtifactBucket": "AWS::S3::Bucket",
		"neon/branch":                         "",
		"aws/branch":                          "",
	} {
		got, ok := awsVendorType(key)
		if got != want || ok != (want != "") {
			t.Errorf("awsVendorType(%q) = %q, %v", key, got, ok)
		}
	}
}

// At plan, a value referencing a resource not created yet is not judged
// against the schema: SQS's DelaySeconds is an integer, and its placeholder
// is a string. The property it names on the referenced type still is.
func TestNativeValidateSpecWithReferences(t *testing.T) {
	queue := newFixtureNative(t, TypeSQSQueue, &fakeClient{})
	pending := resource.Spec{
		Binding: "Q", Name: "q",
		Config: map[string]any{nativePropertiesKey: map[string]any{
			"DelaySeconds": "${JOBS.DelaySeconds}",
			"Tags":         []any{map[string]any{"Key": "source", "Value": "${JOBS.QueueName}"}},
		}},
		References: map[string]string{"JOBS": "aws/AWS::SQS::Queue"},
	}
	if err := queue.ValidateSpec(pending); err != nil {
		t.Fatalf("a pending reference was judged: %v", err)
	}

	misspelled := pending
	misspelled.Config = map[string]any{nativePropertiesKey: map[string]any{"DelaySeconds": "${JOBS.DelaySecond}"}}
	if err := queue.ValidateSpec(misspelled); err == nil || !strings.Contains(err.Error(), "has no property DelaySecond") {
		t.Fatalf("a misspelled referenced property: %v", err)
	}

	// Once known, the resolved value is judged like any other.
	resolved := pending
	resolved.Attributes = map[string]map[string]any{"JOBS.aws/AWS::SQS::Queue": {"DelaySeconds": "five", "QueueName": "jobs"}}
	if err := queue.ValidateSpec(resolved); err == nil || !strings.Contains(err.Error(), "DelaySeconds: got string") {
		t.Fatalf("a resolved value of the wrong type: %v", err)
	}

	// An IAM policy variable needs no escape.
	role := newFixtureNative(t, "AWS::IAM::Role", &fakeClient{})
	policy := map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Effect": "Allow", "Principal": map[string]any{"AWS": "*"}, "Action": "sts:AssumeRole",
		"Condition": map[string]any{"StringEquals": map[string]any{"aws:username": "${aws:username}"}},
	}}}
	if err := role.ValidateSpec(nativeSpec("r", map[string]any{"AssumeRolePolicyDocument": policy})); err != nil {
		t.Fatalf("an IAM policy variable: %v", err)
	}
}

// Diff compares what a reference resolves to. One naming a resource being
// created or replaced has no value yet, and must not read as unchanged.
func TestNativeDiffWithReferences(t *testing.T) {
	cc := &fakeClient{}
	queue := newFixtureNative(t, TypeSQSQueue, cc)
	cc.schema.HasUpdate = true
	live := &resource.State{Attributes: map[string]any{"RedrivePolicy": map[string]any{"deadLetterTargetArn": "arn:dlq"}, "FifoQueue": false}}

	spec := func(properties map[string]any, attrs map[string]map[string]any) resource.Spec {
		return resource.Spec{
			Binding: "Q", Name: "q",
			Config:     map[string]any{nativePropertiesKey: properties},
			References: map[string]string{"DLQ": "aws/AWS::SQS::Queue::Native"},
			Attributes: attrs,
		}
	}
	redrive := map[string]any{"RedrivePolicy": map[string]any{"deadLetterTargetArn": "${DLQ.Arn}"}}
	known := func(arn string) map[string]map[string]any {
		return map[string]map[string]any{"DLQ.aws/AWS::SQS::Queue::Native": {"Arn": arn}}
	}

	for name, c := range map[string]struct {
		spec resource.Spec
		want resource.Difference
	}{
		"resolves to the live value":        {spec(redrive, known("arn:dlq")), resource.Same},
		"resolves to a different value":     {spec(redrive, known("arn:new")), resource.Mutable},
		"names a producer not yet known":    {spec(redrive, nil), resource.Mutable},
		"not known, absent from live state": {spec(map[string]any{"ReceiveMessageWaitTimeSeconds": "${DLQ.Wait}"}, nil), resource.Mutable},
		"create-only property, not known":   {spec(map[string]any{"FifoQueue": "${DLQ.FifoQueue}"}, nil), resource.Immutable},
		"create-only property, known equal": {spec(map[string]any{"FifoQueue": "${DLQ.FifoQueue}"}, map[string]map[string]any{"DLQ.aws/AWS::SQS::Queue::Native": {"FifoQueue": false}}), resource.Same},
	} {
		got, err := queue.Diff(c.spec, live)
		if err != nil || got != c.want {
			t.Errorf("%s: Diff = %v, %v; want %v", name, got, err, c.want)
		}
	}
}

// Create resolves every reference against what apply handed it.
func TestNativeCreateResolvesReferences(t *testing.T) {
	cc := &fakeClient{createID: "https://sqs/q"}
	queue := newFixtureNative(t, TypeSQSQueue, cc)
	spec := resource.Spec{
		Binding: "Q", Name: "q",
		Config: map[string]any{nativePropertiesKey: map[string]any{
			"RedrivePolicy": map[string]any{"deadLetterTargetArn": "${DLQ.Arn}", "maxReceiveCount": 5},
		}},
		References: map[string]string{"DLQ": "aws/AWS::SQS::Queue::Native"},
		Attributes: map[string]map[string]any{"DLQ.aws/AWS::SQS::Queue::Native": {"Arn": "arn:dlq"}},
	}
	if _, err := queue.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := cc.createCalls[0]["RedrivePolicy"]; !reflect.DeepEqual(got, map[string]any{"deadLetterTargetArn": "arn:dlq", "maxReceiveCount": 5}) {
		t.Fatalf("RedrivePolicy = %v", got)
	}

	spec.Attributes = nil
	if _, err := queue.Create(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "published nothing") {
		t.Fatalf("Create with an unpublished reference: %v", err)
	}
}
