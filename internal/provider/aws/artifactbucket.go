package aws

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeArtifactBucket is this registry's key for the per-service Lambda
// artifact bucket. Not "AWS::S3::Bucket" itself: the registry keys on
// provider and type, and that key is already the objects capability's. The
// registration declares VendorType so a plan can still say what it drives.
var TypeArtifactBucket = resource.RoleType(TypeS3Bucket, "ArtifactBucket")

// maxBucketNameLen is S3's own bucket name length ceiling.
const maxBucketNameLen = 63

// artifactBucketSuffix keeps the bucket's real S3 name distinct from the
// derived service name the function, role and URL all reuse. Those live in
// separate namespaces; bucket names are global.
const artifactBucketSuffix = "-artifacts"

// artifactBucketName derives the real S3 bucket name from a service's
// derived name, which is already lowercase and limited to [a-z0-9-],
// truncated to S3's ceiling.
func artifactBucketName(serviceName string) string {
	name := serviceName + artifactBucketSuffix
	if len(name) <= maxBucketNameLen {
		return name
	}
	return strings.TrimRight(name[:maxBucketNameLen], "-")
}

// artifactBucketResource provisions the per-service S3 bucket that holds a
// Lambda's deployment artifacts.
//
// Per service rather than per environment: the planner expands compute once
// per service with no environment-level expansion point, and a bucket
// shared by N services would have N concurrent Gets each see "absent" on a
// fresh environment and N concurrent creates race at one name. One bucket
// per service means one creator per bucket.
type artifactBucketResource struct {
	inner *resourceType
	// client is inner.client's concrete type, kept because ccAPI has no S3
	// data-plane methods and Delete needs EmptyBucket.
	client *Client
}

// bucketOwnedBy adapts Client.OwnsBucket to the engine's ownsFunc, for the
// objects capability's bucket: one this account does not own reads as
// absent, and the create S3 then refuses names the collision.
func bucketOwnedBy(client *Client) ownsFunc {
	return func(ctx context.Context, identifier string, _ map[string]any) (bool, error) {
		owned, err := client.OwnsBucket(ctx, identifier)
		if err != nil {
			return false, err
		}
		if !owned {
			recordForeignBucket(ctx, identifier,
				"bucket exists but is not owned by this account; reporting it as absent")
		}
		return owned, nil
	}
}

func newArtifactBucketResource(client *Client) *artifactBucketResource {
	return &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: client},
		client: client,
	}
}

// bucketRef rewrites a compute Ref's derived service name into the real S3
// bucket name before delegating to the byName engine.
func bucketRef(ref resource.Ref) resource.Ref {
	return resource.Ref{Provider: ref.Provider, Type: ref.Type, Name: artifactBucketName(ref.Name)}
}

// Get reports the real bucket's state, but only once ownership is
// confirmed: bucket names are global, Cloud Control resolves one any
// account owns, and generic environment names ("dev-api-artifacts") were
// already taken by strangers on the first live try. A foreign bucket reads
// as absent, so plan proposes a create and S3 refuses it by name, rather
// than kraai uploading into or deleting from a bucket that is not ours.
func (a *artifactBucketResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	realRef := bucketRef(ref)

	state, err := a.inner.Get(ctx, realRef)
	if err != nil || state == nil {
		return state, err
	}

	owned, err := a.client.OwnsBucket(ctx, realRef.Name)
	if err != nil {
		return nil, err
	}
	if !owned {
		recordForeignBucket(ctx, realRef.Name,
			"artifact bucket exists but is not owned by this account; reporting it as absent so plan proposes creating it")
		return nil, nil
	}

	// Reported under the caller's own Ref, the service-derived name,
	// which is how everything above this type identifies it.
	state.Ref = ref
	return state, nil
}

// recordForeignBucket leaves a span event explaining why a bucket was
// treated as not ours, so an operator staring at an unexpected create or a
// skipped delete can learn why. A span event rather than a log line: this
// package has no logger, and every call here already runs inside the
// telemetry decorator's span.
func recordForeignBucket(ctx context.Context, bucketName, message string) {
	trace.SpanFromContext(ctx).AddEvent(message, trace.WithAttributes(
		attribute.String("kraai.bucket_name", bucketName),
	))
}

func (a *artifactBucketResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	realName := artifactBucketName(spec.Name)

	realSpec := spec
	realSpec.Name = realName
	// The incoming Config is the generic compute shape (dir, settings,
	// trigger), none of which is BucketName. Submitting it unchanged would
	// have S3 generate a name Get can never find again.
	realSpec.Config = map[string]any{"BucketName": realName}

	state, err := a.inner.Create(ctx, realSpec)
	if err != nil || state == nil {
		return state, err
	}
	state.Ref.Name = spec.Name
	return state, nil
}

// Update is refused: this bucket has no configuration to change in place.
// Its only job is to exist and hold the objects lambda.go uploads.
func (a *artifactBucketResource) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	return nil, kerrors.Wrap(resource.ErrImmutable, kerrors.CodeValidation,
		"an artifact bucket has no in-place configuration; replace it instead")
}

// Delete empties the real bucket before deleting it, since S3 refuses to
// delete a non-empty bucket and this one always holds at least one artifact
// by teardown time. Ownership is checked here as well as in Get, because
// emptying is the destructive half and nothing guarantees Get ran first; a
// bucket that is not ours is treated as already gone. EmptyBucket and the
// engine's Delete both tolerate a bucket that no longer exists, so a retry
// after a partial teardown takes the same path.
func (a *artifactBucketResource) Delete(ctx context.Context, ref resource.Ref) error {
	realRef := bucketRef(ref)

	owned, err := a.client.OwnsBucket(ctx, realRef.Name)
	if err != nil {
		return err
	}
	if !owned {
		recordForeignBucket(ctx, realRef.Name,
			"artifact bucket is not owned by this account; skipping emptying and deletion")
		return nil
	}

	if err := a.client.EmptyBucket(ctx, realRef.Name); err != nil {
		return err
	}
	return a.inner.Delete(ctx, realRef)
}
