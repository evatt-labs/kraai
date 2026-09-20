package aws

import (
	"context"
	"reflect"
	"sort"

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
)

// cloudFrontResource provisions a distribution in front of the objects
// bucket its entry names as origin, presenting the certificate its entry
// names, answering on its aliases.
//
// LookupByTag, replacing a lookup by alias: a distribution serving only its
// own *.cloudfront.net hostname has no alias to be found by, and an alias
// is a value the manifest may change, which an identity must not be. The
// kraai tag is what every other tagged type uses, and it survives both.
type cloudFrontResource struct {
	*resourceType
}

func newCloudFrontResource(client *Client) *cloudFrontResource {
	c := &cloudFrontResource{}
	c.resourceType = &resourceType{
		provider: Provider, typeName: TypeCloudFrontDistribution,
		lookup: resource.LookupByTag, client: client,
		match: arrayTagsMatch, stampTag: arrayTagsStampTag,
		translate: func(_ context.Context, spec resource.Spec) (resource.Spec, error) { return c.translate(spec) },
	}
	return c
}

// distributionShape is the projection of a DistributionConfig kraai sets and
// compares: what the manifest decides, and nothing CloudFront defaults.
type distributionShape struct {
	origin      string
	certificate string
	aliases     []string
	enabled     bool
}

// shapeFromSpec reads the entry and the attributes the references it names
// published: the origin bucket's regional endpoint, the certificate's ARN.
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

	shape := distributionShape{origin: originDomain, enabled: true}
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
func shapeFromState(state *resource.State) distributionShape {
	var shape distributionShape
	config, _ := state.Attributes["DistributionConfig"].(map[string]any)
	shape.enabled, _ = config["Enabled"].(bool)
	if origins, ok := config["Origins"].([]any); ok && len(origins) > 0 {
		first, _ := origins[0].(map[string]any)
		shape.origin, _ = first["DomainName"].(string)
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
	return shape
}

// translate builds the distribution: one S3 origin, one cache behaviour
// on AWS's managed CachingOptimized policy, HTTPS enforced, HTTP/2, and the
// certificate for its aliases when it has any — CloudFront's own default
// certificate otherwise, which is only ever valid for its own hostname.
//
// Access from the distribution to a private bucket is not granted here;
// see evatt-labs/kraai#219.
func (c *cloudFrontResource) translate(spec resource.Spec) (resource.Spec, error) {
	shape, err := c.shapeFromSpec(spec)
	if err != nil {
		return resource.Spec{}, err
	}

	config := map[string]any{
		"Enabled":           shape.enabled,
		"Comment":           spec.Name,
		"DefaultRootObject": "index.html",
		"HttpVersion":       "http2",
		"Origins": []any{map[string]any{
			"Id":             cloudFrontOriginID,
			"DomainName":     shape.origin,
			"S3OriginConfig": map[string]any{},
		}},
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
// the type has an update handler, so any difference is Mutable.
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
