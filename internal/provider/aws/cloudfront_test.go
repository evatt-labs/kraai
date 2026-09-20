package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

func cdnSpec(config map[string]any, attrs map[string]map[string]any) resource.Spec {
	return resource.Spec{Binding: "EDGE", Name: "env-site-EDGE", Config: config, Attributes: attrs}
}

func cdnAttrs(originDomain, certARN string) map[string]map[string]any {
	attrs := map[string]map[string]any{
		"ASSETS." + key(TypeS3Bucket): {"RegionalDomainName": originDomain},
	}
	if certARN != "" {
		attrs["CERT."+key(TypeCertificateManagerCertificate)] = map[string]any{certificateArnAttribute: certARN}
	}
	return attrs
}

// The distribution fronts the bucket its entry names and presents the
// certificate its entry names, both read from what those bindings
// published under their namespaced keys.
func TestCloudFrontTranslateBuildsTheDistributionFromItsReferences(t *testing.T) {
	cf := newCloudFrontResource(&Client{})
	spec := cdnSpec(map[string]any{
		"origin": "ASSETS", "certificate": "CERT", "aliases": []any{"www.acme.example", "acme.example"},
	}, cdnAttrs("bucket.s3.us-east-1.amazonaws.com", "arn:cert"))

	translated, err := cf.translate(spec)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	config, _ := translated.Config["DistributionConfig"].(map[string]any)
	if config["Enabled"] != true || config["DefaultRootObject"] != "index.html" {
		t.Errorf("Enabled/DefaultRootObject = %v/%v", config["Enabled"], config["DefaultRootObject"])
	}
	origins, _ := config["Origins"].([]any)
	if len(origins) != 1 {
		t.Fatalf("Origins = %v, want one", config["Origins"])
	}
	origin, _ := origins[0].(map[string]any)
	if origin["DomainName"] != "bucket.s3.us-east-1.amazonaws.com" || origin["Id"] != cloudFrontOriginID {
		t.Errorf("origin = %v, want the bucket's regional endpoint", origin)
	}
	behaviour, _ := config["DefaultCacheBehavior"].(map[string]any)
	if behaviour["TargetOriginId"] != cloudFrontOriginID || behaviour["ViewerProtocolPolicy"] != "redirect-to-https" ||
		behaviour["CachePolicyId"] != cloudFrontCachingOptimizedPolicyID {
		t.Errorf("DefaultCacheBehavior = %v", behaviour)
	}
	viewer, _ := config["ViewerCertificate"].(map[string]any)
	if viewer["AcmCertificateArn"] != "arn:cert" || viewer["SslSupportMethod"] != "sni-only" {
		t.Errorf("ViewerCertificate = %v", viewer)
	}
	aliases, _ := config["Aliases"].([]any)
	if len(aliases) != 2 || aliases[0] != "acme.example" || aliases[1] != "www.acme.example" {
		t.Errorf("Aliases = %v, want both, sorted", aliases)
	}
}

// Without aliases there is nothing a certificate is for, and CloudFront's
// own certificate covers the distribution's own hostname.
func TestCloudFrontTranslateWithoutACertificateUsesTheDefault(t *testing.T) {
	cf := newCloudFrontResource(&Client{})
	translated, err := cf.translate(cdnSpec(map[string]any{"origin": "ASSETS"}, cdnAttrs("b.s3.amazonaws.com", "")))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	config, _ := translated.Config["DistributionConfig"].(map[string]any)
	viewer, _ := config["ViewerCertificate"].(map[string]any)
	if viewer["CloudFrontDefaultCertificate"] != true {
		t.Errorf("ViewerCertificate = %v, want the default certificate", viewer)
	}
	if _, has := config["Aliases"]; has {
		t.Error("Aliases present with none declared")
	}
}

func TestCloudFrontTranslateRefusals(t *testing.T) {
	cf := newCloudFrontResource(&Client{})
	for _, c := range []struct {
		name    string
		spec    resource.Spec
		wantErr string
	}{
		{"no origin", cdnSpec(map[string]any{}, nil), "names no origin"},
		{"origin not published", cdnSpec(map[string]any{"origin": "ASSETS"}, nil), key(TypeS3Bucket)},
		{"aliases without a certificate", cdnSpec(map[string]any{"origin": "ASSETS", "aliases": []any{"acme.example"}},
			cdnAttrs("b", "")), "names no certificate"},
		{"certificate not published", cdnSpec(map[string]any{"origin": "ASSETS", "certificate": "CERT"},
			cdnAttrs("b", "")), key(TypeCertificateManagerCertificate)},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := cf.translate(c.spec)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

// Create carries the identity tag the byTag lookup finds it by again.
func TestCloudFrontCreateStampsTheTag(t *testing.T) {
	cf := newCloudFrontResource(&Client{})
	client := &fakeClient{createID: "E123", createProps: map[string]any{"DomainName": "d123.cloudfront.net"}}
	cf.client = client

	if _, err := cf.Create(context.Background(), cdnSpec(map[string]any{"origin": "ASSETS"}, cdnAttrs("b", ""))); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !arrayTagsMatch(client.createCalls[0], "env-site-EDGE") {
		t.Errorf("desired = %v, want the identity tag", client.createCalls[0])
	}
}

// A live distribution carries dozens of defaulted properties the desired
// state never set; only what the manifest decides is compared, or every
// plan would propose an update.
func TestCloudFrontDiffComparesOnlyWhatTheManifestDecides(t *testing.T) {
	cf := newCloudFrontResource(&Client{})
	spec := cdnSpec(map[string]any{"origin": "ASSETS", "certificate": "CERT", "aliases": []any{"acme.example"}},
		cdnAttrs("b.s3.amazonaws.com", "arn:cert"))
	live := func(origin, cert string, aliases ...any) *resource.State {
		return &resource.State{Attributes: map[string]any{"DistributionConfig": map[string]any{
			"Enabled":           true,
			"Origins":           []any{map[string]any{"Id": "origin", "DomainName": origin, "ConnectionAttempts": 3.0}},
			"ViewerCertificate": map[string]any{"AcmCertificateArn": cert, "MinimumProtocolVersion": "TLSv1.2_2021"},
			"Aliases":           aliases,
			"PriceClass":        "PriceClass_All",
			"HttpVersion":       "http2",
		}}}
	}

	if d, err := cf.Diff(spec, live("b.s3.amazonaws.com", "arn:cert", "acme.example")); err != nil || d != resource.Same {
		t.Fatalf("Diff of the same shape with defaults = %v, %v, want Same", d, err)
	}
	if d, err := cf.Diff(spec, live("other.s3.amazonaws.com", "arn:cert", "acme.example")); err != nil || d != resource.Mutable {
		t.Fatalf("Diff with a different origin = %v, %v, want Mutable", d, err)
	}
	if d, err := cf.Diff(spec, live("b.s3.amazonaws.com", "arn:old", "acme.example")); err != nil || d != resource.Mutable {
		t.Fatalf("Diff with a rotated certificate = %v, %v, want Mutable", d, err)
	}
	if d, err := cf.Diff(spec, live("b.s3.amazonaws.com", "arn:cert")); err != nil || d != resource.Mutable {
		t.Fatalf("Diff with an alias removed = %v, %v, want Mutable", d, err)
	}
}
