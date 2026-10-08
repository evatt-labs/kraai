package aws

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
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

// newQueueResource provisions one standard SQS queue per `queues:` binding:
// the native AWS::SQS::Queue, its properties mapped from the capability's
// config. Found as every native queue is, by kraai's identity tag, since
// Cloud Control identifies a queue by the URL the service assigns. What a
// queue publishes is that URL and its ARN, which the service's function
// receives as environment variables and its execution role names in a grant
// (iamrole.go).
//
// An earlier kraai found queues by QueueName and did not tag them; a run
// allowed to adopt takes an untagged queue of the derived name and tags it.
func newQueueResource(client ccAPI) *nativeResource {
	facts, err := cfschema.Lookup(TypeSQSQueue)
	if err != nil {
		// The index is compiled in; a type missing from it is a broken
		// build, which every test of this type would show.
		panic(err)
	}
	n := newNativeResourceWith(client, nil, facts, resource.LookupByTag)
	n.fromCapability = queueProperties
	n.adoptMatch = queueMatch
	if located, ok := client.(accountRegion); ok {
		n.identifierFor = func(ctx context.Context, name string) (string, error) {
			account, err := located.AccountID(ctx)
			if err != nil {
				return "", err
			}
			return queueURL(located.Region(), account, name), nil
		}
	}
	// The queue was found by QueueName, untagged, until the second
	// generation.
	n.taggedSince = 2
	return n
}

func queueMatch(properties map[string]any, name string) bool {
	return properties["QueueName"] == name
}

// queueProperties is the queue's properties: its name and nothing else.
// Every other property keeps SQS's default, so a manifest cannot yet ask
// for a FIFO queue, a redrive policy or a visibility timeout.
func queueProperties(spec resource.Spec) (map[string]any, error) {
	return map[string]any{"QueueName": spec.Name}, nil
}

// accountRegion is a client that knows the account and region it works
// in, which a queue's URL is built from.
type accountRegion interface {
	AccountID(ctx context.Context) (string, error)
	Region() string
}

// queueURL builds the URL SQS gives a queue of name, Cloud Control's
// identifier for it. SQS's list can omit a queue for minutes after it is
// created, where a read by URL finds it at once.
func queueURL(region, account, name string) string {
	return "https://sqs." + region + ".amazonaws.com/" + account + "/" + name
}

// queueARN builds a queue's ARN from its region, account and name, so a
// grant can name a queue that has not been created yet.
func queueARN(region, account, name string) string {
	return "arn:aws:sqs:" + region + ":" + account + ":" + name
}
