package aws

import (
	"context"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/evatt-labs/kraai/internal/httpx"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
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
// cluster or an aws-secretsmanager reference at the moment a function needs
// it, and DescribeSecret, to read which version currently carries a staging
// label without decrypting anything — the metadata-only call
// environmentMatches and resolveEnvSecret's marker both use.
type secretsManagerAPI interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	DescribeSecret(ctx context.Context, params *secretsmanager.DescribeSecretInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
}

// ssmAPI is the subset of *ssm.Client this package calls: GetParameter, to
// resolve an aws-ssm secret reference, always with decryption so a
// SecureString parameter's plaintext is what a caller gets, and the calls
// secrets.go uses to manage a secrets binding's own parameters without ever
// reading or writing a value outside Create: DescribeParameters and
// ListTagsForResource for identity and metadata, PutParameter to create,
// AddTagsToResource to reconcile a tag in place, and DeleteParameter to tear
// down.
type ssmAPI interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	DescribeParameters(ctx context.Context, params *ssm.DescribeParametersInput, optFns ...func(*ssm.Options)) (*ssm.DescribeParametersOutput, error)
	ListTagsForResource(ctx context.Context, params *ssm.ListTagsForResourceInput, optFns ...func(*ssm.Options)) (*ssm.ListTagsForResourceOutput, error)
	PutParameter(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	AddTagsToResource(ctx context.Context, params *ssm.AddTagsToResourceInput, optFns ...func(*ssm.Options)) (*ssm.AddTagsToResourceOutput, error)
	DeleteParameter(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
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
	ssm ssmAPI

	tagging taggingAPI

	// direct reads and lists through each service's own API, sharing the
	// SDK's instrumented transport and credentials.
	direct *direct.Client

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

	// created is every type this client has created an instance of, for
	// Created.
	created sync.Map

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

// WithSSMAPI substitutes the Systems Manager Parameter Store client, for
// tests.
func WithSSMAPI(api ssmAPI) Option {
	return func(c *Client) { c.ssm = api }
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
	httpClient := httpx.NewClient(60*time.Second, nil, nil)
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(settings.Region),
		awsconfig.WithHTTPClient(httpClient),
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
		ssm:              ssm.NewFromConfig(cfg),
		tagging:          resourcegroupstaggingapi.NewFromConfig(cfg),
		direct:           &direct.Client{HTTP: httpClient, Credentials: cfg.Credentials, Region: cfg.Region},
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

// Created reports whether this client has asked Cloud Control to create an
// instance of typeName: one a lagging index could not yet show.
func (c *Client) Created(typeName string) bool {
	_, ok := c.created.Load(typeName)
	return ok
}

// Region returns the AWS region this Client resolved at construction.
func (c *Client) Region() string { return c.region }
