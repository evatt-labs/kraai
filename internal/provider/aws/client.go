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
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
)

// maxListPages bounds a ListResources page walk against a NextToken that
// never stops advancing.
const maxListPages = 100

// maxEmptyBucketPages bounds EmptyBucket's page walk over ListObjectsV2.
const maxEmptyBucketPages = 100

// maxOwnsBucketPages bounds OwnsBucket's page walk over ListBuckets. Its
// Prefix filter narrows a real account's response to a handful of buckets,
// so this exists only so a broken pagination contract cannot spin forever.
const maxOwnsBucketPages = 100

// Default polling bounds for asynchronous Cloud Control operations. A
// CloudFront distribution's propagation alone can run past fifteen minutes,
// while an S3 bucket create finishes in under a second, so the backoff
// starts small and grows. WithPollTimings overrides all three.
const (
	defaultPollInitialDelay = 2 * time.Second
	defaultPollMaxDelay     = 30 * time.Second
	defaultPollTimeout      = 40 * time.Minute
)

// cloudControlAPI is the subset of *cloudcontrol.Client this package calls,
// shaped like the SDK's own signatures because the thing on the other side
// of this seam is the SDK client, which has no interface of its own.
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

// s3API is the subset of *s3.Client this package calls. Cloud Control
// manages a bucket's existence but has no notion of the objects in it, a
// bucket's policy, or which buckets an account owns, so this is the one
// place the package reaches past Cloud Control to a service SDK.
type s3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	// GetObject, DeleteObject, HeadBucket, CreateBucket and
	// PutPublicAccessBlock serve the environment lock and status record
	// (lockstore.go).
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(ctx context.Context, params *s3.CreateBucketInput, optFns ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	PutPublicAccessBlock(ctx context.Context, params *s3.PutPublicAccessBlockInput, optFns ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error)
	// ListObjectsV2 and DeleteObjects serve EmptyBucket. This package never
	// enables versioning on a bucket it creates, so ListObjectVersions is
	// not needed.
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	// ListBuckets serves OwnsBucket: the one call here that is
	// account-scoped rather than bucket-scoped.
	ListBuckets(ctx context.Context, params *s3.ListBucketsInput, optFns ...func(*s3.Options)) (*s3.ListBucketsOutput, error)
	// PutBucketPolicy, GetBucketPolicy and DeleteBucketPolicy serve
	// cloudfront.go, which grants a distribution read access to the bucket
	// it fronts.
	PutBucketPolicy(ctx context.Context, params *s3.PutBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error)
	GetBucketPolicy(ctx context.Context, params *s3.GetBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error)
	DeleteBucketPolicy(ctx context.Context, params *s3.DeleteBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketPolicyOutput, error)
}

// stsAPI is the subset of *sts.Client this package calls: GetCallerIdentity,
// to resolve the account id an ARN needs.
type stsAPI interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// secretsManagerAPI is the subset of *secretsmanager.Client this package
// calls: GetSecretValue, to read the credential RDS manages for an Aurora
// cluster at the moment a function needs it.
type secretsManagerAPI interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// Client is a thin Cloud Control and CloudFormation client whose exported
// methods speak this package's vocabulary (a decoded properties map, an
// identifier list) rather than the SDK's, so *Client satisfies ccAPI and
// nothing above this file needs to know the SDK exists.
type Client struct {
	cc  cloudControlAPI
	cf  cloudFormationAPI
	s3  s3API
	sts stsAPI
	sm  secretsManagerAPI

	// region is the SDK's resolved region, which every ARN this package
	// builds is against. Taken from the loaded config rather than
	// Settings.Region, which may be empty and deferred to the SDK's chain.
	region string

	// Polling bounds for the async verbs. Defaulted in New, overridable via
	// WithPollTimings.
	pollInitialDelay time.Duration
	pollMaxDelay     time.Duration
	pollTimeout      time.Duration

	// The caller's account id, fetched once and cached only on success, so
	// one throttled STS call is not a permanent failure.
	accountMu     sync.Mutex
	accountID     string
	accountLoaded bool

	// reads memoizes what Cloud Control has already answered this process,
	// so the lookups that sweep one type (every by-tag registration lists
	// a type and reads each instance) cost one request per resource rather
	// than one per lookup. Forgotten per type on any mutation of it.
	reads readCache

	// schemaCacheDir, when set, holds fetched resource provider schemas on
	// disk between runs, for schemaCacheTTL; see schemacache.go.
	schemaCacheDir string
	schemaCacheTTL time.Duration
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

// WithS3API substitutes the S3 caller.
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
// asynchronous operation to a terminal state, so tests can exercise polling
// in milliseconds.
func WithPollTimings(initialDelay, maxDelay, timeout time.Duration) Option {
	return func(c *Client) {
		c.pollInitialDelay = initialDelay
		c.pollMaxDelay = maxDelay
		c.pollTimeout = timeout
	}
}

// New builds a Client for settings.Region, authenticating through the SDK's
// own default credential chain. kraai never handles an AWS credential
// itself.
func New(ctx context.Context, settings Settings, opts ...Option) (*Client, error) {
	// One shared, instrumented HTTP transport across every provider rather
	// than a pool per SDK service; WithHTTPClient substitutes only the
	// transport under the SDK's retry and credential layers. Adaptive
	// retries with a longer budget, because Cloud Control throttles a
	// plan's burst of reads well before three standard attempts are spent.
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(settings.Region),
		awsconfig.WithHTTPClient(httpx.NewClient(60*time.Second, nil, nil)),
		awsconfig.WithRetryMode(aws.RetryModeAdaptive),
		awsconfig.WithRetryMaxAttempts(cloudControlMaxAttempts),
	)
	if err != nil {
		// The error can name a credential file path but never a value.
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
// ResourceNotFoundException is the only outcome translated to found=false
// with a nil error. Every other error is real and must not be read as
// absence: teardown treats "does not exist" as "already deleted, keep
// going", so misreading an outage as absence would orphan a resource.
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
// typeName Cloud Control can see, across all pages. Identifiers only, never
// properties: Cloud Control guarantees only the identifier per listed
// resource, and a lookup must not trust a field List never promised.
//
// resourceModel is nil for the usual unscoped list handler. A parent-scoped
// handler requires a ResourceModel naming the parent, and reports a parent
// that does not exist as ResourceNotFoundException rather than an empty
// list. That is translated to an empty list here, and only for a scoped
// call: a permission's parent function not existing yet means the
// permission does not exist yet, which is the absence plan needs to report
// a create rather than a failure.
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
// the defaults for a Client built by a struct literal, as tests do, so the
// poll loop never busy-spins on a zero delay.
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
// OperationStatus, or until the status call fails, ctx is cancelled, or the
// poll timeout elapses. It returns the raw terminal event rather than
// deciding whether FAILED is an error: Delete treats NotFound as success
// and Create does not, and each verb keeps that decision.
//
// Backoff starts at initialDelay and doubles up to maxDelay, honouring Cloud
// Control's RetryAfter hint when it is later. Every iteration either
// returns or waits.
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
// something the API will never accept as is: a validation problem the
// manifest can fix. Everything else is CodeUnexpected.
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

// translateFailure maps a terminal FAILED or CANCEL_COMPLETE event onto a
// kerrors bucket, naming the resource, the status and the handler's code.
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

// decodeResourceModel decodes a terminal ProgressEvent's ResourceModel into
// a properties map. Cloud Control promises the final model on SUCCESS, so no
// follow-up GetResource is needed.
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

// UpdateResource submits patch, an RFC 6902 JSON Patch document, against
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
// state. Deleting something already absent is success, both when Cloud
// Control rejects the delete with ResourceNotFoundException and when the
// async handler reports a terminal FAILED with NotFound.
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

// DescribeType fetches and decodes typeName's CloudFormation resource
// provider schema. Fetched on first use and cached per type per process by
// resourceType.getSchema, and across runs by the on-disk schema cache when
// one is configured.
func (c *Client) DescribeType(ctx context.Context, typeName string) (cfschema.Facts, error) {
	raw, err := c.schemaDocument(ctx, typeName)
	if err != nil {
		return cfschema.Facts{}, err
	}
	doc, err := cfschema.Parse(raw)
	if err != nil {
		return cfschema.Facts{}, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding schema for %s", typeName)
	}
	if doc.TypeName == "" {
		doc.TypeName = typeName
	}
	return cfschema.Derive(doc), nil
}

// fetchSchema asks CloudFormation for typeName's schema document.
func (c *Client) fetchSchema(ctx context.Context, typeName string) ([]byte, error) {
	out, err := c.cf.DescribeType(ctx, &cloudformation.DescribeTypeInput{
		Type:     cftypes.RegistryTypeResource,
		TypeName: aws.String(typeName),
	})
	if err != nil {
		var notFound *cftypes.TypeNotFoundException
		if errors.As(err, &notFound) {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation, "no CloudFormation resource provider schema is registered for %q", typeName)
		}
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "describing type %s", typeName)
	}
	if out.Schema == nil {
		return nil, kerrors.Validation("DescribeType for %s returned no schema", typeName)
	}
	return []byte(*out.Schema), nil
}

// Region returns the AWS region this Client resolved at construction.
func (c *Client) Region() string { return c.region }

// PutObject uploads body to bucket/key, replacing any existing object. No
// existence check first: PutObject is itself an overwrite, and the artifact
// key is a content hash, so a repeat write for unchanged input writes back
// bytes S3 already holds.
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

// EmptyBucket deletes every object in bucket, so a subsequent bucket delete
// through Cloud Control can succeed; S3 refuses to delete a non-empty
// bucket, and a destroy once left every artifact bucket behind.
//
// Each ListObjectsV2 page is deleted as its own DeleteObjects batch, since
// both are capped at 1000 keys. No version or delete-marker handling: this
// package never enables versioning on a bucket it creates, so current
// objects are everything such a bucket can hold. A NoSuchBucket from the
// listing means the bucket is already gone, which is success; every other
// failure, including a per-object error inside a successful DeleteObjects
// response, is returned naming the bucket.
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
			// A DeleteObjects call can succeed overall while individual
			// keys failed; S3 reports those per object. The first is enough
			// to act on.
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
// Bucket names are global, and Cloud Control's GetResource for a bucket
// resolves that namespace without checking ownership, so a stranger's
// bucket that collides with a derived name would read as "exists, no
// change" and then be written into or deleted. HeadBucket cannot answer
// this: with or without ExpectedBucketOwner it returns the same 403 for a
// foreign bucket as for one of ours the caller's IAM cannot see, and AWS
// documents that ambiguity as intentional. ListBuckets returns only the
// buckets the authenticated account owns, so it can never be fooled by a
// bucket policy and never has to compare an account id. A failure of the
// call itself, most likely a missing s3:ListAllMyBuckets, is returned as an
// error and must never be read as "not owned".
//
// Filtered by Prefix: an unpaginated ListBuckets is rejected once an
// account exceeds 10,000 buckets. Prefix is "begins with", so each
// candidate's name is still checked for equality.
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

// bucketPolicyAbsent reports whether err is S3 saying a bucket policy, or
// the bucket itself, is not there. NoSuchBucketPolicy has no typed Go error
// the way NoSuchBucket does, so one ErrorCode check recognizes either.
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
// when the bucket has no policy or does not exist. Every other error is
// returned, never folded into absence.
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

// DeleteBucketPolicy removes bucket's policy, tolerating one already absent
// or a bucket already gone.
func (c *Client) DeleteBucketPolicy(ctx context.Context, bucket string) error {
	_, err := c.s3.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(bucket)})
	if err != nil && !bucketPolicyAbsent(err) {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting bucket policy for %q", bucket)
	}
	return nil
}

// AccountID returns the AWS account id the configured credentials
// authenticate as, fetched once via STS and cached for the process.
//
// A Lambda's Role and an EventBridge Rule's Target need full ARNs. Building
// every ARN locally from the account id, the region and the resource's
// derived name removes any live cross-resource lookup, and with it any
// ordering the lookup would need. Assumes the "aws" partition; GovCloud and
// China are not verified.
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

// SecretValue returns the string value of the secret at arn. Only the value
// is returned, never logged or kept: this is the producer behind an Aurora
// cluster's credential, called at the moment a function's environment is
// built.
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

func sortedActions(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for action := range set {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}
