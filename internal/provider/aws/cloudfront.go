package aws

import (
	"context"
	"reflect"
	"sort"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

const (
	// cloudFrontOriginID is the one origin a kraai distribution has, named
	// for what it is rather than for the bucket, so TargetOriginId never
	// changes when the origin does.
	cloudFrontOriginID = "origin"
	// cloudFrontCachingOptimizedPolicyID is AWS's managed CachingOptimized
	// cache policy, the same id in every account and region.
	cloudFrontCachingOptimizedPolicyID = "658327ea-f89d-4fab-a63d-7e88639e58f6"
	// cloudFrontHostedZoneID is the hosted zone every distribution's domain
	// lives in, fixed by AWS; an alias record names it as the target's zone.
	cloudFrontHostedZoneID = "Z2FDTNDATAQYW2"
	cloudFrontMinimumTLS   = "TLSv1.2_2021"

	// bucketGrantSid names the one statement this package's bucket policy
	// writes.
	bucketGrantSid = "AllowCloudFrontServicePrincipalReadOnly"

	// oacIDConfigKey carries the OriginAccessControl id Create and Update
	// resolved into translate, through a copy of spec.Config. One
	// cloudFrontResource serves every cdn binding concurrently, so nothing
	// about one call can live on the struct. translate strips it before
	// building the desired state.
	oacIDConfigKey = "kraai:originAccessControlId"
)

// cloudFrontResource provisions a distribution in front of the objects
// bucket its entry names as origin, presenting the certificate its entry
// names, answering on its aliases, plus the OriginAccessControl and bucket
// policy that let it read a private bucket. A composite: the distribution
// (inner), its OAC (oac, created before and deleted after) and the bucket
// policy have one lifecycle. client reaches the S3 policy verbs, AccountID
// and OwnsBucket, none of which are in ccAPI.
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
// named name. OAC names are unique per account, so a retry after a Create
// that crashed before learning its id finds the existing one rather than
// creating a second.
func originAccessControlMatchByName(properties map[string]any, name string) bool {
	config, _ := properties["OriginAccessControlConfig"].(map[string]any)
	got, _ := config["Name"].(string)
	return got == name
}

// distributionShape is the projection of a DistributionConfig kraai sets
// and compares: what the manifest decides, and nothing CloudFront defaults.
// originAccessControl and granted are always wanted true, and
// shapeFromState reports what the live distribution and bucket policy show,
// so a missing OAC reference or an ungranted policy diffs as Mutable and is
// repaired by Update.
type distributionShape struct {
	origin              string
	bucket              string
	certificate         string
	aliases             []string
	enabled             bool
	originAccessControl bool
	granted             bool
}

// shapeFromSpec reads the entry and the attributes its references
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
// granted comes from the attribute Get records after checking the bucket's
// live policy; it is not part of DistributionConfig.
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

// firstOrigin returns state's one origin, or nil when it carries none. A
// distribution kraai created always has exactly one, but a live read is
// never trusted to echo a shape this package just built.
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
// S3 endpoint, "my-bucket.s3.us-east-1.amazonaws.com" to "my-bucket". Sound
// because every bucket name kraai derives is restricted to [a-z0-9-].
func bucketFromOriginDomain(domain string) string {
	if i := strings.Index(domain, "."); i >= 0 {
		return domain[:i]
	}
	return domain
}

// translate builds the distribution: one S3 origin, one cache behaviour on
// the managed CachingOptimized policy, HTTPS enforced, HTTP/2, and the
// certificate for its aliases when it has any, CloudFront's default
// certificate otherwise. The OAC id travels in under oacIDConfigKey and is
// stripped before shapeFromSpec runs, so it is never read as manifest
// input.
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
// returns and the desired state never carried. Nothing here is createOnly
// and the type has an update handler, so any difference is Mutable,
// including a missing OAC reference or an ungranted bucket policy.
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

// kraaiOriginGrantedAttribute is the key Get records under State.Attributes
// reporting whether the origin bucket's live policy grants this
// distribution read access. Spelled so it can never collide with a
// CloudFront property, which are all PascalCase with no separator.
const kraaiOriginGrantedAttribute = "kraai.OriginGranted"

// Get reports the distribution's live state, plus whether its origin
// bucket's policy currently grants it read access. Without that, a Create
// whose distribution succeeded but whose policy Put failed would read as
// healthy on the next plan and the grant would never be retried. One extra
// GetBucketPolicy per plan of a cdn binding.
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

// Create provisions the OAC before the distribution, whose origin must name
// the OAC's id, then grants the new distribution read access to its bucket.
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

// Update reconciles the distribution to spec, then re-grants or moves the
// bucket policy. The OAC id comes from the live origin when it has one; a
// distribution created without one gets one made here. If the origin bucket
// changed, the old bucket's policy is removed before the new one is granted.
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

// Delete removes the distribution, then the OAC it referenced (Cloud
// Control refuses to delete one still referenced), then the origin bucket's
// policy. The live state is read first because it is the only place the
// OAC id and origin bucket are still available; Delete carries only a Ref.
// Each step tolerates its own absence, and the policy is skipped when the
// bucket is not this account's.
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
