package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeSQSQueue is AWS::SQS::Queue's Cloud Control TypeName.
const TypeSQSQueue = "AWS::SQS::Queue"

// newQueueResource provisions one standard SQS queue per `queues:` binding.
//
// Found by QueueName rather than by the derived name directly: Cloud
// Control identifies a queue by its URL, which the service assigns, so the
// engine lists the region's queues and matches on the name SQS guarantees
// unique within an account and region. What a queue publishes is that URL
// and its ARN, which the service's function receives as environment
// variables and its execution role names in a grant (iamrole.go).
func newQueueResource(client ccAPI) *resourceType {
	return &resourceType{
		provider: Provider, typeName: TypeSQSQueue, lookup: resource.LookupByAttr, client: client,
		match:     queueMatch,
		translate: queueTranslate,
	}
}

func queueMatch(properties map[string]any, name string) bool {
	return properties["QueueName"] == name
}

// queueTranslate builds the queue's desired state: its name and nothing
// else. Every other property keeps SQS's default, so a manifest cannot
// yet ask for a FIFO queue, a redrive policy or a visibility timeout.
func queueTranslate(_ context.Context, spec resource.Spec) (resource.Spec, error) {
	translated := spec
	translated.Config = map[string]any{"QueueName": spec.Name}
	return translated, nil
}

// queueARN builds a queue's ARN from its region, account and name, so a
// grant can name a queue that has not been created yet.
func queueARN(region, account, name string) string {
	return "arn:aws:sqs:" + region + ":" + account + ":" + name
}
