package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/evatt-labs/kraai/internal/httpx"
	"github.com/evatt-labs/kraai/internal/kerrors"
)

// maxListPages bounds a ListResources page walk, as a backstop against a
// NextToken that never stops advancing — the same defensive pattern the
// Neon client uses for its cursor walk.
const maxListPages = 100

// maxEmptyBucketPages bounds EmptyBucket's page walk over ListObjectsV2, the
// same defensive backstop maxListPages applies to ListResources, against a
// NextContinuationToken that never stops advancing.
const maxEmptyBucketPages = 100

// maxOwnsBucketPages bounds OwnsBucket's page walk over ListBuckets, the
// same defensive backstop maxListPages and maxEmptyBucketPages apply to
// their own list loops, against a ContinuationToken that never stops
// advancing. In practice OwnsBucket's own Prefix filter (see its doc
// comment) narrows a real account's response to at most a handful of
// buckets, so this bound is never expected to matter — it exists for the
// same reason the other two do: an API that stops honouring its own
// pagination contract must not spin this loop forever.
const maxOwnsBucketPages = 100

// Default polling bounds for asynchronous Cloud Control operations
// (CreateResource, UpdateResource, DeleteResource). These are generous on
// the ceiling and cheap on the floor: a CloudFront distribution's
// propagation alone can run past fifteen minutes, while an S3 bucket create
// typically finishes in under a second, so the backoff starts small and
// grows rather than picking one fixed interval that is wrong for most
// resource types. WithPollTimings overrides all three, which is how tests
// exercise polling without real waiting.
const (
	defaultPollInitialDelay = 2 * time.Second
	defaultPollMaxDelay     = 30 * time.Second
	defaultPollTimeout      = 40 * time.Minute
)

// cloudControlAPI is the subset of *cloudcontrol.Client this package's
// *Client calls: the five verbs Cloud Control exposes uniformly across every
// resource type (see this package's own doc comment) plus the status poll
// the async ones require.
//
// Shaped like the SDK's own method signatures rather than this package's
// vocabulary, unlike ccAPI in resource.go: this is the seam being
// substituted in client_test.go, and the thing on the other side of it is
// the SDK client itself, which is a concrete struct with no interface of
// its own to depend on instead.
type cloudControlAPI interface {
	GetResource(ctx context.Context, params *cloudcontrol.GetResourceInput, optFns ...func(*cloudcontrol.Options)) (*cloudcontrol.GetResourceOutput, error)
	ListResources(ctx context.Context, params *cloudcontrol.ListResourcesInput, optFns ...func(*cloudcontrol.Options)) (*cloudcontrol.ListResourcesOutput, error)
	CreateResource(ctx context.Context, params *cloudcontrol.CreateResourceInput, optFns ...func(*cloudcontrol.Options)) (*cloudcontrol.CreateResourceOutput, error)
	UpdateResource(ctx context.Context, params *cloudcontrol.UpdateResourceInput, optFns ...func(*cloudcontrol.Options)) (*cloudcontrol.UpdateResourceOutput, error)
	DeleteResource(ctx context.Context, params *cloudcontrol.DeleteResourceInput, optFns ...func(*cloudcontrol.Options)) (*cloudcontrol.DeleteResourceOutput, error)
	GetResourceRequestStatus(ctx context.Context, params *cloudcontrol.GetResourceRequestStatusInput, optFns ...func(*cloudcontrol.Options)) (*cloudcontrol.GetResourceRequestStatusOutput, error)
}

// cloudFormationAPI is the subset of *cloudformation.Client this package
// calls: DescribeType, to fetch a resource type's schema.
type cloudFormationAPI interface {
	DescribeType(ctx context.Context, params *cloudformation.DescribeTypeInput, optFns ...func(*cloudformation.Options)) (*cloudformation.DescribeTypeOutput, error)
}

// s3API is the subset of *s3.Client this package calls: PutObject, to
// upload a Lambda deployment artifact to the per-environment artifact
// bucket (aws-provider-compute), ListObjectsV2/DeleteObjects, to empty an
// artifact bucket before Cloud Control deletes it (EmptyBucket below), and
// ListBuckets, to answer "do we actually own this bucket" (OwnsBucket
// below) — see that method's own doc comment for why this package needs
// that question answered at all.
//
// Deliberately not Cloud-Control-routed like every other verb in this
// package: Cloud Control manages a bucket's own existence
// (AWS::S3::Bucket's Get/Create/Delete), but has no notion of "put this
// object in it," "list what's in it," "remove these objects," or "which
// buckets does this account actually own" — object data planes and
// account-scoped listing are not part of the Cloud Control resource-
// provider surface for any type. This is the one place this package
// reaches past Cloud Control to a service-specific SDK client, for exactly
// the operations Cloud Control cannot express.
type s3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	// ListObjectsV2 lists (up to 1000) current objects in a bucket per
	// call — EmptyBucket's only listing primitive. This package never
	// enables object versioning on a bucket it creates (artifactbucket.go's
	// Create submits only BucketName, no VersioningConfiguration property,
	// and Update is refused outright for this type — see that file's
	// Delete doc comment for the full finding), so ListObjectVersions,
	// which would additionally surface noncurrent versions and delete
	// markers, is not part of this seam: adding it would be handling a
	// state this package's own bucket can never reach.
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	// DeleteObjects removes up to 1000 objects in a single request —
	// EmptyBucket's batch delete primitive, chosen over one DeleteObject
	// call per key because this runs on every teardown and the two
	// services' page/batch ceilings are identical (see EmptyBucket's doc
	// comment).
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	// ListBuckets returns the buckets this AWS account itself owns,
	// optionally narrowed by Prefix — OwnsBucket's only primitive, and the
	// one call in this seam that is inherently account-scoped rather than
	// bucket-scoped (see OwnsBucket's own doc comment for why that
	// property is exactly what makes it useful here).
	ListBuckets(ctx context.Context, params *s3.ListBucketsInput, optFns ...func(*s3.Options)) (*s3.ListBucketsOutput, error)
	// PutBucketPolicy replaces a bucket's entire policy — cloudfront.go's
	// only use of it, granting the CloudFront distribution fronting an
	// objects bucket read access to it. A bucket policy is not part of
	// AWS::S3::Bucket's own Cloud Control schema, so this is object-data-
	// plane territory in the same sense PutObject is.
	PutBucketPolicy(ctx context.Context, params *s3.PutBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error)
	// GetBucketPolicy reads a bucket's current policy, or fails with
	// NoSuchBucketPolicy/NoSuchBucket when there is none — see
	// Client.GetBucketPolicy for how those are translated to absence.
	GetBucketPolicy(ctx context.Context, params *s3.GetBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error)
	// DeleteBucketPolicy removes a bucket's policy, tolerating one already
	// absent — see Client.DeleteBucketPolicy.
	DeleteBucketPolicy(ctx context.Context, params *s3.DeleteBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketPolicyOutput, error)
}

// stsAPI is the subset of *sts.Client this package calls: GetCallerIdentity,
// to resolve the AWS account id an ARN needs.
type stsAPI interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// secretsManagerAPI is the subset of *secretsmanager.Client this package
// calls: GetSecretValue, to read the master credential RDS manages for an
// Aurora cluster at the moment a function needs it.
type secretsManagerAPI interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// Client is a thin Cloud Control + CloudFormation client.
//
// "Thin" here means its exported methods already speak this package's own
// vocabulary — a decoded properties map, a plain identifier list — rather
// than the SDK's Input/Output types. resource.go's ccAPI interface is
// satisfied by *Client precisely because of that shape, so nothing above
// this file needs to know the SDK exists.
type Client struct {
	cc  cloudControlAPI
	cf  cloudFormationAPI
	s3  s3API
	sts stsAPI
	sm  secretsManagerAPI

	// region is the resolved AWS region every ARN this package constructs
	// (see AccountID's doc comment) is built against. Set from the SDK
	// config's own resolved value in New, not from Settings.Region
	// directly — Settings.Region may be empty and deferred to the SDK's own
	// resolution chain (see Settings.Region's doc comment), and by the time
	// LoadDefaultConfig returns, cfg.Region already holds whatever that
	// chain actually settled on.
	region string

	// Polling bounds for the async verbs. Defaulted in New, overridable via
	// WithPollTimings.
	pollInitialDelay time.Duration
	pollMaxDelay     time.Duration
	pollTimeout      time.Duration

	// accountMu/accountID/accountLoaded cache the caller's AWS account id
	// for the process's lifetime, the same lazy-on-first-use, cache-only-
	// on-success shape resourceType.getSchema already uses and for the
	// identical reason: more than one Tier 2 resource (the Lambda's own
	// Role ARN, an EventBridge Rule's Target ARN) needs it, an account's id
	// cannot change mid-process, and a single transient STS throttle should
	// not be cached as a permanent failure.
	accountMu     sync.Mutex
	accountID     string
	accountLoaded bool

	// reads memoizes what Cloud Control has already answered this process,
	// so the many lookups that sweep one type (every by-tag registration
	// lists a type and reads each instance) cost one request per resource
	// rather than one per lookup. Forgotten per type on any mutation of it.
	reads readCache
}

// Option configures a Client.
type Option func(*Client)

// WithCloudControlAPI substitutes the Cloud Control caller, which is how
// tests exercise Client without an AWS account or network.
func WithCloudControlAPI(api cloudControlAPI) Option {
	return func(c *Client) { c.cc = api }
}

// WithCloudFormationAPI substitutes the CloudFormation caller.
func WithCloudFormationAPI(api cloudFormationAPI) Option {
	return func(c *Client) { c.cf = api }
}

// WithS3API substitutes the S3 caller, which is how tests exercise artifact
// upload without an AWS account or network.
func WithS3API(api s3API) Option {
	return func(c *Client) { c.s3 = api }
}

// WithSTSAPI substitutes the STS caller.
func WithSTSAPI(api stsAPI) Option {
	return func(c *Client) { c.sts = api }
}

// WithSecretsManagerAPI substitutes the Secrets Manager client, for tests.
func WithSecretsManagerAPI(api secretsManagerAPI) Option {
	return func(c *Client) { c.sm = api }
}

// WithPollTimings overrides the backoff and overall timeout used to poll an
// asynchronous operation's ProgressEvent to a terminal state. Tests use this
// to exercise polling, timeout and cancellation behavior in milliseconds
// rather than the production defaults, which are sized for real Cloud
// Control propagation times (minutes, not milliseconds).
func WithPollTimings(initialDelay, maxDelay, timeout time.Duration) Option {
	return func(c *Client) {
		c.pollInitialDelay = initialDelay
		c.pollMaxDelay = maxDelay
		c.pollTimeout = timeout
	}
}

// New builds a Client for settings.Region, authenticating via the AWS SDK's
// own default credential chain (environment, shared config, IMDS) — kraai
// never reads or handles an AWS credential itself, delegating entirely to
// the SDK's own auth resolution.
func New(ctx context.Context, settings Settings, opts ...Option) (*Client, error) {
	// awsconfig.WithHTTPClient: kraai shares one tuned HTTP client across
	// every provider for connection reuse rather than letting each SDK build
	// its own. The SDK builds its own HTTP client per service (cloudcontrol,
	// cloudformation, s3, sts) unless told otherwise, which is a fifth
	// independently-pooled client alongside internal/reachability,
	// internal/provider/neon and internal/provider/cloudflare. httpx.NewClient's *http.Client satisfies
	// the SDK's minimal HTTPClient interface (a Do(*http.Request) method),
	// so this puts every AWS call through the same shared pool and the same
	// otelhttp instrumentation as everything else, without replacing any of
	// the SDK's own retry or credential-resolution behaviour — WithHTTPClient
	// only substitutes the transport those layers run on top of.
	//
	// Adaptive retries with a longer budget: Cloud Control throttles a plan's
	// burst of reads well before the SDK's three standard attempts are
	// spent, and adaptive mode paces the client to the limit it hits rather
	// than failing the plan on it.
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(settings.Region),
		awsconfig.WithHTTPClient(httpx.NewClient(60*time.Second, nil, nil)),
		awsconfig.WithRetryMode(aws.RetryModeAdaptive),
		awsconfig.WithRetryMaxAttempts(cloudControlMaxAttempts),
	)
	if err != nil {
		// LoadDefaultConfig's error can name a credential file path but never
		// a credential value, so wrapping it is safe.
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "loading AWS configuration for region %q", settings.Region)
	}

	c := &Client{
		cc:               cloudcontrol.NewFromConfig(cfg),
		cf:               cloudformation.NewFromConfig(cfg),
		s3:               s3.NewFromConfig(cfg),
		sts:              sts.NewFromConfig(cfg),
		sm:               secretsmanager.NewFromConfig(cfg),
		region:           cfg.Region,
		pollInitialDelay: defaultPollInitialDelay,
		pollMaxDelay:     defaultPollMaxDelay,
		pollTimeout:      defaultPollTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// GetResource returns typeName/identifier's current properties, or
// found=false when Cloud Control reports the resource does not exist.
//
// # Absence versus failure
//
// ResourceNotFoundException is the only outcome translated to found=false,
// err=nil. Every other error — throttling, access denied, a network
// failure, a malformed response — is returned as a real error with
// found=false and must not be read as absence: Get's caller (and, through
// it, teardown) treats "does not exist" as "already deleted, keep going",
// so misreading an outage as absence would orphan a real resource.
func (c *Client) GetResource(ctx context.Context, typeName, identifier string) (map[string]any, bool, error) {
	if properties, found, hit := c.reads.get(typeName, identifier); hit {
		return properties, found, nil
	}
	out, err := c.cc.GetResource(ctx, &cloudcontrol.GetResourceInput{
		TypeName:   aws.String(typeName),
		Identifier: aws.String(identifier),
	})
	if err != nil {
		var notFound *cctypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			c.reads.putGet(typeName, identifier, "", false)
			return nil, false, nil
		}
		return nil, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "getting %s %q", typeName, identifier)
	}
	if out.ResourceDescription == nil || out.ResourceDescription.Properties == nil {
		return nil, false, kerrors.Validation("GetResource for %s %q returned no properties", typeName, identifier)
	}

	var properties map[string]any
	if err := json.Unmarshal([]byte(*out.ResourceDescription.Properties), &properties); err != nil {
		return nil, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding properties for %s %q", typeName, identifier)
	}
	c.reads.putGet(typeName, identifier, *out.ResourceDescription.Properties, true)
	return properties, true, nil
}

// ListResources returns the primary identifier of every instance of
// typeName Cloud Control can see, across all pages.
//
// Deliberately returns identifiers only, never properties. Cloud Control's
// own documentation guarantees only the identifier per resource — "it may
// include part or all of the resource's properties" — and its own worked
// example shows a type (Kinesis streams) whose list response carries a
// single property while everything else requires GetResource. Exposing a
// partial, type-dependent properties map here would invite exactly the bug
// this package's byAttr/byTag lookups must not have: trusting a field that
// List never promised to populate.
//
// # resourceModel: parent-scoped types
//
// Most Cloud Control list handlers enumerate every instance of typeName in
// the account/region and resourceModel is nil. A parent-scoped type's list
// handler instead requires a ResourceModel identifying the parent whose
// instances to list — AWS::Lambda::Permission is the first this package
// registers (resourceType.listScope), verified directly against Cloud
// Control on the live Evatt Labs account (409032463870, us-east-1):
//
//	$ aws cloudcontrol list-resources --type-name AWS::Lambda::Permission --region us-east-1
//	InvalidRequestException: Missing or invalid ResourceModel property in
//	AWS::Lambda::Permission list handler request input. Required property:
//	(#: required key [FunctionName] not found)
//
//	$ aws cloudcontrol list-resources --type-name AWS::Lambda::Permission \
//	    --resource-model '{"FunctionName":"does-not-exist"}' --region us-east-1
//	ResourceNotFoundException: AWS::Lambda::Permission Handler returned
//	status FAILED: The resource you requested does not exist.
//	(HandlerErrorCode: NotFound)
//
// The second call is the reason resourceModel != nil changes error
// handling below, not just the request: naming a parent that does not
// exist is reported as ResourceNotFoundException, not an empty result —
// unlike every unscoped list handler this package has observed, where "no
// instances" and "no error" are the same response. Translated to an empty
// identifier list here, deliberately mirroring GetResource's own
// absence-versus-failure contract: a permission's parent function not
// existing yet means the permission does not exist yet either, which is
// exactly the (nil, nil) Get must return for plan to report "create," not
// "failed" (see resource.Resource's own doc comment). This translation is
// gated on resourceModel != nil precisely because it was only ever
// observed for a scoped list; an unscoped ListResources returning
// ResourceNotFoundException remains a real, unexpected error, exactly as
// it was before this parameter existed.
func (c *Client) ListResources(ctx context.Context, typeName string, resourceModel map[string]any) ([]string, error) {
	var modelJSON *string
	if resourceModel != nil {
		body, err := json.Marshal(resourceModel)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding resource model for listing %s", typeName)
		}
		s := string(body)
		modelJSON = &s
	}
	model := ""
	if modelJSON != nil {
		model = *modelJSON
	}
	if identifiers, hit := c.reads.list(typeName, model); hit {
		return identifiers, nil
	}

	var identifiers []string
	var nextToken *string

	for range maxListPages {
		out, err := c.cc.ListResources(ctx, &cloudcontrol.ListResourcesInput{
			TypeName:      aws.String(typeName),
			ResourceModel: modelJSON,
			NextToken:     nextToken,
		})
		if err != nil {
			var notFoundType *cctypes.TypeNotFoundException
			if errors.As(err, &notFoundType) {
				return nil, kerrors.Wrap(err, kerrors.CodeValidation, "AWS Cloud Control has no registered type %q", typeName)
			}
			if modelJSON != nil {
				var notFoundResource *cctypes.ResourceNotFoundException
				if errors.As(err, &notFoundResource) {
					return nil, nil
				}
			}
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "listing %s", typeName)
		}
		for _, desc := range out.ResourceDescriptions {
			if desc.Identifier != nil {
				identifiers = append(identifiers, *desc.Identifier)
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			c.reads.putList(typeName, model, identifiers)
			return identifiers, nil
		}
		nextToken = out.NextToken
	}
	return nil, kerrors.Validation("listing %s did not terminate within %d pages", typeName, maxListPages)
}

// pollTimings resolves the effective backoff and timeout, falling back to
// the production defaults when a Client was built by a struct literal
// rather than New — every existing test in this package does exactly that
// (e.g. &Client{cc: cc}), and a zero-value delay would otherwise make the
// poll loop busy-spin instead of backing off.
func (c *Client) pollTimings() (initialDelay, maxDelay, timeout time.Duration) {
	initialDelay, maxDelay, timeout = c.pollInitialDelay, c.pollMaxDelay, c.pollTimeout
	if initialDelay <= 0 {
		initialDelay = defaultPollInitialDelay
	}
	if maxDelay <= 0 {
		maxDelay = defaultPollMaxDelay
	}
	if timeout <= 0 {
		timeout = defaultPollTimeout
	}
	return initialDelay, maxDelay, timeout
}

// pollToTerminal polls requestToken's status until it reaches a terminal
// OperationStatus (SUCCESS, FAILED or CANCEL_COMPLETE), or until an
// infra-level failure: the status call itself erroring, an empty response,
// context cancellation, or this poll's own timeout elapsing.
//
// It deliberately does not decide whether a terminal FAILED counts as an
// application-level error — Delete's "deleting something already absent is
// success" contract needs to inspect the terminal event's ErrorCode itself
// (NotFound is success for Delete, a real failure for Create/Update), so
// returning the raw terminal event and letting each verb interpret it keeps
// that decision where the contract actually lives, rather than baking one
// verb's success criteria into the shared poll loop.
//
// Backoff starts at initialDelay and doubles up to maxDelay on every
// non-terminal response, honouring Cloud Control's own RetryAfter hint when
// it is later than the computed backoff would be. Never spins without a
// delay: every iteration either returns or waits.
func (c *Client) pollToTerminal(ctx context.Context, requestToken, typeName, identifier string) (*cctypes.ProgressEvent, error) {
	initialDelay, maxDelay, timeout := c.pollTimings()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	delay := initialDelay
	var lastStatus cctypes.OperationStatus
	for {
		out, err := c.cc.GetResourceRequestStatus(ctx, &cloudcontrol.GetResourceRequestStatusInput{
			RequestToken: aws.String(requestToken),
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, kerrors.Wrap(ctx.Err(), kerrors.CodeUnexpected,
					"polling %s %q timed out or was cancelled (last known status %q)", typeName, identifier, lastStatus)
			}
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "polling status of %s %q", typeName, identifier)
		}
		if out.ProgressEvent == nil {
			return nil, kerrors.Validation("polling %s %q returned no progress event", typeName, identifier)
		}
		event := out.ProgressEvent
		lastStatus = event.OperationStatus

		switch event.OperationStatus {
		case cctypes.OperationStatusSuccess, cctypes.OperationStatusFailed, cctypes.OperationStatusCancelComplete:
			return event, nil
		}

		wait := delay
		if event.RetryAfter != nil {
			if until := time.Until(*event.RetryAfter); until > wait {
				wait = until
			}
		}
		select {
		case <-ctx.Done():
			return nil, kerrors.Wrap(ctx.Err(), kerrors.CodeUnexpected,
				"polling %s %q timed out or was cancelled (last known status %q)", typeName, identifier, lastStatus)
		case <-time.After(wait):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// terminalFailureCodes are the HandlerErrorCodes that mean the caller did
// something the API will never accept as-is — a validation problem, not an
// unexpected platform failure. Everything else (throttling, internal
// errors, network failures, an unset code) is CodeUnexpected: retryable or
// unknown, not something the caller can fix by changing the manifest.
var terminalFailureCodes = map[cctypes.HandlerErrorCode]bool{
	cctypes.HandlerErrorCodeNotUpdatable:                 true,
	cctypes.HandlerErrorCodeInvalidRequest:               true,
	cctypes.HandlerErrorCodeAccessDenied:                 true,
	cctypes.HandlerErrorCodeUnauthorizedTaggingOperation: true,
	cctypes.HandlerErrorCodeInvalidCredentials:           true,
	cctypes.HandlerErrorCodeAlreadyExists:                true,
	cctypes.HandlerErrorCodeNotFound:                     true,
	cctypes.HandlerErrorCodeResourceConflict:             true,
	cctypes.HandlerErrorCodeServiceLimitExceeded:         true,
}

// translateFailure maps a terminal FAILED/CANCEL_COMPLETE ProgressEvent onto
// a kerrors bucket, naming the resource and the last status per this
// workstream's requirement that a stuck or failed operation return a useful
// error rather than a bare "it failed."
func translateFailure(op, typeName, identifier string, event *cctypes.ProgressEvent) error {
	statusMessage := "no status message"
	if event.StatusMessage != nil && *event.StatusMessage != "" {
		statusMessage = *event.StatusMessage
	}

	code := kerrors.CodeUnexpected
	if terminalFailureCodes[event.ErrorCode] {
		code = kerrors.CodeValidation
	}
	return kerrors.Wrap(errors.New(statusMessage), code,
		"%s %s %q: operation ended %s (error code %q)", op, typeName, identifier, event.OperationStatus, event.ErrorCode)
}

// decodeResourceModel decodes a terminal ProgressEvent's ResourceModel —
// "a JSON string containing the resource model, consisting of each resource
// property and its current value" per Cloud Control's own documentation —
// into this package's plain properties map. Read from the ProgressEvent
// itself rather than issuing a follow-up GetResource: Cloud Control already
// promises the final model on SUCCESS, and a follow-up call would just be
// an extra round trip to re-fetch what the operation already returned.
func decodeResourceModel(model *string) (map[string]any, error) {
	if model == nil || *model == "" {
		return map[string]any{}, nil
	}
	var properties map[string]any
	if err := json.Unmarshal([]byte(*model), &properties); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding resource model")
	}
	return properties, nil
}

// CreateResource submits desiredState for creation and polls to a terminal
// state, returning the provider-assigned identifier and the resulting
// properties.
func (c *Client) CreateResource(ctx context.Context, typeName string, desiredState map[string]any) (string, map[string]any, error) {
	// Before and after: a read of this type in flight during the call must
	// not repopulate the cache with the world as it was.
	c.reads.forget(typeName)
	defer c.reads.forget(typeName)
	body, err := json.Marshal(desiredState)
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding desired state for %s", typeName)
	}

	out, err := c.cc.CreateResource(ctx, &cloudcontrol.CreateResourceInput{
		TypeName:     aws.String(typeName),
		DesiredState: aws.String(string(body)),
	})
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "creating %s", typeName)
	}
	if out.ProgressEvent == nil || out.ProgressEvent.RequestToken == nil {
		return "", nil, kerrors.Validation("CreateResource for %s returned no request token", typeName)
	}

	event, err := c.pollToTerminal(ctx, *out.ProgressEvent.RequestToken, typeName, "<pending>")
	if err != nil {
		return "", nil, err
	}
	if event.OperationStatus != cctypes.OperationStatusSuccess {
		identifier := ""
		if event.Identifier != nil {
			identifier = *event.Identifier
		}
		return "", nil, translateFailure("creating", typeName, identifier, event)
	}

	identifier := ""
	if event.Identifier != nil {
		identifier = *event.Identifier
	}
	properties, err := decodeResourceModel(event.ResourceModel)
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding created %s %q", typeName, identifier)
	}
	return identifier, properties, nil
}

// UpdateResource submits patch (an RFC 6902 JSON Patch document) against
// identifier and polls to a terminal state, returning the resulting
// properties.
func (c *Client) UpdateResource(ctx context.Context, typeName, identifier string, patch []byte) (map[string]any, error) {
	c.reads.forget(typeName)
	defer c.reads.forget(typeName)
	out, err := c.cc.UpdateResource(ctx, &cloudcontrol.UpdateResourceInput{
		TypeName:      aws.String(typeName),
		Identifier:    aws.String(identifier),
		PatchDocument: aws.String(string(patch)),
	})
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "updating %s %q", typeName, identifier)
	}
	if out.ProgressEvent == nil || out.ProgressEvent.RequestToken == nil {
		return nil, kerrors.Validation("UpdateResource for %s %q returned no request token", typeName, identifier)
	}

	event, err := c.pollToTerminal(ctx, *out.ProgressEvent.RequestToken, typeName, identifier)
	if err != nil {
		return nil, err
	}
	if event.OperationStatus != cctypes.OperationStatusSuccess {
		return nil, translateFailure("updating", typeName, identifier, event)
	}

	properties, err := decodeResourceModel(event.ResourceModel)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding updated %s %q", typeName, identifier)
	}
	return properties, nil
}

// DeleteResource submits a delete for identifier and polls to a terminal
// state. Deleting something already absent is success, exactly as Get
// reports absence rather than failure (see resource.Resource's doc
// comment): that holds both when Cloud Control rejects the delete
// synchronously with ResourceNotFoundException, and when the async delete
// handler itself discovers the resource is already gone and reports a
// terminal FAILED with HandlerErrorCodeNotFound.
func (c *Client) DeleteResource(ctx context.Context, typeName, identifier string) error {
	c.reads.forget(typeName)
	defer c.reads.forget(typeName)
	out, err := c.cc.DeleteResource(ctx, &cloudcontrol.DeleteResourceInput{
		TypeName:   aws.String(typeName),
		Identifier: aws.String(identifier),
	})
	if err != nil {
		var notFound *cctypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return nil
		}
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting %s %q", typeName, identifier)
	}
	if out.ProgressEvent == nil || out.ProgressEvent.RequestToken == nil {
		return kerrors.Validation("DeleteResource for %s %q returned no request token", typeName, identifier)
	}

	event, err := c.pollToTerminal(ctx, *out.ProgressEvent.RequestToken, typeName, identifier)
	if err != nil {
		return err
	}
	if event.OperationStatus == cctypes.OperationStatusSuccess {
		return nil
	}
	if event.ErrorCode == cctypes.HandlerErrorCodeNotFound {
		return nil
	}
	return translateFailure("deleting", typeName, identifier, event)
}

// Schema is the subset of a CloudFormation resource-provider schema this
// package currently decodes.
//
// DescribeType's Schema field is the full resource-provider schema as a JSON
// string — up to 116KB for AWS::CloudFront::Distribution, per the
// aws-provider-core workstream's own measurement — carrying properties,
// required, handlers and more. Only the fields relevant to identity and
// replacement detection are decoded here; the rest is left for the
// write-path workstream that actually needs schema-driven diffing.
type Schema struct {
	// PrimaryIdentifier is the property path (or paths, for a compound
	// identifier) Cloud Control treats as this type's primary identifier.
	PrimaryIdentifier []string `json:"primaryIdentifier"`
	// CreateOnlyProperties are the properties that force a replacement
	// rather than an in-place update — the authoritative source for
	// plan's replace-or-update decision, consumed via resourceType's
	// Diff (plan.Differ).
	CreateOnlyProperties []string `json:"createOnlyProperties"`
	// WriteOnlyProperties are accepted on create and update but never
	// returned by a read — a Lambda function's Code, say. Excluded from the
	// mutable comparison in resourceType.Diff: comparing a value the vendor
	// will never echo back would report every such property as drifted on
	// every plan, forever.
	WriteOnlyProperties []string `json:"writeOnlyProperties"`
	// Handlers lists this type's implemented verbs by name ("create",
	// "read", "update", "delete", "list"). Decoded as raw JSON because this
	// package only ever asks whether a key is present — an
	// IMMUTABLE-provisioning type (create/read/delete, no update handler)
	// omits "update" entirely rather than declaring it empty, so presence of
	// the key is the whole signal HasUpdateHandler needs.
	Handlers map[string]json.RawMessage `json:"handlers"`
	// Tagging carries the actions the type's tag handling needs, beside
	// the per-verb handler permissions; both feed the policy kraai prints
	// for a manifest (Permissions).
	Tagging struct {
		Permissions []string `json:"permissions"`
	} `json:"tagging"`
}

// listHandler is the part of a schema's list handler this package reads: the
// input model the handler requires. Cloud Control's list handlers for a
// parent-scoped type (a function's permissions, a domain's mappings, a
// zone's records) refuse an unscoped call, and say so here rather than only
// in the error that comes back.
type listHandler struct {
	HandlerSchema struct {
		Required []string `json:"required"`
		OneOf    []struct {
			Required []string `json:"required"`
		} `json:"oneOf"`
	} `json:"handlerSchema"`
}

// ListRequirements returns the property sets a list call must supply, as
// alternatives: satisfying any one is enough. Empty when the list handler
// declares no input model, which is every type that lists the whole
// account unscoped.
//
// AWS::Lambda::Permission declares required [FunctionName], one
// alternative. AWS::Route53::RecordSet declares a oneOf of [HostedZoneId]
// and [HostedZoneName], two. A required list beside a oneOf applies to
// every alternative.
func (s Schema) ListRequirements() [][]string {
	raw, ok := s.Handlers["list"]
	if !ok {
		return nil
	}
	var handler listHandler
	if err := json.Unmarshal(raw, &handler); err != nil {
		// A list handler this package cannot read is one it cannot check;
		// the call proceeds and Cloud Control's own error stands.
		return nil
	}
	base := handler.HandlerSchema.Required
	if len(handler.HandlerSchema.OneOf) == 0 {
		if len(base) == 0 {
			return nil
		}
		return [][]string{base}
	}
	alternatives := make([][]string, 0, len(handler.HandlerSchema.OneOf))
	for _, alt := range handler.HandlerSchema.OneOf {
		alternatives = append(alternatives, append(append([]string(nil), base...), alt.Required...))
	}
	return alternatives
}

// HasUpdateHandler reports whether this type's schema declares an update
// handler at all. A type without one cannot be reconciled in place — Update
// must refuse with resource.ErrImmutable rather than attempt a call Cloud
// Control will reject, per this workstream's brief.
func (s Schema) HasUpdateHandler() bool {
	_, ok := s.Handlers["update"]
	return ok
}

// DescribeType fetches and decodes typeName's CloudFormation resource
// provider schema.
//
// Fetched at runtime on first use per type and cached for the process's
// lifetime (resourceType.getSchema), never vendored: this package's own
// measurement found schema sizes up to 116KB (see the Schema type's own doc
// comment), and a resource type's schema does not
// change within a single kraai invocation, so one DescribeType call per type
// per process is the right amount of caching — enough to avoid repeating an
// expensive call on every Get/Create/Update/Diff, not so much
// that a real schema change (a new AWS API version) would need vendored
// files kept in sync by hand.
func (c *Client) DescribeType(ctx context.Context, typeName string) (Schema, error) {
	out, err := c.cf.DescribeType(ctx, &cloudformation.DescribeTypeInput{
		Type:     cftypes.RegistryTypeResource,
		TypeName: aws.String(typeName),
	})
	if err != nil {
		var notFound *cftypes.TypeNotFoundException
		if errors.As(err, &notFound) {
			return Schema{}, kerrors.Wrap(err, kerrors.CodeValidation, "no CloudFormation resource provider schema is registered for %q", typeName)
		}
		return Schema{}, kerrors.Wrap(err, kerrors.CodeUnexpected, "describing type %s", typeName)
	}
	if out.Schema == nil {
		return Schema{}, kerrors.Validation("DescribeType for %s returned no schema", typeName)
	}

	var schema Schema
	if err := json.Unmarshal([]byte(*out.Schema), &schema); err != nil {
		return Schema{}, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding schema for %s", typeName)
	}
	return schema, nil
}

// Region returns the AWS region this Client resolved at construction (see
// the region field's own doc comment).
func (c *Client) Region() string { return c.region }

// PutObject uploads body to bucket/key, replacing any existing object at
// that key.
//
// No existence check first: S3 PutObject is itself an overwrite, so a
// HeadObject-then-PutObject round trip would only add a request without
// changing the outcome. This package's callers additionally never call it
// with the same key twice for different content — the artifact key is a
// content hash (aws-provider-compute's zip determinism), so a repeat
// PutObject for an unchanged input writes back bytes S3 already holds.
func (c *Client) PutObject(ctx context.Context, bucket, key string, body []byte) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "uploading s3://%s/%s", bucket, key)
	}
	return nil
}

// EmptyBucket deletes every object in bucket, so a subsequent
// AWS::S3::Bucket delete (Client.DeleteResource, routed through Cloud
// Control) can succeed. This closes a real, reproduced failure: `kraai
// destroy` against a live account left every artifact bucket behind,
// because S3 refuses to delete a non-empty bucket —
//
//	deleting AWS::S3::Bucket "kraaiapi-pull-request-00001-api-artifacts":
//	operation ended FAILED (error code "GeneralServiceException"): The
//	bucket you tried to delete is not empty (Service: S3, Status Code: 409)
//
// — and artifactBucketResource.Delete used to hand that delete straight to
// the generic Cloud Control engine with no emptying step at all.
//
// # Why this is a Client method, not Cloud Control logic
//
// Emptying a bucket is exactly the kind of object-data-plane operation
// Cloud Control cannot express (see the s3API doc comment): it belongs
// beside PutObject, the one other place this package reaches past Cloud
// Control to the S3 SDK directly, not inside DeleteResource's generic
// poll-to-terminal machinery, which has no notion of a bucket's contents
// at all.
//
// # One DeleteObjects call per ListObjectsV2 page, not list-then-rebatch
//
// ListObjectsV2 returns at most 1000 keys per page; DeleteObjects accepts
// at most 1000 keys per request. Those ceilings are identical, so each
// page is deleted as its own batch immediately, rather than accumulating
// every key across every page in memory and re-chunking afterward — fewer
// round trips than one DeleteObject per key (this runs on every teardown),
// and no unbounded buffering for a bucket with many objects. The loop
// keeps following NextContinuationToken while IsTruncated is true, so a
// bucket with more than 1000 objects is still fully emptied, not just its
// first page.
//
// # No version or delete-marker handling — verified, not assumed
//
// artifactbucket.go's Create submits only {"BucketName": realName} as
// desired state (see that file's own doc comment on why Config must be
// replaced rather than carried through), never a VersioningConfiguration
// property, and Update is refused outright for this type — there is no
// path through this package that ever turns versioning on for a bucket it
// created. S3 buckets are unversioned by default. A ListObjectsV2 pass
// over current objects therefore already sees everything a kraai-created
// artifact bucket can ever hold; there are no noncurrent versions or
// delete markers to additionally list and remove. Handling them anyway
// would be speculative code for a state this package's own bucket cannot
// reach — the s3API doc comment records the same finding at the interface
// boundary.
//
// # Absence is success, per resource.Resource.Delete's own contract
//
// A NoSuchBucket error from the listing call means the bucket is already
// gone: teardown must be retryable (internal/destroy/doc.go), and a
// destroy that already emptied and deleted this bucket on a prior,
// partially-failed run must be able to finish cleanly on a retry rather
// than erroring on a bucket that no longer exists. Every other failure —
// access denied, throttling, a malformed response, an object-level error
// reported inside a nominally successful DeleteObjects response — is real
// and is returned wrapped with kerrors, naming the bucket, never silently
// treated as if it meant the same thing as absence.
func (c *Client) EmptyBucket(ctx context.Context, bucket string) error {
	var token *string
	for range maxEmptyBucketPages {
		out, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			ContinuationToken: token,
		})
		if err != nil {
			var notFound *s3types.NoSuchBucket
			if errors.As(err, &notFound) {
				return nil
			}
			return kerrors.Wrap(err, kerrors.CodeUnexpected, "listing objects in bucket %q", bucket)
		}

		if len(out.Contents) > 0 {
			ids := make([]s3types.ObjectIdentifier, len(out.Contents))
			for i, obj := range out.Contents {
				ids[i] = s3types.ObjectIdentifier{Key: obj.Key}
			}
			delOut, err := c.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(bucket),
				Delete: &s3types.Delete{Objects: ids},
			})
			if err != nil {
				return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting objects from bucket %q", bucket)
			}
			// A DeleteObjects call can return a 200 OK overall while
			// individual keys failed — S3 reports those per-object, in
			// Errors, not as a Go error from the call itself. Surfacing
			// only the first is enough to make the failure actionable
			// without flooding the caller with a redundant list when many
			// keys fail for the same reason (e.g. one access-denied
			// policy blocking every delete in the batch).
			if len(delOut.Errors) > 0 {
				first := delOut.Errors[0]
				key, code, msg := "<unknown key>", "<unknown code>", "<no message>"
				if first.Key != nil {
					key = *first.Key
				}
				if first.Code != nil {
					code = *first.Code
				}
				if first.Message != nil {
					msg = *first.Message
				}
				return kerrors.Wrap(errors.New(msg), kerrors.CodeUnexpected,
					"deleting %d of %d object(s) from bucket %q failed (e.g. key %q: %s)",
					len(delOut.Errors), len(ids), bucket, key, code)
			}
		}

		if out.IsTruncated == nil || !*out.IsTruncated || out.NextContinuationToken == nil {
			return nil
		}
		token = out.NextContinuationToken
	}
	return kerrors.Validation("emptying bucket %q did not terminate within %d pages", bucket, maxEmptyBucketPages)
}

// OwnsBucket reports whether bucket belongs to this AWS account.
//
// # The bug this closes
//
// S3 bucket names are unique globally, across every AWS account on Earth,
// not just within this one — and Cloud Control's GetResource for
// AWS::S3::Bucket resolves that global namespace without checking
// ownership at all. Verified against the live Evatt Labs account
// (409032463870, us-east-1):
//
//	$ aws s3api list-buckets --query "Buckets[?contains(Name,'dev-api')].Name"
//	[]                                          # we own no such bucket
//
//	$ aws s3api head-bucket --bucket dev-api-artifacts
//	An error occurred (403) when calling the HeadBucket operation: Forbidden
//	                                            # it exists, owned by someone else
//
//	$ aws cloudcontrol get-resource --type-name AWS::S3::Bucket --identifier dev-api-artifacts
//	{"BucketName":"dev-api-artifacts","Arn":"arn:aws:s3:::dev-api-artifacts", ...}
//	                                            # full success
//
// Before this method existed, artifactBucketResource.Get trusted Cloud
// Control's success unconditionally: a stranger's bucket that merely
// happened to collide with this environment's derived name (dev,
// dev-api-artifacts — exactly the kind of generic name every environment
// produces) read as "exists, no change needed." destroy would then attempt
// EmptyBucket/DeleteResource against a bucket kraai does not own, and
// apply would skip creating the real bucket and upload the Lambda artifact
// straight into a stranger's. See this workstream's PR description for the
// full consequence chain.
//
// # Why ListBuckets, not HeadBucket + ExpectedBucketOwner
//
// S3 supports an expected-owner check on many operations
// (x-amz-expected-bucket-owner, the Go SDK's ExpectedBucketOwner field),
// and HeadBucket is the obvious first candidate for an ownership-aware
// existence check. Verified against the live account that it does not
// work for this: HeadBucket on the foreign bucket above returns an
// identical 403 Forbidden whether or not ExpectedBucketOwner is set to
// this account's own id, and a HeadBucket on a bucket name that does not
// exist at all returns 404 either way — the ExpectedBucketOwner mismatch
// produces no distinguishable signal from a bare access-denied. This is
// not a gap in this package's probing; AWS's own HeadBucket reference
// documents it as intentional: "If the bucket doesn't exist or you don't
// have permission to access it, the HEAD request returns a generic 400 Bad
// Request, 403 Forbidden, or 404 Not Found HTTP status code. A message
// body isn't included, so you can't determine the exception beyond these
// HTTP response codes." A 403 from HeadBucket therefore always carries the
// same ambiguity this workstream's brief named explicitly: it cannot be
// told apart from "this bucket is ours, but the caller's own IAM policy is
// too narrow to list it" — and silently reading that ambiguous case as
// "someone else owns this" would misreport a misconfigured policy as a
// foreign bucket, which is exactly the failure mode this method must not
// introduce.
//
// ListBuckets sidesteps the ambiguity rather than resolving it: per AWS's
// own reference, it "[r]eturns a list of all buckets owned by the
// authenticated sender of the request" — an account-scoped listing, not a
// per-bucket permission check, so it can never be fooled by a bucket-level
// policy (this account's own artifact bucket denying HeadBucket to an
// over-narrow caller would still appear in this account's own
// ListBuckets) and never needs to guess at a foreign account's ownership
// (a bucket this account does not own can never appear in this account's
// own bucket list, full stop — there is no cross-account visibility to be
// ambiguous about). The two questions this workstream's brief asks Get to
// keep separate — "does this bucket exist at all" and "do we own it" —
// are answered by two different AWS APIs precisely so that a bucket-level
// permission problem on our own bucket can never be misread as a
// foreign-ownership one: Cloud Control's GetResource (existence, globally,
// no ownership check) stays the existence check exactly as before, and
// this method alone decides ownership. If ListBuckets itself fails — most
// plausibly because the caller's IAM lacks the account-wide
// s3:ListAllMyBuckets action this operation requires — that is a real,
// account-level authorization problem unrelated to any specific bucket's
// ownership, and is returned as an error here rather than folded into
// either a true or false ownership answer; see this method's callers in
// artifactbucket.go for why an error here must never be read as "not
// owned."
//
// Filtered by Prefix rather than paged through the account's entire bucket
// list: AWS's own documentation now warns that an unpaginated ListBuckets
// call is rejected outright once an account's bucket quota exceeds 10,000,
// so narrowing server-side to candidates that could possibly match bucket's
// exact name is both cheaper and the only form of this call safe to rely
// on regardless of account size. Prefix is a "begins with" filter, not an
// exact match, so the loop below still checks each candidate's Name for
// equality rather than trusting a single result.
//
// # AccountID is not needed here
//
// Every other ARN-constructing method in this package (see AccountID's own
// doc comment) needs this account's id as a literal value to embed in a
// constructed ARN. This method needs no such value: ListBuckets is
// inherently scoped to "whatever account these credentials authenticate
// as," so the question "is this ours" never has to compare an id at all —
// another way the account-scoped API sidesteps the ambiguity a bare id
// comparison via ExpectedBucketOwner could not.
func (c *Client) OwnsBucket(ctx context.Context, bucket string) (bool, error) {
	var token *string
	for range maxOwnsBucketPages {
		out, err := c.s3.ListBuckets(ctx, &s3.ListBucketsInput{
			Prefix:            aws.String(bucket),
			ContinuationToken: token,
		})
		if err != nil {
			return false, kerrors.Wrap(err, kerrors.CodeUnexpected,
				"listing this account's own S3 buckets to check ownership of %q", bucket)
		}
		for _, b := range out.Buckets {
			if b.Name != nil && *b.Name == bucket {
				return true, nil
			}
		}
		if out.ContinuationToken == nil || *out.ContinuationToken == "" {
			return false, nil
		}
		token = out.ContinuationToken
	}
	return false, kerrors.Validation("checking ownership of bucket %q did not terminate within %d pages", bucket, maxOwnsBucketPages)
}

// bucketPolicyAbsent reports whether err is S3's way of saying a bucket
// policy — or the bucket itself — is not there: NoSuchBucketPolicy and
// NoSuchBucket, the two codes GetBucketPolicy and DeleteBucketPolicy must
// both treat as absence rather than failure. S3 does not model
// NoSuchBucketPolicy as its own Go error type the way NoSuchBucket
// (s3types.NoSuchBucket) is; both surface through the generic
// smithy.APIError interface that every typed S3 error also implements, so
// one ErrorCode check recognizes either.
func bucketPolicyAbsent(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "NoSuchBucketPolicy", "NoSuchBucket":
		return true
	default:
		return false
	}
}

// PutBucketPolicy replaces bucket's entire policy with document.
//
// Nothing else in this package writes a policy on an objects bucket —
// cloudfront.go's composite distribution resource is the only caller,
// granting the CloudFront distribution fronting it read access to the
// objects it serves.
func (c *Client) PutBucketPolicy(ctx context.Context, bucket, document string) error {
	_, err := c.s3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucket),
		Policy: aws.String(document),
	})
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "putting bucket policy on %q", bucket)
	}
	return nil
}

// GetBucketPolicy returns bucket's current policy document, or found=false
// when the bucket has no policy at all or does not exist — both read as
// absence here, per the same absence-versus-failure discipline as
// GetResource and OwnsBucket. Every other error is real and returned
// wrapped, never folded into an absent result.
func (c *Client) GetBucketPolicy(ctx context.Context, bucket string) (string, bool, error) {
	out, err := c.s3.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(bucket)})
	if err != nil {
		if bucketPolicyAbsent(err) {
			return "", false, nil
		}
		return "", false, kerrors.Wrap(err, kerrors.CodeUnexpected, "getting bucket policy for %q", bucket)
	}
	if out.Policy == nil {
		return "", false, nil
	}
	return *out.Policy, true, nil
}

// DeleteBucketPolicy removes bucket's policy, tolerating one already
// absent or a bucket already gone — the same already-absent-is-success
// contract as Client.DeleteResource.
func (c *Client) DeleteBucketPolicy(ctx context.Context, bucket string) error {
	_, err := c.s3.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(bucket)})
	if err != nil && !bucketPolicyAbsent(err) {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting bucket policy for %q", bucket)
	}
	return nil
}

// AccountID returns the AWS account id the configured credentials
// authenticate as, fetched once via STS GetCallerIdentity and cached for
// the process's lifetime (see the Client.accountID field doc for why
// caching, and why only on success).
//
// # Why this exists: ARN construction without a live cross-resource lookup
//
// A Lambda's execution Role property and an EventBridge Rule's Target Arn
// both require a full ARN, not a bare name — unlike AWS::Lambda::Url's
// TargetFunctionArn, which documents bare-name acceptance. The role and the
// function it assumes into are now a real DependsOn edge (register.go:
// TypeLambdaFunction depends on TypeIAMRole), so a live GetResource lookup
// of the role's Arn attribute would be safe today. The function and
// EventBridge Rule are not ordered against each other at all — Rule
// declares no DependsOn on the function (see eventsrule.go's own doc
// comment) because PutTargets never validates the target's existence, so a
// live lookup here would still race. Constructing every ARN this package
// needs locally, from the account id plus the region plus the resource's
// own derived name, removes the dependency entirely rather than papering
// over a race with a retry — and does so uniformly, rather than requiring
// every call site to know which of its cross-resource references the
// dependency graph has already made safe and which it has not.
//
// Assumes the "aws" partition. kraai's stated first deployment target is
// commercial AWS, used to run kraai.dev's own infrastructure; GovCloud/China
// partitions, whose ARNs use "aws-us-gov"/"aws-cn", are not something this
// workstream verified against and are out of scope here.
func (c *Client) AccountID(ctx context.Context) (string, error) {
	c.accountMu.Lock()
	defer c.accountMu.Unlock()
	if c.accountLoaded {
		return c.accountID, nil
	}

	out, err := c.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "resolving the AWS account id via STS")
	}
	if out.Account == nil || *out.Account == "" {
		return "", kerrors.Validation("STS GetCallerIdentity returned no account id")
	}

	c.accountID = *out.Account
	c.accountLoaded = true
	return c.accountID, nil
}

// SecretValue returns the string value of the secret at arn.
//
// Only the value is returned, never logged or kept: this is the producer
// behind an Aurora cluster's credential (resource.Secret), called at the
// moment a function's environment is built and nowhere else.
func (c *Client) SecretValue(ctx context.Context, arn string) (string, error) {
	out, err := c.sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
	if err != nil {
		// The error names the secret's ARN at most, never its value.
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "reading the secret at %s", arn)
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return "", kerrors.Validation("the secret at %s has no string value", arn)
	}
	return *out.SecretString, nil
}

// Permissions returns every IAM action this type's handlers and tag
// handling declare, across all verbs, sorted and without duplicates. A
// verb kraai never calls on a type still contributes: the policy this
// feeds covers plan, apply and destroy alike, and the difference is a
// handful of actions not worth a second policy.
func (s Schema) Permissions() []string {
	set := map[string]bool{}
	for _, raw := range s.Handlers {
		var handler struct {
			Permissions []string `json:"permissions"`
		}
		if err := json.Unmarshal(raw, &handler); err != nil {
			continue
		}
		for _, action := range handler.Permissions {
			set[action] = true
		}
	}
	for _, action := range s.Tagging.Permissions {
		set[action] = true
	}
	return sortedActions(set)
}

func sortedActions(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for action := range set {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}
