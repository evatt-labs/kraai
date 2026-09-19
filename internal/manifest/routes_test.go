package manifest

import (
	"strings"
	"testing"
)

func serviceWithCert() map[string]Service {
	return map[string]Service{"api": {Bindings: Bindings{
		CapabilityTLS: {{"binding": "CERT", "domain": "api.example.com"}},
	}}}
}

// A custom domain is a resource kraai builds, and it cannot be built without
// the certificate it presents. This is the check kraai-api's production
// manifest fails today — its route claimed a custom domain as a security
// boundary with nothing behind it.
func TestValidateRoutesCustomDomainRequiresACertificate(t *testing.T) {
	env := &Environment{Routes: map[string][]Route{
		"api": {{Pattern: "api.example.com", CustomDomain: true}},
	}}
	err := validateRoutes("prod", serviceWithCert(), env)
	if err == nil {
		t.Fatal("a custom domain with no certificate was accepted")
	}
	for _, want := range []string{"routes.api[0]", "custom_domain requires certificate", "api.example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestValidateRoutesCertificateMustBeATLSBindingOnThatService(t *testing.T) {
	env := &Environment{Routes: map[string][]Route{
		"api": {{Pattern: "api.example.com", CustomDomain: true, Certificate: "NOPE"}},
	}}
	err := validateRoutes("prod", serviceWithCert(), env)
	if err == nil {
		t.Fatal("a certificate naming no tls binding was accepted")
	}
	if !strings.Contains(err.Error(), `"NOPE" is not a tls binding`) {
		t.Errorf("error should name the missing binding: %v", err)
	}

	// A binding of the right name under the wrong capability does not count.
	wrongCapability := map[string]Service{"api": {Bindings: Bindings{
		CapabilityObjects: {{"binding": "CERT"}},
	}}}
	env.Routes["api"][0].Certificate = "CERT"
	if err := validateRoutes("prod", wrongCapability, env); err == nil {
		t.Fatal("an objects binding was accepted as a certificate")
	}
}

func TestValidateRoutesCertificateWithoutCustomDomainIsAnError(t *testing.T) {
	env := &Environment{Routes: map[string][]Route{
		"api": {{Pattern: "api.example.com", Certificate: "CERT"}},
	}}
	err := validateRoutes("prod", serviceWithCert(), env)
	if err == nil {
		t.Fatal("a certificate on a non-custom-domain route was accepted")
	}
	if !strings.Contains(err.Error(), "custom_domain is not") {
		t.Errorf("error should explain the pairing: %v", err)
	}
}

func TestValidateRoutesRejectsAnUndeclaredServiceAndAnEmptyPattern(t *testing.T) {
	err := validateRoutes("prod", serviceWithCert(), &Environment{Routes: map[string][]Route{
		"ghost": {{Pattern: "x.example.com"}},
	}})
	if err == nil || !strings.Contains(err.Error(), `"ghost" is not a declared service`) {
		t.Errorf("undeclared service: %v", err)
	}

	err = validateRoutes("prod", serviceWithCert(), &Environment{Routes: map[string][]Route{
		"api": {{Pattern: ""}},
	}})
	if err == nil || !strings.Contains(err.Error(), "pattern is required") {
		t.Errorf("empty pattern: %v", err)
	}
}

func TestValidateRoutesAcceptsAWellFormedCustomDomainAndADefaultRoute(t *testing.T) {
	env := &Environment{Routes: map[string][]Route{
		"api": {
			{Pattern: "api.example.com", CustomDomain: true, Certificate: "CERT"},
			{Pattern: "api-alt.example.com"},
		},
	}}
	if err := validateRoutes("prod", serviceWithCert(), env); err != nil {
		t.Fatalf("a valid routes block was rejected: %v", err)
	}
	if err := validateRoutes("prod", serviceWithCert(), &Environment{}); err != nil {
		t.Fatalf("no routes at all was rejected: %v", err)
	}
}
