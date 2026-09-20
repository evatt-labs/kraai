package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

const (
	// cloudFrontOriginID is the one origin a kraai distribution has, named
	// for what it is rather than for the bucket, so a cache behaviour's
	// TargetOriginId never has to change when the origin does.
	cloudFrontOriginID = "origin"
	// cloudFrontCachingOptimizedPolicyID is AWS's managed CachingOptimized
	// cache policy, the recommended one for an S3 origin. A policy id is
	// required on a cache behaviour now that ForwardedValues is legacy, and
	// this one is the same in every account and region.
	cloudFrontCachingOptimizedPolicyID = "658327ea-f89d-4fab-a63d-7e88639e58f6"
	// cloudFrontHostedZoneID is the hosted zone every CloudFront
	// distribution's domain lives in, fixed by AWS; a Route 53 alias record
	// pointing at a distribution names it as the target's zone.
	cloudFrontHostedZoneID = "Z2FDTNDATAQYW2"
	cloudFrontMinimumTLS   = "TLSv1.2_2021"

	// bucketGrantSid names the one statement this package's bucket policy
	// ever writes, for a reader identifying it in a live policy document.
	bucketGrantSid = "AllowCloudFrontServicePrincipalReadOnly"

	// oacIDConfigKey carries the OriginAccessControl id Create/Update
	// resolved into translate, through a copy of spec.Config rather than a
	// field on cloudFrontResource: one cloudFrontResource instance serves
	// every cdn binding, and two bindings can be created concurrently, so
	// nothing about a single Create/Update call can live on the struct
	// itself. translate reads and strips this key before building the
	// distribution's real desired state.
	oacIDConfigKey = "kraai:originAccessControlId"
)

// cloudFrontResource provisions a distribution in front of the objects
// bucket its entry names as origin, presenting the certificate its entry
// names, answering on its aliases — and, because that bucket is private by
// default, the OriginAccessControl and bucket policy that let the
// distribution actually read it (evatt-labs/kraai#219).
//
// A composite, not a bare resourceType: the distribution, its
// OriginAccessControl and the origin bucket's policy are three Cloud
// Control-adjacent operations with one lifecycle, the same shape
// artifactBucketResource already established for a bucket's own object
// data plane. inner is the distribution's own engine; oac is a second engine
// instance for the OriginAccessControl, created before the distribution and
// deleted after it; client reaches the S3 bucket-policy verbs and
// AccountID/OwnsBucket, none of which are part of ccAPI (see
// artifactbucket.go's own doc comment for the identical reason it keeps a
// concrete *Client beside its engine).
type cloudFrontResource struct {
	inner  *resourceType
	oac    *resourceType
	client *Client
}

func newCloudFrontResource(client *Client) *cloudFrontResource {
	c := &cloudFrontResource{client: client}
	c.inner = &resourceType{
		provider: Provider, typeName: TypeCloudFrontDistribution,
		lookup: resource.LookupByTag, client: client,
		match: arrayTagsMatch, stampTag: arrayTagsStampTag,
		translate: func(_ context.Context, spec resource.Spec) (resource.Spec, error) { return c.translate(spec) },
	}
	c.oac = &resourceType{
		provider: Provider, typeName: TypeCloudFrontOriginAccessControl,
		lookup: resource.LookupByAttr, client: client,
		match: originAccessControlMatchByName,
	}
	return c
}

// originAccessControlMatchByName reports whether properties is the OAC
// named name. OriginAccessControl names are unique per account, so a retry
// after a Create that succeeded but crashed before this type learned its id
// must find the existing one by name rather than create a second one this
// package would then have no way to tell apart from the first.
func originAccessControlMatchByName(properties map[string]any, name string) bool {
	config, _ := properties["OriginAccessControlConfig"].(map[string]any)
	got, _ := config["Name"].(string)
	return got == name
}

// distributionShape is the projection of a DistributionConfig kraai sets and
// compares: what the manifest decides, and nothing CloudFront defaults.
// originAccessControl and granted are not manifest input — shapeFromSpec
// always wants both true, and shapeFromState reports what a live
// distribution and its origin bucket's policy actually show — so a missing
// OAC reference or an ungranted bucket policy diffs the same as any other
// drift: Mutable, repaired by Update.
type distributionShape struct {
	origin              string
	bucket              string
	certificate         string
	aliases             []string
	enabled             bool
	originAccessControl bool
	granted             bool
}

// shapeFromSpec reads the entry and the attributes the references it names
// published: the origin bucket's regional endpoint and name, the
// certificate's ARN.
func (c *cloudFrontResource) shapeFromSpec(spec resource.Spec) (distributionShape, error) {
	origin, _ := spec.Config["origin"].(string)
	if origin == "" {
		return distributionShape{}, kerrors.Validation(
			"cdn binding %q names no origin — the objects binding it fronts", spec.Binding)
	}
	// The regional endpoint, not DomainName: the global one redirects for a
	// while after a bucket is created outside us-east-1, and a CloudFront
	// origin does not follow redirects.
	originDomain, err := referencedAttribute(spec, origin, TypeS3Bucket, "RegionalDomainName")
	if err != nil {
		return distributionShape{}, kerrors.Wrap(err, kerrors.CodeValidation,
			"resolving the origin bucket for cdn binding %q", spec.Binding)
	}
	bucketName, err := referencedAttribute(spec, origin, TypeS3Bucket, "BucketName")
	if err != nil {
		return distributionShape{}, kerrors.Wrap(err, kerrors.CodeValidation,
			"resolving the origin bucket name for cdn binding %q", spec.Binding)
	}

	shape := distributionShape{
		origin: originDomain, bucket: bucketName, enabled: true,
		originAccessControl: true, granted: true,
	}
	if certificate, _ := spec.Config["certificate"].(string); certificate != "" {
		arn, err := referencedAttribute(spec, certificate, TypeCertificateManagerCertificate, certificateArnAttribute)
		if err != nil {
			return distributionShape{}, kerrors.Wrap(err, kerrors.CodeValidation,
				"resolving the certificate for cdn binding %q", spec.Binding)
		}
		shape.certificate = arn
	}
	if raw, ok := spec.Config["aliases"].([]any); ok {
		for _, a := range raw {
			if s, ok := a.(string); ok && s != "" {
				shape.aliases = append(shape.aliases, s)
			}
		}
		sort.Strings(shape.aliases)
	}
	if len(shape.aliases) > 0 && shape.certificate == "" {
		// CloudFront refuses this too, but far from the manifest line that
		// caused it.
		return distributionShape{}, kerrors.Validation(
			"cdn binding %q answers on aliases %v but names no certificate to present for them",
			spec.Binding, shape.aliases)
	}
	return shape, nil
}

// shapeFromState reads the same projection back out of a live distribution.
// originAccessControl is read off the one origin's OriginAccessControlId;
// granted is read off the private attribute Get (below) records after
// checking the origin bucket's live policy — it is not itself part of
// DistributionConfig.
func shapeFromState(state *resource.State) distributionShape {
	var shape distributionShape
	config, _ := state.Attributes["DistributionConfig"].(map[string]any)
	shape.enabled, _ = config["Enabled"].(bool)
	if origin := firstOrigin(state); origin != nil {
		shape.origin, _ = origin["DomainName"].(string)
		shape.bucket = bucketFromOriginDomain(shape.origin)
		if oacID, _ := origin["OriginAccessControlId"].(string); oacID != "" {
			shape.originAccessControl = true
		}
	}
	if viewer, ok := config["ViewerCertificate"].(map[string]any); ok {
		shape.certificate, _ = viewer["AcmCertificateArn"].(string)
	}
	if aliases, ok := config["Aliases"].([]any); ok {
		for _, a := range aliases {
			if s, ok := a.(string); ok {
				shape.aliases = append(shape.aliases, s)
			}
		}
		sort.Strings(shape.aliases)
	}
	shape.granted, _ = state.Attributes[kraaiOriginGrantedAttribute].(bool)
	return shape
}

// firstOrigin returns state's one origin as a map, or nil when state carries
// none — a distribution kraai created always has exactly one (translate
// below), but a live read is never trusted to echo back a shape this
// package did not itself just build.
func firstOrigin(state *resource.State) map[string]any {
	config, _ := state.Attributes["DistributionConfig"].(map[string]any)
	origins, _ := config["Origins"].([]any)
	if len(origins) == 0 {
		return nil
	}
	first, _ := origins[0].(map[string]any)
	return first
}

// bucketFromOriginDomain extracts the bucket name from an origin's regional
// S3 endpoint, e.g. "my-bucket.s3.us-east-1.amazonaws.com" -> "my-bucket".
// Sound because every bucket name kraai derives is restricted to [a-z0-9-]
// (naming.ServiceName, artifactBucketName's own doc comment) — a domain
// built from one can never contain a "." before the ".s3." segment that
// would make taking the first label ambiguous.
func bucketFromOriginDomain(domain string) string {
	if i := strings.Index(domain, "."); i >= 0 {
		return domain[:i]
	}
	return domain
}

// translate builds the distribution: one S3 origin, one cache behaviour
// on AWS's managed CachingOptimized policy, HTTPS enforced, HTTP/2, and the
// certificate for its aliases when it has any — CloudFront's own default
// certificate otherwise, which is only ever valid for its own hostname.
//
// The OriginAccessControl id Create/Update already resolved travels in
// under oacIDConfigKey (see that constant's own doc comment) rather than as
// a parameter: translateFunc's signature is shared by every type this
// package registers, and changing it for this one caller would ripple
// through resource.go. Stripped from spec.Config before shapeFromSpec runs,
// so it is never mistaken for manifest input.
func (c *cloudFrontResource) translate(spec resource.Spec) (resource.Spec, error) {
	oacID, _ := spec.Config[oacIDConfigKey].(string)
	if _, has := spec.Config[oacIDConfigKey]; has {
		stripped := make(map[string]any, len(spec.Config)-1)
		for k, v := range spec.Config {
			if k != oacIDConfigKey {
				stripped[k] = v
			}
		}
		spec.Config = stripped
	}

	shape, err := c.shapeFromSpec(spec)
	if err != nil {
		return resource.Spec{}, err
	}

	origin := map[string]any{
		"Id":             cloudFrontOriginID,
		"DomainName":     shape.origin,
		"S3OriginConfig": map[string]any{},
	}
	if oacID != "" {
		origin["OriginAccessControlId"] = oacID
	}

	config := map[string]any{
		"Enabled":           shape.enabled,
		"Comment":           spec.Name,
		"DefaultRootObject": "index.html",
		"HttpVersion":       "http2",
		"Origins":           []any{origin},
		"DefaultCacheBehavior": map[string]any{
			"TargetOriginId":       cloudFrontOriginID,
			"ViewerProtocolPolicy": "redirect-to-https",
			"CachePolicyId":        cloudFrontCachingOptimizedPolicyID,
		},
	}
	if shape.certificate != "" {
		config["ViewerCertificate"] = map[string]any{
			"AcmCertificateArn":      shape.certificate,
			"SslSupportMethod":       "sni-only",
			"MinimumProtocolVersion": cloudFrontMinimumTLS,
		}
	} else {
		config["ViewerCertificate"] = map[string]any{"CloudFrontDefaultCertificate": true}
	}
	if len(shape.aliases) > 0 {
		aliases := make([]any, 0, len(shape.aliases))
		for _, a := range shape.aliases {
			aliases = append(aliases, a)
		}
		config["Aliases"] = aliases
	}

	translated := spec
	translated.Config = map[string]any{"DistributionConfig": config}
	return translated, nil
}

// Diff compares the projection kraai decides, not the whole
// DistributionConfig: CloudFront fills the rest with defaults a read
// returns and the desired state never carried, and comparing those would
// report an update on every plan, forever. Nothing here is createOnly, and
// the type has an update handler, so any difference is Mutable — including
// a missing OriginAccessControl reference or an ungranted bucket policy
// (distributionShape's own doc comment), which is exactly the gap that
// closes evatt-labs/kraai#219: a distribution whose bucket policy Put
// failed after Create no longer reads as Same on the next plan.
func (c *cloudFrontResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	want, err := c.shapeFromSpec(spec)
	if err != nil {
		return resource.Same, err
	}
	if reflect.DeepEqual(want, shapeFromState(state)) {
		return resource.Same, nil
	}
	return resource.Mutable, nil
}

// kraaiOriginGrantedAttribute is the key Get (below) records under
// State.Attributes reporting whether the origin bucket's live policy
// currently grants this distribution read access. Chosen to read
// unambiguously as kraai's own, never a CloudFront property: every real
// CloudFormation property name in this file is PascalCase with no
// separator, and Cloud Control's ResourceModel decode (client.go) can only
// ever produce keys from CloudFront's own schema, so this can never collide
// with one.
const kraaiOriginGrantedAttribute = "kraai.OriginGranted"

// Get reports the distribution's live state, plus (under
// kraaiOriginGrantedAttribute) whether its origin bucket's policy currently
// grants it read access.
//
// # Why this exists
//
// Without it, a Create whose distribution succeeded but whose bucket-policy
// Put then failed would still read back as a normal, healthy distribution
// on the next plan: Get would report the distribution, Diff would compare
// only DistributionConfig, and the grant would never be retried. Recording
// the live grant here, and comparing it in Diff via shapeFromState/
// distributionShape.granted, turns that into an ordinary Mutable diff that
// Update repairs.
//
// One extra GetBucketPolicy call per plan of a cdn binding, in addition to
// the GetResource this method already made for the distribution itself.
func (c *cloudFrontResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	state, err := c.inner.Get(ctx, ref)
	if err != nil || state == nil {
		return state, err
	}

	granted, err := c.checkGrant(ctx, state)
	if err != nil {
		return nil, err
	}

	attrs := make(map[string]any, len(state.Attributes)+1)
	for k, v := range state.Attributes {
		attrs[k] = v
	}
	attrs[kraaiOriginGrantedAttribute] = granted
	state.Attributes = attrs
	return state, nil
}

// checkGrant reports whether the origin bucket named in state's live
// DistributionConfig currently carries a policy granting this exact
// distribution s3:GetObject. false, not an error, when the distribution has
// no recognizable origin yet — a state this package's own translate would
// never produce, but Get must not fail outright on a distribution this
// package did not create in the shape it expects.
func (c *cloudFrontResource) checkGrant(ctx context.Context, state *resource.State) (bool, error) {
	origin := firstOrigin(state)
	domain, _ := origin["DomainName"].(string)
	if domain == "" {
		return false, nil
	}
	bucket := bucketFromOriginDomain(domain)

	account, err := c.client.AccountID(ctx)
	if err != nil {
		return false, err
	}
	want, err := bucketPolicyDocument(bucket, distributionARN(account, state.ID))
	if err != nil {
		return false, err
	}

	live, found, err := c.client.GetBucketPolicy(ctx, bucket)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return policyDocumentsEqual(want, live)
}

// bucketPolicyStatement and bucketPolicyDocumentShape are marshalled, never
// hand-built as a string, so PutBucketPolicy and the test suite comparing
// against it can never drift from each other on formatting.
type bucketPolicyStatement struct {
	Sid       string                       `json:"Sid"`
	Effect    string                       `json:"Effect"`
	Principal map[string]string            `json:"Principal"`
	Action    string                       `json:"Action"`
	Resource  string                       `json:"Resource"`
	Condition map[string]map[string]string `json:"Condition"`
}

type bucketPolicyDocumentShape struct {
	Version   string                  `json:"Version"`
	Statement []bucketPolicyStatement `json:"Statement"`
}

// bucketPolicyDocument builds the policy document granting distARN's
// distribution s3:GetObject on bucket, conditioned on its own SourceArn so
// no other distribution's requests are covered by it.
func bucketPolicyDocument(bucket, distARN string) (string, error) {
	doc := bucketPolicyDocumentShape{
		Version: "2012-10-17",
		Statement: []bucketPolicyStatement{{
			Sid:       bucketGrantSid,
			Effect:    "Allow",
			Principal: map[string]string{"Service": "cloudfront.amazonaws.com"},
			Action:    "s3:GetObject",
			Resource:  fmt.Sprintf("arn:aws:s3:::%s/*", bucket),
			Condition: map[string]map[string]string{"StringEquals": {"AWS:SourceArn": distARN}},
		}},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding bucket policy for %q", bucket)
	}
	return string(body), nil
}

// policyDocumentsEqual compares two policy documents structurally rather
// than as strings: a live policy S3 echoes back may reorder keys or
// whitespace differently from what json.Marshal produced when this package
// wrote it, and a byte-for-byte comparison would report drift that is not
// there.
func policyDocumentsEqual(a, b string) (bool, error) {
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding desired bucket policy")
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding live bucket policy")
	}
	return reflect.DeepEqual(av, bv), nil
}

// specWithOACID copies spec.Config and adds oacID under oacIDConfigKey,
// without mutating the caller's own map — spec.Config may still be read
// elsewhere (a concurrent Diff over the same spec value).
func specWithOACID(spec resource.Spec, oacID string) resource.Spec {
	cfg := make(map[string]any, len(spec.Config)+1)
	for k, v := range spec.Config {
		cfg[k] = v
	}
	cfg[oacIDConfigKey] = oacID
	spec.Config = cfg
	return spec
}

// findOrCreateOAC returns the id of the OriginAccessControl named name,
// creating one if none exists yet. name is the distribution's own derived
// name: OAC names are unique per account, so this doubles as this
// resource's identity for the OAC the same way the distribution's own tag
// does for itself, and a retry after a Create that made the OAC but failed
// before the distribution existed finds the same one again instead of
// creating a second, orphaned OAC no later run could tell apart from the
// first.
func (c *cloudFrontResource) findOrCreateOAC(ctx context.Context, name string) (string, error) {
	ref := resource.Ref{Provider: Provider, Type: TypeCloudFrontOriginAccessControl, Name: name}
	existing, err := c.oac.Get(ctx, ref)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return existing.ID, nil
	}

	spec := resource.Spec{
		Binding: name,
		Name:    name,
		Config: map[string]any{
			"OriginAccessControlConfig": map[string]any{
				"Name":                          name,
				"OriginAccessControlOriginType": "s3",
				"SigningBehavior":               "always",
				"SigningProtocol":               "sigv4",
			},
		},
	}
	created, err := c.oac.Create(ctx, spec)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

// grant puts the bucket policy granting distributionID read access to
// bucket.
func (c *cloudFrontResource) grant(ctx context.Context, bucket, distributionID string) error {
	account, err := c.client.AccountID(ctx)
	if err != nil {
		return err
	}
	doc, err := bucketPolicyDocument(bucket, distributionARN(account, distributionID))
	if err != nil {
		return err
	}
	return c.client.PutBucketPolicy(ctx, bucket, doc)
}

// Create provisions the OriginAccessControl before the distribution — the
// distribution's origin must name the OAC's id, so the OAC has to exist
// first — then grants the new distribution read access to its origin
// bucket. Cloud Control cannot delete an OAC still referenced by a
// distribution, so this ordering (and Delete's mirror of it) is the only
// one either verb can use.
func (c *cloudFrontResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	shape, err := c.shapeFromSpec(spec)
	if err != nil {
		return nil, err
	}

	oacID, err := c.findOrCreateOAC(ctx, spec.Name)
	if err != nil {
		return nil, err
	}

	state, err := c.inner.Create(ctx, specWithOACID(spec, oacID))
	if err != nil {
		return nil, err
	}

	if err := c.grant(ctx, shape.bucket, state.ID); err != nil {
		return nil, err
	}
	return state, nil
}

// Update reconciles the distribution to spec, then re-grants (or moves) the
// bucket policy.
//
// The OriginAccessControl id comes from the live distribution's own origin
// when it has one — a distribution created before this workstream has
// none, and gets one made for it here rather than failing. If the origin
// bucket changed, the old bucket's policy is removed first (tolerating one
// already absent) before the new bucket is granted.
func (c *cloudFrontResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	shape, err := c.shapeFromSpec(spec)
	if err != nil {
		return nil, err
	}

	live, err := c.inner.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if live == nil {
		return nil, kerrors.Validation("cannot update %s %q: it does not currently exist", TypeCloudFrontDistribution, ref.Name)
	}

	liveOrigin := firstOrigin(live)
	oacID, _ := liveOrigin["OriginAccessControlId"].(string)
	if oacID == "" {
		oacID, err = c.findOrCreateOAC(ctx, spec.Name)
		if err != nil {
			return nil, err
		}
	}
	liveDomain, _ := liveOrigin["DomainName"].(string)
	oldBucket := bucketFromOriginDomain(liveDomain)

	state, err := c.inner.Update(ctx, ref, specWithOACID(spec, oacID))
	if err != nil {
		return nil, err
	}

	if oldBucket != "" && oldBucket != shape.bucket {
		if err := c.client.DeleteBucketPolicy(ctx, oldBucket); err != nil {
			return nil, err
		}
	}
	if err := c.grant(ctx, shape.bucket, state.ID); err != nil {
		return nil, err
	}
	return state, nil
}

// Delete removes the distribution, then the OriginAccessControl it
// referenced, then the origin bucket's policy — in that order because Cloud
// Control refuses to delete an OAC still referenced by a distribution, and
// because the distribution's own live state (read here, before deleting
// it) is the only place the OAC id and origin bucket are still available:
// Delete's own contract carries no spec, only a Ref.
//
// A distribution already absent is success, matching every other type's
// Delete. The OAC and the bucket policy are each deleted tolerating their
// own absence too, and the bucket policy is skipped entirely when
// OwnsBucket reports the origin bucket is not this account's — deleting a
// stranger's policy is never this package's to do.
func (c *cloudFrontResource) Delete(ctx context.Context, ref resource.Ref) error {
	live, err := c.inner.Get(ctx, ref)
	if err != nil {
		return err
	}
	if live == nil {
		return nil
	}

	origin := firstOrigin(live)
	oacID, _ := origin["OriginAccessControlId"].(string)
	domain, _ := origin["DomainName"].(string)

	if err := c.inner.Delete(ctx, ref); err != nil {
		return err
	}

	if oacID != "" {
		oacRef := resource.Ref{
			Provider: Provider, Type: TypeCloudFrontOriginAccessControl,
			Import: &resource.Import{ID: oacID},
		}
		if err := c.oac.Delete(ctx, oacRef); err != nil {
			return err
		}
	}

	if domain == "" {
		return nil
	}
	bucket := bucketFromOriginDomain(domain)
	owned, err := c.client.OwnsBucket(ctx, bucket)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return c.client.DeleteBucketPolicy(ctx, bucket)
}
