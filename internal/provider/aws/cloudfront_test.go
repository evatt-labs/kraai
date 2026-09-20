package aws

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/evatt-labs/kraai/internal/resource"
)

func cdnSpec(config map[string]any, attrs map[string]map[string]any) resource.Spec {
	return resource.Spec{Binding: "EDGE", Name: "env-site-EDGE", Config: config, Attributes: attrs}
}

// cdnAttrs is what the objects binding named "ASSETS" and, optionally, the
// tls binding named "CERT" publish: the origin bucket's regional endpoint
// and its real bucket name (bucketFromOriginDomain(originDomain), the same
// value a live AWS::S3::Bucket's own BucketName property would carry for
// that endpoint), and the certificate's ARN.
func cdnAttrs(originDomain, certARN string) map[string]map[string]any {
	attrs := map[string]map[string]any{
		"ASSETS." + key(TypeS3Bucket): {
			"RegionalDomainName": originDomain,
			"BucketName":         bucketFromOriginDomain(originDomain),
		},
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
	if _, has := origin["OriginAccessControlId"]; has {
		t.Errorf("origin = %v, want no OriginAccessControlId when translate was called with no oacIDConfigKey", origin)
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

// translate reads the OriginAccessControl id in under oacIDConfigKey (how
// Create/Update pass it through, see specWithOACID) and both sets it on the
// origin and strips the key from the shape it hands to shapeFromSpec.
func TestCloudFrontTranslateSetsTheOriginAccessControlID(t *testing.T) {
	cf := newCloudFrontResource(&Client{})
	spec := specWithOACID(cdnSpec(map[string]any{"origin": "ASSETS"}, cdnAttrs("b.s3.amazonaws.com", "")), "OAC123")

	translated, err := cf.translate(spec)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	config, _ := translated.Config["DistributionConfig"].(map[string]any)
	origins, _ := config["Origins"].([]any)
	origin, _ := origins[0].(map[string]any)
	if origin["OriginAccessControlId"] != "OAC123" {
		t.Errorf("OriginAccessControlId = %v, want %q", origin["OriginAccessControlId"], "OAC123")
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

// A live distribution carries dozens of defaulted properties the desired
// state never set; only what the manifest decides is compared, or every
// plan would propose an update.
func TestCloudFrontDiffComparesOnlyWhatTheManifestDecides(t *testing.T) {
	cf := newCloudFrontResource(&Client{})
	spec := cdnSpec(map[string]any{"origin": "ASSETS", "certificate": "CERT", "aliases": []any{"acme.example"}},
		cdnAttrs("b.s3.amazonaws.com", "arn:cert"))
	live := func(origin, cert, oacID string, granted bool, aliases ...any) *resource.State {
		originMap := map[string]any{"Id": "origin", "DomainName": origin, "ConnectionAttempts": 3.0}
		if oacID != "" {
			originMap["OriginAccessControlId"] = oacID
		}
		return &resource.State{Attributes: map[string]any{
			"DistributionConfig": map[string]any{
				"Enabled":           true,
				"Origins":           []any{originMap},
				"ViewerCertificate": map[string]any{"AcmCertificateArn": cert, "MinimumProtocolVersion": "TLSv1.2_2021"},
				"Aliases":           aliases,
				"PriceClass":        "PriceClass_All",
				"HttpVersion":       "http2",
			},
			kraaiOriginGrantedAttribute: granted,
		}}
	}

	if d, err := cf.Diff(spec, live("b.s3.amazonaws.com", "arn:cert", "OAC1", true, "acme.example")); err != nil || d != resource.Same {
		t.Fatalf("Diff of the same shape with defaults = %v, %v, want Same", d, err)
	}
	if d, err := cf.Diff(spec, live("other.s3.amazonaws.com", "arn:cert", "OAC1", true, "acme.example")); err != nil || d != resource.Mutable {
		t.Fatalf("Diff with a different origin = %v, %v, want Mutable", d, err)
	}
	if d, err := cf.Diff(spec, live("b.s3.amazonaws.com", "arn:old", "OAC1", true, "acme.example")); err != nil || d != resource.Mutable {
		t.Fatalf("Diff with a rotated certificate = %v, %v, want Mutable", d, err)
	}
	if d, err := cf.Diff(spec, live("b.s3.amazonaws.com", "arn:cert", "OAC1", true)); err != nil || d != resource.Mutable {
		t.Fatalf("Diff with an alias removed = %v, %v, want Mutable", d, err)
	}
	if d, err := cf.Diff(spec, live("b.s3.amazonaws.com", "arn:cert", "", true, "acme.example")); err != nil || d != resource.Mutable {
		t.Fatalf("Diff with no OriginAccessControl = %v, %v, want Mutable", d, err)
	}
	if d, err := cf.Diff(spec, live("b.s3.amazonaws.com", "arn:cert", "OAC1", false, "acme.example")); err != nil || d != resource.Mutable {
		t.Fatalf("Diff with the grant ungranted = %v, %v, want Mutable", d, err)
	}
}

func TestBucketFromOriginDomain(t *testing.T) {
	for _, c := range []struct{ domain, want string }{
		{"my-bucket.s3.us-east-1.amazonaws.com", "my-bucket"},
		{"my-bucket.s3.amazonaws.com", "my-bucket"},
		{"no-dots", "no-dots"},
		{"", ""},
	} {
		if got := bucketFromOriginDomain(c.domain); got != c.want {
			t.Errorf("bucketFromOriginDomain(%q) = %q, want %q", c.domain, got, c.want)
		}
	}
}

func TestBucketPolicyDocument(t *testing.T) {
	doc, err := bucketPolicyDocument("my-bucket", "arn:aws:cloudfront::123456789012:distribution/E123")
	if err != nil {
		t.Fatalf("bucketPolicyDocument: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(doc), &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if decoded["Version"] != "2012-10-17" {
		t.Errorf("Version = %v", decoded["Version"])
	}
	statements, _ := decoded["Statement"].([]any)
	if len(statements) != 1 {
		t.Fatalf("Statement = %v, want exactly one", decoded["Statement"])
	}
	stmt, _ := statements[0].(map[string]any)
	if stmt["Sid"] != bucketGrantSid || stmt["Effect"] != "Allow" || stmt["Action"] != "s3:GetObject" {
		t.Errorf("statement = %v", stmt)
	}
	if stmt["Resource"] != "arn:aws:s3:::my-bucket/*" {
		t.Errorf("Resource = %v, want the bucket's object ARN", stmt["Resource"])
	}
	principal, _ := stmt["Principal"].(map[string]any)
	if principal["Service"] != "cloudfront.amazonaws.com" {
		t.Errorf("Principal = %v", principal)
	}
	condition, _ := stmt["Condition"].(map[string]any)
	stringEquals, _ := condition["StringEquals"].(map[string]any)
	if stringEquals["AWS:SourceArn"] != "arn:aws:cloudfront::123456789012:distribution/E123" {
		t.Errorf("Condition = %v, want the distribution's own ARN", condition)
	}
}

func TestPolicyDocumentsEqual(t *testing.T) {
	a := `{"Version":"2012-10-17","Statement":[{"Sid":"x","Effect":"Allow"}]}`
	b := ` { "Statement" : [ { "Effect" : "Allow" , "Sid" : "x" } ] , "Version" : "2012-10-17" } `
	eq, err := policyDocumentsEqual(a, b)
	if err != nil || !eq {
		t.Fatalf("policyDocumentsEqual = %v, %v, want equal regardless of key order/whitespace", eq, err)
	}
	c := `{"Version":"2012-10-17","Statement":[{"Sid":"y","Effect":"Allow"}]}`
	eq, err = policyDocumentsEqual(a, c)
	if err != nil || eq {
		t.Fatalf("policyDocumentsEqual = %v, %v, want unequal", eq, err)
	}
}

func TestDistributionARN(t *testing.T) {
	got := distributionARN("123456789012", "E123EXAMPLE")
	want := "arn:aws:cloudfront::123456789012:distribution/E123EXAMPLE"
	if got != want {
		t.Fatalf("distributionARN = %q, want %q", got, want)
	}
}

// cloudFrontHarness wires a cloudFrontResource to a fakeClient (serving
// both the distribution's byTag lookup and the OAC's byAttr lookup — see
// resource_test.go's fakeClient.createResults for how a single fake tells
// the two types' CreateResource calls apart) and a *Client backed by fakeS3
// and fakeSTS, so Create/Update/Delete/Get can be exercised end to end with
// no AWS account or network.
type cloudFrontHarness struct {
	cf  *cloudFrontResource
	cc  *fakeClient
	s3  *fakeS3
	sts *fakeSTS
}

func newCloudFrontHarness() *cloudFrontHarness {
	h := &cloudFrontHarness{
		cc:  &fakeClient{},
		s3:  &fakeS3{},
		sts: &fakeSTS{account: "123456789012"},
	}
	h.cf = newCloudFrontResource(&Client{})
	h.cf.inner.client = h.cc
	h.cf.oac.client = h.cc
	h.cf.client = &Client{s3: h.s3, sts: h.sts}
	return h
}

// oacProps is a live OriginAccessControl's decoded properties, as
// GetResource/CreateResource would return them.
func oacProps(id, name string) map[string]any {
	return map[string]any{
		"Id": id,
		"OriginAccessControlConfig": map[string]any{
			"Name":                          name,
			"OriginAccessControlOriginType": "s3",
			"SigningBehavior":               "always",
			"SigningProtocol":               "sigv4",
		},
	}
}

// Create makes the OriginAccessControl before the distribution, the
// distribution's origin carries its id, and the resulting distribution's
// bucket policy names the right bucket, the right distribution ARN and the
// exact grant document.
func TestCloudFrontCreateMakesTheOACBeforeTheDistributionAndGrants(t *testing.T) {
	h := newCloudFrontHarness()
	h.cc.createResults = map[string]struct {
		id    string
		props map[string]any
		err   error
	}{
		TypeCloudFrontOriginAccessControl: {id: "OAC1", props: oacProps("OAC1", "env-site-EDGE")},
		TypeCloudFrontDistribution:        {id: "E123", props: map[string]any{"DomainName": "d123.cloudfront.net"}},
	}

	spec := cdnSpec(map[string]any{"origin": "ASSETS"}, cdnAttrs("my-bucket.s3.us-east-1.amazonaws.com", ""))
	state, err := h.cf.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if state.ID != "E123" {
		t.Fatalf("state.ID = %q, want the distribution's own id", state.ID)
	}

	if len(h.cc.createOrder) != 2 || h.cc.createOrder[0] != TypeCloudFrontOriginAccessControl ||
		h.cc.createOrder[1] != TypeCloudFrontDistribution {
		t.Fatalf("createOrder = %v, want [OAC, Distribution]", h.cc.createOrder)
	}

	distDesired := h.cc.createCalls[1]
	config, _ := distDesired["DistributionConfig"].(map[string]any)
	origins, _ := config["Origins"].([]any)
	origin, _ := origins[0].(map[string]any)
	if origin["OriginAccessControlId"] != "OAC1" {
		t.Fatalf("origin = %v, want OriginAccessControlId %q", origin, "OAC1")
	}
	if !arrayTagsMatch(distDesired, "env-site-EDGE") {
		t.Errorf("distribution desired state = %v, want the identity tag", distDesired)
	}

	if len(h.s3.putPolicyReq) != 1 {
		t.Fatalf("got %d PutBucketPolicy calls, want 1", len(h.s3.putPolicyReq))
	}
	put := h.s3.putPolicyReq[0]
	if *put.Bucket != "my-bucket" {
		t.Fatalf("PutBucketPolicy bucket = %q, want %q", *put.Bucket, "my-bucket")
	}
	want, err := bucketPolicyDocument("my-bucket", "arn:aws:cloudfront::123456789012:distribution/E123")
	if err != nil {
		t.Fatalf("bucketPolicyDocument: %v", err)
	}
	eq, err := policyDocumentsEqual(want, *put.Policy)
	if err != nil || !eq {
		t.Fatalf("PutBucketPolicy document = %s, want %s (equal=%v, err=%v)", *put.Policy, want, eq, err)
	}
}

// A retry after a Create that made the OAC but crashed before the
// distribution existed must find the same OAC by name, not create a
// second one.
func TestCloudFrontCreateFindsAnExistingOACInstead(t *testing.T) {
	h := newCloudFrontHarness()
	h.cc.list = []string{"OAC1"}
	h.cc.byIdentifier = map[string]map[string]any{"OAC1": oacProps("OAC1", "env-site-EDGE")}
	h.cc.createResults = map[string]struct {
		id    string
		props map[string]any
		err   error
	}{
		TypeCloudFrontDistribution: {id: "E123", props: map[string]any{}},
	}

	spec := cdnSpec(map[string]any{"origin": "ASSETS"}, cdnAttrs("my-bucket.s3.us-east-1.amazonaws.com", ""))
	if _, err := h.cf.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, typeName := range h.cc.createOrder {
		if typeName == TypeCloudFrontOriginAccessControl {
			t.Fatalf("createOrder = %v, want no OAC create — the existing one should have been found by name", h.cc.createOrder)
		}
	}
	if len(h.cc.createCalls) != 1 {
		t.Fatalf("got %d CreateResource calls, want exactly 1 (the distribution)", len(h.cc.createCalls))
	}
	distDesired := h.cc.createCalls[0]
	config, _ := distDesired["DistributionConfig"].(map[string]any)
	origins, _ := config["Origins"].([]any)
	origin, _ := origins[0].(map[string]any)
	if origin["OriginAccessControlId"] != "OAC1" {
		t.Fatalf("origin = %v, want the existing OAC's id %q", origin, "OAC1")
	}
}

// Update on a distribution created before this workstream — no
// OriginAccessControl on its origin — creates one and re-grants, rather
// than failing or leaving the origin ungranted.
func TestCloudFrontUpdateCreatesAnOACWhenTheDistributionHasNone(t *testing.T) {
	h := newCloudFrontHarness()
	h.cc.schema = Schema{Handlers: map[string]json.RawMessage{"update": json.RawMessage("{}")}}
	ref := resource.Ref{Provider: Provider, Type: TypeCloudFrontDistribution, Name: "env-site-EDGE"}
	h.cc.list = []string{"E123"}
	h.cc.byIdentifier = map[string]map[string]any{
		"E123": {
			"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "env-site-EDGE"}},
			"DistributionConfig": map[string]any{
				"Enabled": true,
				"Origins": []any{map[string]any{
					"Id": cloudFrontOriginID, "DomainName": "my-bucket.s3.us-east-1.amazonaws.com",
				}},
			},
		},
	}
	h.cc.createResults = map[string]struct {
		id    string
		props map[string]any
		err   error
	}{
		TypeCloudFrontOriginAccessControl: {id: "OACNEW", props: oacProps("OACNEW", "env-site-EDGE")},
	}
	h.cc.updateProps = map[string]any{"DomainName": "d123.cloudfront.net"}

	spec := cdnSpec(map[string]any{"origin": "ASSETS"}, cdnAttrs("my-bucket.s3.us-east-1.amazonaws.com", ""))
	if _, err := h.cf.Update(context.Background(), ref, spec); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(h.cc.createCalls) != 1 || h.cc.createOrder[0] != TypeCloudFrontOriginAccessControl {
		t.Fatalf("createOrder = %v, want exactly one OAC create", h.cc.createOrder)
	}
	if len(h.cc.updateCalls) != 1 {
		t.Fatalf("got %d UpdateResource calls, want 1", len(h.cc.updateCalls))
	}
	var patch []map[string]any
	if err := json.Unmarshal(h.cc.updatePatches[0], &patch); err != nil {
		t.Fatalf("decoding patch: %v", err)
	}
	if len(patch) != 1 {
		t.Fatalf("patch = %v, want exactly one operation", patch)
	}
	value, _ := patch[0]["value"].(map[string]any)
	origins, _ := value["Origins"].([]any)
	origin, _ := origins[0].(map[string]any)
	if origin["OriginAccessControlId"] != "OACNEW" {
		t.Fatalf("patched DistributionConfig = %v, want origin OriginAccessControlId %q", value, "OACNEW")
	}
	if len(h.s3.putPolicyReq) != 1 || *h.s3.putPolicyReq[0].Bucket != "my-bucket" {
		t.Fatalf("PutBucketPolicy calls = %v, want one against my-bucket", h.s3.putPolicyReq)
	}
}

// When the origin bucket changed, Update removes the old bucket's policy
// before granting the new one.
func TestCloudFrontUpdateMovesTheGrantWhenTheOriginBucketChanged(t *testing.T) {
	h := newCloudFrontHarness()
	h.cc.schema = Schema{Handlers: map[string]json.RawMessage{"update": json.RawMessage("{}")}}
	ref := resource.Ref{Provider: Provider, Type: TypeCloudFrontDistribution, Name: "env-site-EDGE"}
	h.cc.list = []string{"E123"}
	h.cc.byIdentifier = map[string]map[string]any{
		"E123": {
			"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "env-site-EDGE"}},
			"DistributionConfig": map[string]any{
				"Enabled": true,
				"Origins": []any{map[string]any{
					"Id": cloudFrontOriginID, "DomainName": "old-bucket.s3.us-east-1.amazonaws.com",
					"OriginAccessControlId": "OAC1",
				}},
			},
		},
	}
	h.cc.updateProps = map[string]any{"DomainName": "d123.cloudfront.net"}

	spec := cdnSpec(map[string]any{"origin": "ASSETS"}, cdnAttrs("new-bucket.s3.us-east-1.amazonaws.com", ""))
	if _, err := h.cf.Update(context.Background(), ref, spec); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(h.cc.createCalls) != 0 {
		t.Fatalf("createOrder = %v, want no OAC create — the live one already has an id", h.cc.createOrder)
	}
	if len(h.s3.deletePolicyReq) != 1 || *h.s3.deletePolicyReq[0].Bucket != "old-bucket" {
		t.Fatalf("DeleteBucketPolicy calls = %v, want one against old-bucket", h.s3.deletePolicyReq)
	}
	if len(h.s3.putPolicyReq) != 1 || *h.s3.putPolicyReq[0].Bucket != "new-bucket" {
		t.Fatalf("PutBucketPolicy calls = %v, want one against new-bucket", h.s3.putPolicyReq)
	}
}

// Delete order: distribution, then OAC, then bucket policy. Delete of an
// absent distribution is a no-op that touches neither the OAC nor the
// policy.
func TestCloudFrontDeleteOrder(t *testing.T) {
	h := newCloudFrontHarness()
	ref := resource.Ref{Provider: Provider, Type: TypeCloudFrontDistribution, Name: "env-site-EDGE"}
	h.cc.list = []string{"E123"}
	h.cc.byIdentifier = map[string]map[string]any{
		"E123": {
			"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "env-site-EDGE"}},
			"DistributionConfig": map[string]any{
				"Enabled": true,
				"Origins": []any{map[string]any{
					"Id": cloudFrontOriginID, "DomainName": "my-bucket.s3.us-east-1.amazonaws.com",
					"OriginAccessControlId": "OAC1",
				}},
			},
		},
	}
	h.s3.listBucketsOut = []*s3.ListBucketsOutput{{Buckets: nil}} // overwritten per subtest below

	t.Run("deletes the distribution, then the OAC, then the policy", func(t *testing.T) {
		h.s3.listBucketsOut = nil // fakeS3's ListBuckets default: this account owns whatever bucket is asked about
		if err := h.cf.Delete(context.Background(), ref); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		// The distribution is deleted first, then the OAC it referenced —
		// both go through the same fakeClient's DeleteResource, in order.
		if len(h.cc.deleteCalls) != 2 || h.cc.deleteCalls[0] != "E123" || h.cc.deleteCalls[1] != "OAC1" {
			t.Fatalf("deleteCalls = %v, want [E123, OAC1]", h.cc.deleteCalls)
		}
		if len(h.s3.deletePolicyReq) != 1 || *h.s3.deletePolicyReq[0].Bucket != "my-bucket" {
			t.Fatalf("DeleteBucketPolicy calls = %v, want one against my-bucket", h.s3.deletePolicyReq)
		}
	})

	t.Run("absent distribution is a no-op", func(t *testing.T) {
		h2 := newCloudFrontHarness()
		if err := h2.cf.Delete(context.Background(), resource.Ref{Provider: Provider, Type: TypeCloudFrontDistribution, Name: "gone"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(h2.cc.deleteCalls) != 0 || len(h2.s3.deletePolicyReq) != 0 {
			t.Fatalf("Delete of an absent distribution touched the OAC or the policy: deleteCalls=%v, deletePolicyReq=%v",
				h2.cc.deleteCalls, h2.s3.deletePolicyReq)
		}
	})
}

// Delete skips the bucket policy entirely when the origin bucket is not
// this account's own — deleting a stranger's policy is never this
// package's to do.
func TestCloudFrontDeleteSkipsAForeignBucketsPolicy(t *testing.T) {
	h := newCloudFrontHarness()
	ref := resource.Ref{Provider: Provider, Type: TypeCloudFrontDistribution, Name: "env-site-EDGE"}
	h.cc.list = []string{"E123"}
	h.cc.byIdentifier = map[string]map[string]any{
		"E123": {
			"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "env-site-EDGE"}},
			"DistributionConfig": map[string]any{
				"Enabled": true,
				"Origins": []any{map[string]any{
					"Id": cloudFrontOriginID, "DomainName": "my-bucket.s3.us-east-1.amazonaws.com",
				}},
			},
		},
	}
	h.s3.listBucketsOut = []*s3.ListBucketsOutput{{Buckets: nil}}

	if err := h.cf.Delete(context.Background(), ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(h.s3.deletePolicyReq) != 0 {
		t.Fatalf("DeleteBucketPolicy calls = %v, want none against a bucket this account does not own", h.s3.deletePolicyReq)
	}
}

// Get records the live grant under kraaiOriginGrantedAttribute.
func TestCloudFrontGetRecordsTheGrant(t *testing.T) {
	h := newCloudFrontHarness()
	ref := resource.Ref{Provider: Provider, Type: TypeCloudFrontDistribution, Name: "env-site-EDGE"}
	h.cc.list = []string{"E123"}
	h.cc.byIdentifier = map[string]map[string]any{
		"E123": {
			"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "env-site-EDGE"}},
			"DistributionConfig": map[string]any{
				"Enabled": true,
				"Origins": []any{map[string]any{
					"Id": cloudFrontOriginID, "DomainName": "my-bucket.s3.us-east-1.amazonaws.com",
					"OriginAccessControlId": "OAC1",
				}},
			},
		},
	}

	t.Run("granted when the live policy matches", func(t *testing.T) {
		doc, err := bucketPolicyDocument("my-bucket", "arn:aws:cloudfront::123456789012:distribution/E123")
		if err != nil {
			t.Fatalf("bucketPolicyDocument: %v", err)
		}
		h.s3.getPolicyOut = &s3.GetBucketPolicyOutput{Policy: aws.String(doc)}

		state, err := h.cf.Get(context.Background(), ref)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if state.Attributes[kraaiOriginGrantedAttribute] != true {
			t.Fatalf("%s = %v, want true", kraaiOriginGrantedAttribute, state.Attributes[kraaiOriginGrantedAttribute])
		}
	})

	t.Run("ungranted when there is no live policy", func(t *testing.T) {
		h.s3.getPolicyOut = nil

		state, err := h.cf.Get(context.Background(), ref)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if state.Attributes[kraaiOriginGrantedAttribute] != false {
			t.Fatalf("%s = %v, want false", kraaiOriginGrantedAttribute, state.Attributes[kraaiOriginGrantedAttribute])
		}
	})

	t.Run("absent distribution reports no state at all", func(t *testing.T) {
		state, err := h.cf.Get(context.Background(), resource.Ref{Provider: Provider, Type: TypeCloudFrontDistribution, Name: "gone"})
		if err != nil || state != nil {
			t.Fatalf("Get = %v, %v, want (nil, nil)", state, err)
		}
	})
}
