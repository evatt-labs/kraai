package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// certificateValidationMethod is the one validation ACM can complete on its
// own when it is told the zone: with DomainValidationOptions naming a hosted
// zone this account controls, CloudFormation writes the validation CNAME
// there and waits for ISSUED. Email validation would leave the certificate
// pending on a human forever.
const certificateValidationMethod = "DNS"

// certificateResource requests an ACM certificate for the tls entry's
// domain, validated through the dns binding the entry names as its zone.
//
// The bare engine could not create one: DomainName is required, so an empty
// desired state was refused loudly (evatt-labs/kraai#117), but a translate
// supplying only the name would have been worse — a DNS-validated
// certificate with no zone to validate in stays PENDING_VALIDATION until
// something else writes the record. Naming the zone is what lets ACM finish
// the job itself, which is why this translate refuses to run without one.
type certificateResource struct {
	*resourceType
}

func newCertificateResource(client *Client) *certificateResource {
	c := &certificateResource{}
	c.resourceType = &resourceType{
		provider: Provider, typeName: TypeCertificateManagerCertificate,
		lookup: resource.LookupByTag, client: client,
		match: certificateMatch, stampTag: certificateStampTag,
		translate: func(_ context.Context, spec resource.Spec) (resource.Spec, error) { return c.translate(spec) },
	}
	return c
}

// translate builds the request: the domain and its alternate names, DNS
// validation, and a validation option per name pointing at the zone the
// entry references — whose id the zone published under
// "<zone binding>.<Ref.Key()>", the graph having ordered this after it.
func (c *certificateResource) translate(spec resource.Spec) (resource.Spec, error) {
	domain, _ := spec.Config["domain"].(string)
	if domain == "" {
		return resource.Spec{}, kerrors.Validation(
			"tls binding %q declares no domain to request a certificate for", spec.Binding)
	}
	zone, _ := spec.Config["zone"].(string)
	if zone == "" {
		return resource.Spec{}, kerrors.Validation(
			"tls binding %q names no zone to validate in — a certificate kraai requests is validated "+
				"by a record in a hosted zone it manages, so name a dns binding as zone, or adopt "+
				"an existing certificate under the environment's resources: block", spec.Binding)
	}
	zoneID, err := referencedAttribute(spec, zone, TypeRoute53HostedZone, "Id")
	if err != nil {
		return resource.Spec{}, kerrors.Wrap(err, kerrors.CodeValidation,
			"resolving the zone %q validates the certificate for %q in", zone, domain)
	}
	zoneID = bareHostedZoneID(zoneID)

	names := []string{domain}
	var alternates []any
	if raw, ok := spec.Config["alternateNames"].([]any); ok {
		for _, n := range raw {
			if s, ok := n.(string); ok && s != "" {
				alternates = append(alternates, s)
				names = append(names, s)
			}
		}
	}
	options := make([]any, 0, len(names))
	for _, n := range names {
		options = append(options, map[string]any{"DomainName": n, "HostedZoneId": zoneID})
	}

	config := map[string]any{
		"DomainName":              domain,
		"ValidationMethod":        certificateValidationMethod,
		"DomainValidationOptions": options,
	}
	if len(alternates) > 0 {
		config["SubjectAlternativeNames"] = alternates
	}
	translated := spec
	translated.Config = config
	return translated, nil
}

// Diff defers to the schema: DomainName and SubjectAlternativeNames are
// createOnly, so a changed name is a replacement; ValidationMethod is
// writeOnly and never compared. An adopted certificate carries no zone in
// its entry and is never translated here — plan compares it only when it
// exists, and an import that exists is read, never rewritten.
func (c *certificateResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	if zone, _ := spec.Config["zone"].(string); zone == "" {
		// Adopted, or declared without a zone: nothing kraai would write,
		// so nothing to differ on. A missing zone is reported at create,
		// where it matters.
		return resource.Same, nil
	}
	return c.resourceType.Diff(spec, state)
}
