package aws

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

func TestQueueCreateSetsOnlyItsName(t *testing.T) {
	fc := &fakeClient{createID: "https://sqs.us-east-1.amazonaws.com/123456789012/env-svc-jobs", createProps: map[string]any{
		"QueueName": "env-svc-jobs",
		"QueueUrl":  "https://sqs.us-east-1.amazonaws.com/123456789012/env-svc-jobs",
		"Arn":       "arn:aws:sqs:us-east-1:123456789012:env-svc-jobs",
	}}
	queue := newQueueResource(fc)

	state, err := queue.Create(context.Background(), resource.Spec{Binding: "JOBS", Name: "env-svc-jobs"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := fc.createCalls[0]
	if len(desired) != 1 || desired["QueueName"] != "env-svc-jobs" {
		t.Fatalf("desired state = %v, want exactly {QueueName: env-svc-jobs}", desired)
	}
	if state.Attributes["QueueUrl"] == nil || state.Attributes["Arn"] == nil {
		t.Fatalf("published attributes = %v, want QueueUrl and Arn for the function and role to read", state.Attributes)
	}
}

// A queue's Cloud Control identifier is its URL, which only SQS can assign,
// so the derived name is matched against QueueName across the listed
// queues rather than used as the identifier directly.
func TestQueueGetMatchesOnQueueName(t *testing.T) {
	fc := &fakeClient{
		list: []string{"https://sqs/other", "https://sqs/env-svc-jobs"},
		byIdentifier: map[string]map[string]any{
			"https://sqs/other":        {"QueueName": "env-svc-other"},
			"https://sqs/env-svc-jobs": {"QueueName": "env-svc-jobs", "Arn": "arn:jobs"},
		},
	}
	queue := newQueueResource(fc)

	state, err := queue.Get(context.Background(), resource.Ref{Name: "env-svc-jobs"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil || state.ID != "https://sqs/env-svc-jobs" {
		t.Fatalf("Get = %+v, want the queue whose QueueName matches", state)
	}

	absent, err := queue.Get(context.Background(), resource.Ref{Name: "env-svc-missing"})
	if err != nil || absent != nil {
		t.Fatalf("Get(missing) = %v, %v; want nil, nil", absent, err)
	}
}

func TestQueueDiffIsSameWhenTheNameMatches(t *testing.T) {
	fc := &fakeClient{schema: Schema{
		PrimaryIdentifier:    []string{"/properties/QueueUrl"},
		CreateOnlyProperties: []string{"/properties/QueueName"},
	}}
	queue := newQueueResource(fc)

	spec := resource.Spec{Name: "env-svc-jobs"}
	same, err := queue.Diff(spec, &resource.State{Attributes: map[string]any{"QueueName": "env-svc-jobs"}})
	if err != nil || same != resource.Same {
		t.Fatalf("Diff(same name) = %v, %v; want Same", same, err)
	}
	renamed, err := queue.Diff(spec, &resource.State{Attributes: map[string]any{"QueueName": "env-svc-old"}})
	if err != nil || renamed != resource.Immutable {
		t.Fatalf("Diff(other name) = %v, %v; want Immutable: a queue cannot be renamed in place", renamed, err)
	}
}
