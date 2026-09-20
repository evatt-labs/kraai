package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

func certificateSpec(config map[string]any, zoneID string) resource.Spec {
	spec := resource.Spec{Binding: "CERT", Name: "env-site-CERT", Config: config}
	if zoneID != "" {
		spec.Attributes = map[string]map[string]any{
			"ZONE." + key(TypeRoute53HostedZone): {"Id": zoneID},
		}
	}
	return spec
}

// A certificate kraai requests is validated by ACM itself, through the zone
// the entry references: one validation option per name, all pointing at the
// zone's id, which the zone published and the reference ordered first.
func TestCertificateTranslateValidatesThroughTheReferencedZone(t *testing.T) {
	cert := newCertificateResource(&Client{})
	spec := certificateSpec(map[string]any{
		"domain": "acme.example", "zone": "ZONE",
		"alternateNames": []any{"www.acme.example"},
	}, "/hostedzone/Z123")

	translated, err := cert.translate(spec)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	cfg := translated.Config
	if cfg["DomainName"] != "acme.example" || cfg["ValidationMethod"] != certificateValidationMethod {
		t.Errorf("DomainName/ValidationMethod = %v/%v", cfg["DomainName"], cfg["ValidationMethod"])
	}
	sans, _ := cfg["SubjectAlternativeNames"].([]any)
	if len(sans) != 1 || sans[0] != "www.acme.example" {
		t.Errorf("SubjectAlternativeNames = %v", cfg["SubjectAlternativeNames"])
	}
	options, _ := cfg["DomainValidationOptions"].([]any)
	if len(options) != 2 {
		t.Fatalf("DomainValidationOptions = %v, want one per name", cfg["DomainValidationOptions"])
	}
	for i, name := range []string{"acme.example", "www.acme.example"} {
		opt, _ := options[i].(map[string]any)
		if opt["DomainName"] != name || opt["HostedZoneId"] != "Z123" {
			t.Errorf("option %d = %v, want %s validated in Z123 (bare id)", i, opt, name)
		}
	}
}

// Without a zone there is nothing to validate in, and a requested
// certificate would sit pending forever; the error says what to do instead.
func TestCertificateTranslateRefusesWithoutAZone(t *testing.T) {
	cert := newCertificateResource(&Client{})

	_, err := cert.translate(certificateSpec(map[string]any{"domain": "acme.example"}, ""))
	if err == nil {
		t.Fatal("a certificate was requested with no zone to validate in")
	}
	for _, want := range []string{"zone", "resources:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}

	_, err = cert.translate(certificateSpec(map[string]any{"zone": "ZONE"}, "Z123"))
	if err == nil || !strings.Contains(err.Error(), "domain") {
		t.Fatalf("err = %v, want the missing domain named", err)
	}
}

// The zone named but not yet published is the ordering failure Rule 20
// wants named, not an empty HostedZoneId ACM rejects far from here.
func TestCertificateTranslateFailsLoudlyWithoutTheZoneAttribute(t *testing.T) {
	cert := newCertificateResource(&Client{})
	client := &fakeClient{}
	cert.client = client

	_, err := cert.Create(context.Background(), certificateSpec(map[string]any{"domain": "acme.example", "zone": "ZONE"}, ""))
	if err == nil {
		t.Fatal("a certificate was requested with no zone id")
	}
	if !strings.Contains(err.Error(), key(TypeRoute53HostedZone)) {
		t.Errorf("error should name the producer that never published: %v", err)
	}
	if len(client.createCalls) != 0 {
		t.Fatal("a create was submitted despite the missing zone id")
	}
}

// Create stamps the identity tag the byTag lookup needs, in the same call.
func TestCertificateCreateStampsTheTag(t *testing.T) {
	cert := newCertificateResource(&Client{})
	client := &fakeClient{createID: "arn:aws:acm:us-east-1:1:certificate/abc"}
	cert.client = client

	if _, err := cert.Create(context.Background(),
		certificateSpec(map[string]any{"domain": "acme.example", "zone": "ZONE"}, "Z123")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !arrayTagsMatch(client.createCalls[0], "env-site-CERT") {
		t.Errorf("desired = %v, want the identity tag", client.createCalls[0])
	}
}

// An adopted certificate carries no zone and is never rewritten: plan reads
// it and compares nothing, rather than refusing the import for lacking a
// zone it never needed.
func TestCertificateDiffOfAnAdoptedCertificateIsSame(t *testing.T) {
	cert := newCertificateResource(&Client{})
	d, err := cert.Diff(certificateSpec(map[string]any{"domain": "acme.example"}, ""),
		&resource.State{Attributes: map[string]any{"DomainName": "acme.example"}})
	if err != nil || d != resource.Same {
		t.Fatalf("Diff = %v, %v, want Same", d, err)
	}
}
