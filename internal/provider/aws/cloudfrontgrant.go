package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// checkGrant reports whether the origin bucket named in state's live
// DistributionConfig carries a policy granting this exact distribution
// s3:GetObject. false, not an error, when the distribution has no
// recognizable origin: Get must not fail on a distribution this package did
// not create in the shape it expects.
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
// hand-built as a string, so the policy written and the one tests compare
// against cannot drift on formatting.
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

// bucketPolicyDocument builds the policy granting distARN's distribution
// s3:GetObject on bucket, conditioned on its SourceArn so no other
// distribution's requests are covered.
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

// policyDocumentsEqual compares two policy documents structurally: a live
// policy S3 echoes back may differ in key order or whitespace.
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
// without mutating the caller's map, which a concurrent Diff may be reading.
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
// creating one if none exists. name is the distribution's derived name, so
// a retry after a Create that made the OAC but failed before the
// distribution finds the same one again.
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
