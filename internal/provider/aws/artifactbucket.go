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
// artifact bucket — deliberately not "AWS::S3::Bucket" itself.
//
// Registry.Register keys uniqueness on Provider+Type alone
// (internal/resource/registry.go), with no notion of "the same AWS type
// registered twice for two different capabilities." "AWS::S3::Bucket" is
// already registered under CapabilityObjects for a manifest's own
// `objects:` bindings (see TypeS3Bucket above); a manifest that configures
// both objects and compute on aws — kraai's own kraai-api is exactly this
// shape — would fail Register outright on a duplicate key if this
// registration reused that same string.
//
// The registration declares VendorType: TypeS3Bucket, which is what says
// out loud that this key drives an ordinary S3 bucket. The registry checks
// the two agree, and a plan names both.
var TypeArtifactBucket = resource.RoleType(TypeS3Bucket, "ArtifactBucket")

// maxBucketNameLen is S3's own bucket name length ceiling.
const maxBucketNameLen = 63

// artifactBucketSuffix disambiguates the artifact bucket's real S3 name
// from the derived service name every other Tier 2 registration for the
// same service reuses verbatim (the Lambda function, its execution role,
// its Url) — appending it is what keeps this bucket from colliding with
// those other resources' own AWS-side names, which is otherwise
// unenforced: nothing stops an AWS::IAM::Role and an AWS::Lambda::Function
// sharing a name (they are different namespaces), but two S3 buckets never
// can, globally, across every AWS account on Earth.
const artifactBucketSuffix = "-artifacts"

// artifactBucketName derives the real S3 bucket name from a service's
// kraai-derived name (naming.ServiceName's output, already lowercase and
// limited to [a-z0-9-] — see that function's own doc comment), truncating
// to S3's 63-character ceiling the same way internal/naming already
// truncates for the identical DNS-compliance reason (R2/S3 both cap bucket
// names at 63).
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
// # Per-service, not per-environment — a deviation from the brief, flagged here
//
// The brief calls for one artifact bucket shared by every service in an
// environment. Structurally, that shape is not reachable from this
// workstream alone: internal/plan's expandCompute (out of scope for this
// workstream) expands every CapabilityCompute registration once per
// service, deriving each Ref from that service's own name, with no
// environment-only expansion point this package's registration can hook
// into. But even setting that aside, a genuinely shared bucket is actively
// wrong under this design's own concurrency model, not merely
// unreachable: `kraai plan` calls Get for every planned item in a wave
// concurrently, before anything is created. If every service's
// artifact-bucket item resolved to the identical AWS-side bucket name, a
// fresh environment's first `kraai plan` would have every service's Get
// independently observe "does not exist yet" and plan ActionCreate — none
// of them would see a sibling's not-yet-applied plan. `kraai apply` then
// runs every ActionCreate in the wave concurrently too, which means N
// services racing N concurrent CreateResource calls at the same bucket
// name, a create pattern this contract's Resource.Create was never built
// to tolerate (it is called once per Ref, not N times concurrently for
// equivalent Refs). One bucket per service sidesteps this entirely: each
// service's Ref is genuinely distinct, so there is only ever one creator
// per bucket. Depended upon by TypeLambdaFunction's own registration
// (register.go) — the function that reads it still waits for it, one wave
// later, and it is still destroyed whenever that service's own resources
// are torn down, but multiplied by service count rather than shared. A
// future workstream wanting a genuinely shared bucket needs a real
// environment-scoped expansion phase in internal/plan plus a way to
// serialize (or dedupe) concurrent creators of the same resource — neither
// exists today, and inventing either here is out of this workstream's
// scope; noted in this workstream's PR description rather than worked
// around silently.
type artifactBucketResource struct {
	inner *resourceType
	// client is the same *Client inner.client already holds, kept as its
	// own field because inner.client is typed ccAPI — the generic Cloud
	// Control surface (resource.go) — which has no S3 object-data-plane
	// methods on it at all. Delete needs EmptyBucket, which lives on the
	// concrete *Client beside PutObject (client.go), so this field exists
	// purely to reach it; every other method here still goes through inner.
	client *Client
}

func newArtifactBucketResource(client *Client) *artifactBucketResource {
	return &artifactBucketResource{
		inner:  &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: client},
		client: client,
	}
}

// bucketRef/bucketSpec rewrite a compute Ref/Spec's derived service name
// into this type's own real S3 bucket name before delegating to the
// generic byName engine — see artifactBucketName's own doc comment.

func bucketRef(ref resource.Ref) resource.Ref {
	return resource.Ref{Provider: ref.Provider, Type: ref.Type, Name: artifactBucketName(ref.Name)}
}

// Get reports the real bucket's state, but only once ownership is
// confirmed — Cloud Control's GetResource resolving a bucket name is not,
// by itself, evidence this account has anything to do with it.
//
// # The bug this closes
//
// S3 bucket names are unique globally, across every AWS account, not just
// this one, and Cloud Control's GetResource for AWS::S3::Bucket resolves
// that global namespace without checking ownership at all. Verified
// against the live Evatt Labs account (409032463870, us-east-1) with a
// generic environment name ("dev") that this package's own
// artifactBucketName would derive into "dev-api-artifacts":
//
//	$ aws s3api list-buckets --query "Buckets[?contains(Name,'dev-api')].Name"
//	[]                                          # we own no such bucket
//	$ aws s3api head-bucket --bucket dev-api-artifacts
//	An error occurred (403) when calling the HeadBucket operation: Forbidden
//	                                            # it exists, owned by someone else
//	$ aws cloudcontrol get-resource --type-name AWS::S3::Bucket --identifier dev-api-artifacts
//	{"BucketName":"dev-api-artifacts","Arn":"arn:aws:s3:::dev-api-artifacts", ...}
//	                                            # full success
//
// Before this check existed, a.inner.Get's success here was read as "this
// environment's own bucket exists, no change needed" unconditionally.
// Generic environment names (dev, qa, test, staging — the names any team
// reaches for first) make this likely, not exotic: "dev-api-artifacts"
// was already taken by a stranger on the very first try against a real
// account. See this workstream's PR description for the full
// destroy/apply consequence chain a false no-change here enables.
//
// # A foreign bucket reads as absent, not as an error
//
// Client.OwnsBucket answers "does this account own it," definitively —
// see that method's own doc comment for why it, not HeadBucket, is what
// answers this and how it avoids the ambiguous-403 trap a HeadBucket-based
// check would fall into. A confirmed-foreign bucket is reported here
// exactly like a bucket Cloud Control never found at all: (nil, nil).
// plan's own contract already treats that as "propose ActionCreate" (see
// resource.Resource.Get's own doc comment) — which is the honest outcome
// this workstream's brief asks for: apply then genuinely tries to create
// the bucket, and S3 itself refuses with BucketAlreadyExists, naming the
// real problem instead of kraai silently reusing a stranger's bucket.
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

	// Report the state under the caller's own Ref (the service-derived
	// name), not the transformed bucket name: everything above this type
	// (plan, the applier) identifies this resource by the Ref the
	// registry handed it, and substituting a different Ref.Name here would
	// make this resource's identity inconsistent with every other Tier 2
	// registration for the same service.
	state.Ref = ref
	return state, nil
}

// recordForeignBucket leaves a diagnostic trail for a bucket this method
// or Delete declined to treat as this account's own, rather than letting
// that decision vanish silently into a bare (nil, nil) or nil return — an
// operator staring at an unexpected "create" or a skipped delete in a
// plan/destroy run has no other way to learn why.
//
// Recorded as a span event on ctx's active span (a no-op when tracing
// isn't active, e.g. every test in this package) rather than a log line:
// this package has no logger of its own — internal/resource's own
// Instrument decorator is the only structured-output mechanism
// anything above this package already wires up, and every Get/Delete call
// this method can be reached from is already running inside the span that
// decorator starts (resource/otel.go's instrumented.observe wraps ctx
// before calling into the inner Resource). Riding that existing span
// keeps this diagnostic attached to the exact call it explains, with no
// new package dependency and no forbidigo violation (fmt.Print* is
// forbidden outside cmd/kraai, which owns all process-level output).
func recordForeignBucket(ctx context.Context, bucketName, message string) {
	trace.SpanFromContext(ctx).AddEvent(message, trace.WithAttributes(
		attribute.String("kraai.bucket_name", bucketName),
	))
}

func (a *artifactBucketResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	realName := artifactBucketName(spec.Name)

	realSpec := spec
	realSpec.Name = realName
	// Config must be replaced, not merely carried through: spec.Config on
	// entry is expandCompute's generic compute shape (dir, settings,
	// trigger, handler, schedule), none of which is BucketName — the
	// property this bare byName type's own primary identifier is
	// actually built from. Submitting the generic shape unchanged would
	// hand Cloud Control a desired state with no BucketName at all; S3
	// treats an absent bucket name as "generate one," so every apply would
	// create a new, differently-named bucket that Get (which looks up
	// realName specifically) can never find again — the exact
	// self-inflicted, permanent duplication PR #80's third review pass
	// named as the risk this whole package's registrations needed
	// auditing for.
	realSpec.Config = map[string]any{"BucketName": realName}

	state, err := a.inner.Create(ctx, realSpec)
	if err != nil || state == nil {
		return state, err
	}
	state.Ref.Name = spec.Name
	return state, nil
}

// Update is refused: this bucket has no configuration a manifest declares
// or a service needs changed in place — its only job is to exist and hold
// objects Lambda's Create/Update paths upload directly via S3 PutObject
// (client.go), outside Cloud Control entirely. A difference here would
// only ever be this bucket's own name changing, which Delete-then-Create
// already handles as a replacement.
func (a *artifactBucketResource) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	return nil, kerrors.Wrap(resource.ErrImmutable, kerrors.CodeValidation,
		"an artifact bucket has no in-place configuration; replace it instead")
}

// Delete empties the real bucket before deleting it.
//
// # The bug this closes
//
// Before this method emptied anything, Delete forwarded straight to
// a.inner.Delete — the generic Cloud Control engine — which issues a bare
// DeleteResource. S3's own delete handler refuses a non-empty bucket, and
// this bucket is never empty by the time teardown runs it: lambda.go's
// PutObject has already uploaded at least one content-addressed artifact
// object into it (artifactObjectKey) by the time a service's Lambda
// function exists to delete. A real `kraai destroy` against a live AWS
// account reproduced exactly this:
//
//	destroy for "kraaiapi-pull-request-00001": 10 deleted, 0 skipped, 2 failed (12 total)
//	  !  failed  ...ArtifactBucket  api.api   deleting AWS::S3::Bucket "kraaiapi-pull-request-00001-api-artifacts":
//	     operation ended FAILED (error code "GeneralServiceException"): The bucket you tried to delete
//	     is not empty (Service: S3, Status Code: 409 ...)
//
// Both this service's buckets ("-api-artifacts" and "-tick-artifacts")
// then remained in the account indefinitely, still billable, with every
// future destroy of the same environment hitting the identical failure —
// exactly the silent-accumulation failure mode kraai's ephemeral-
// environment thesis exists to prevent (this workstream's brief).
//
// # Emptying runs before the real bucket is resolved for delete
//
// EmptyBucket (client.go) is called against the same real bucket name
// bucketRef(ref) already resolves — the same rewrite Get and Create use,
// for the same reason (see bucketRef's own doc comment): the caller's Ref
// still carries the service-derived name, never the real S3 bucket name,
// so calling EmptyBucket against ref.Name directly would target a bucket
// that was never the one this resource actually owns.
//
// # Why emptying does not itself decide whether to call inner.Delete
//
// EmptyBucket already returns nil for a bucket that does not exist (see
// its own doc comment) rather than a sentinel this method would need to
// branch on, and a.inner.Delete already tolerates a bucket that does not
// exist: this type's byName lookup treats the derived name as the
// identifier unconditionally (resourceType.resolve), so Delete always
// reaches Client.DeleteResource, whose own contract already treats a
// ResourceNotFoundException/NotFound terminal state as success (see that
// method's doc comment). Running both steps unconditionally, in order,
// means an already-fully-deleted bucket takes the exact same path as a
// bucket freshly emptied here, with no extra branching either method needs
// to expose to the other.
//
// # Ownership is checked here too, not only in Get, and cannot be bypassed
//
// Emptying a bucket (ListObjectsV2 then DeleteObjects) is the genuinely
// destructive half of this whole file — see Get's own doc comment for the
// live foreign-bucket evidence that motivated checking ownership at all.
// This method re-derives that answer itself with Client.OwnsBucket rather
// than trusting that whatever called Delete already called Get first and
// would have refused to proceed on a foreign bucket: internal/destroy is
// manifest-driven: nothing kraai stores records what was created (see
// internal/resource/resource.go's
// own package doc comment — "the authoritative answer to 'does this exist'
// is always a fresh lookup"), and nothing in this type's contract
// guarantees Get ran immediately before Delete in the same process, on the
// same information, with no chance for the world to have changed between
// the two calls. A guard that only lived in Get would be bypassable by any
// future caller — inside or outside this package — that reaches Delete on
// its own, exactly the kind of gap Rule 20 (fail explicitly, never
// silently) exists to close. Putting the check inside Delete itself is the
// only placement that makes it unconditional: every path to the
// destructive calls below runs through this method, and this method now
// always asks first.
//
// A bucket this account does not own — because it was never this
// account's to begin with, or because Cloud Control's byName resolve
// found nothing at all — is treated as already gone: nothing to empty,
// nothing to delete, success. That is exactly resource.Resource.Delete's
// own "deleting something already absent is success" contract, extended
// to cover "this was never ours to delete" as the same kind of no-op
// rather than a new outcome this method would need its own branch for.
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
