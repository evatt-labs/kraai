package aws

import (
	"context"
	"reflect"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// A queue is created with its derived name and kraai's identity tag, and
// nothing else.
func TestQueueCreateSetsOnlyItsNameAndTag(t *testing.T) {
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
	want := map[string]any{"QueueName": "env-svc-jobs", "Tags": identityTags("env-svc-jobs")}
	if !reflect.DeepEqual(desired, want) {
		t.Fatalf("desired state = %v, want %v", desired, want)
	}
	if state.Attributes["QueueUrl"] == nil || state.Attributes["Arn"] == nil {
		t.Fatalf("published attributes = %v, want QueueUrl and Arn for the function and role to read", state.Attributes)
	}
}

// A queue's Cloud Control identifier is its URL, which only SQS can assign,
// so the queue is found by kraai's identity tag across the listed queues.
// An untagged queue of the derived name, which an earlier kraai found by
// QueueName, is taken only by a run allowed to adopt, and marked to be
// tagged; one tagged for another name never is.
func TestQueueGetFindsItsTagOrAdoptsByName(t *testing.T) {
	queues := func() *fakeClient {
		return &fakeClient{
			list: []string{"https://sqs/other", "https://sqs/foreign", "https://sqs/untagged", "https://sqs/env-svc-jobs"},
			byIdentifier: map[string]map[string]any{
				"https://sqs/other":        {"QueueName": "env-svc-other", "Tags": identityTags("env-svc-other")},
				"https://sqs/foreign":      {"QueueName": "env-svc-foreign", "Tags": identityTags("someone-else")},
				"https://sqs/untagged":     {"QueueName": "env-svc-old"},
				"https://sqs/env-svc-jobs": {"QueueName": "env-svc-jobs", "Arn": "arn:jobs", "Tags": identityTags("env-svc-jobs")},
			},
			schema: cfschema.Facts{HasUpdate: true},
		}
	}
	ctx := context.Background()
	adopt := resource.WithTagVersion(ctx, 0)
	for name, c := range map[string]struct {
		ctx       context.Context
		ref       string
		wantID    string
		wantAdopt bool
	}{
		"tagged":                        {ctx, "env-svc-jobs", "https://sqs/env-svc-jobs", false},
		"tagged, run may adopt":         {adopt, "env-svc-jobs", "https://sqs/env-svc-jobs", false},
		"untagged":                      {ctx, "env-svc-old", "", false},
		"untagged, run may adopt":       {adopt, "env-svc-old", "https://sqs/untagged", true},
		"tagged for another, may adopt": {adopt, "env-svc-foreign", "", false},
		"missing":                       {adopt, "env-svc-missing", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			state, err := newQueueResource(queues()).Get(c.ctx, resource.Ref{Name: c.ref})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if c.wantID == "" {
				if state != nil {
					t.Fatalf("Get = %+v, want absent", state)
				}
				return
			}
			if state == nil || state.ID != c.wantID || state.Adopt != c.wantAdopt {
				t.Fatalf("Get = %+v, want %s with adopt %v", state, c.wantID, c.wantAdopt)
			}
		})
	}
}

func TestQueueDiffIsSameWhenTheNameMatches(t *testing.T) {
	fc := &fakeClient{schema: cfschema.Facts{
		PrimaryIdentifier: []string{"/properties/QueueUrl"},
		CreateOnly:        []string{"/properties/QueueName"},
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

// locatedClient is a fakeClient that knows its account and region.
type locatedClient struct{ *fakeClient }

func (locatedClient) AccountID(context.Context) (string, error) { return "123456789012", nil }
func (locatedClient) Region() string                             { return "us-east-1" }

// A queue created moments ago, which SQS's list does not show yet, is
// still found: its URL follows from its name, and a read by URL sees it.
func TestQueueIsFoundByItsURLWhileTheListLags(t *testing.T) {
	url := "https://sqs.us-east-1.amazonaws.com/123456789012/env-svc-jobs"
	fc := &fakeClient{
		list:         nil,
		byIdentifier: map[string]map[string]any{url: taggedProps("env-svc-jobs", map[string]any{"QueueName": "env-svc-jobs"})},
		schema:       cfschema.Facts{HasUpdate: true},
	}
	state, err := newQueueResource(locatedClient{fc}).Get(context.Background(), resource.Ref{Name: "env-svc-jobs"})
	if err != nil || state == nil || state.ID != url {
		t.Fatalf("Get = %+v, %v; want the queue at %s", state, err, url)
	}
}
