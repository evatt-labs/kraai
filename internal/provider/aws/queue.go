package aws

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeSQSQueue is AWS::SQS::Queue's Cloud Control TypeName.
const TypeSQSQueue = "AWS::SQS::Queue"

// sqsQueueNameLimit is SQS's own queue name length limit, which the
// CloudFormation schema's bare `{"type": "string"}` for QueueName does not
// declare: fifoQueueSuffix must fit inside it along with the rest of the
// name.
const sqsQueueNameLimit = 80

// fifoQueueSuffix is required at the end of every FIFO queue's name; SQS
// rejects a FifoQueue: true create whose name does not end in it.
const fifoQueueSuffix = ".fifo"

// deriveFifoQueueName is a native AWS::SQS::Queue's per-type naming rule.
// kraai's derived name never ends in .fifo, so a FIFO queue left to it would
// be rejected at create; this fills QueueName from the derived name plus
// the suffix, truncating to leave the suffix room within sqsQueueNameLimit.
//
// Only when the entry leaves QueueName unset: an explicit QueueName that
// does not end in .fifo is refused rather than silently rewritten, since the
// entry asked for that exact name.
func deriveFifoQueueName(spec resource.Spec, properties map[string]any) error {
	fifo, _ := properties["FifoQueue"].(bool)
	if !fifo {
		return nil
	}
	if existing, set := properties["QueueName"]; set {
		name, ok := existing.(string)
		if !ok || !strings.HasSuffix(name, fifoQueueSuffix) {
			return kerrors.Validation(
				"%s: FifoQueue is true, so QueueName must end in %q", TypeSQSQueue, fifoQueueSuffix)
		}
		return nil
	}
	base := spec.Name
	// A planner-derived name is already at most 63 bytes (internal/naming's
	// own S3/R2 bound), so this branch does not fire from a manifest today;
	// kept because nothing here guarantees that bound stays below this
	// type's own limit forever.
	if len(base)+len(fifoQueueSuffix) > sqsQueueNameLimit {
		base = base[:sqsQueueNameLimit-len(fifoQueueSuffix)]
	}
	properties["QueueName"] = base + fifoQueueSuffix
	return nil
}

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
