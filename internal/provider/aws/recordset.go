package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// recordSetType is the one record kraai writes: the zone apex, as an alias
// A record at the distribution the entry names. An alias A record answers
// AAAA queries too when the target is dual-stacked, and CloudFront is.
const recordSetType = "A"

// recordSetResource provisions the alias record that points a zone's apex at
// the cdn distribution its entry names.
//
// Registered NameFromEntry on "zone", like the hosted zone it lives in, so
// its name is the zone name and the record it stands for is the apex. Only
// planned when the entry names an alias (RequiresBindingKey): a zone with
// nothing to point at has no record to write.
//
// Found by listing the zone's records — Cloud Control's list handler for
// this type requires a zone to list in, and accepts HostedZoneName, which is
// exactly the name this instance carries — and matching the apex A record.
// That closes the two ambiguities recordSetMatch used to document: listing
// one zone removes the cross-zone one, and matching on Type the other.
type recordSetResource struct {
	inner *resourceType
}

func newRecordSetResource(client *Client) *recordSetResource {
	return &recordSetResource{
		inner: &resourceType{
			provider: Provider, typeName: TypeRoute53RecordSet,
			lookup: resource.LookupByAttr, client: client,
			match: recordSetMatch, listScope: recordSetListScope,
		},
	}
}

// recordSetListScope scopes the list to the record's own zone. Route 53
// wants the zone name fully qualified.
func recordSetListScope(zone string) (map[string]any, error) {
	if zone == "" {
		return nil, kerrors.Validation("cannot list %s without a zone to list in", TypeRoute53RecordSet)
	}
	return map[string]any{"HostedZoneName": fqdn(zone)}, nil
}

// fqdn is the zone name with the trailing dot Route 53 wants on input.
func fqdn(zone string) string {
	if len(zone) > 0 && zone[len(zone)-1] == '.' {
		return zone
	}
	return zone + "."
}

// translate builds the record: in the zone its own binding created (the
// zone id, published under the bare key since the two share a binding),
// named for the apex, aliased to the distribution the entry names.
func (r *recordSetResource) translate(spec resource.Spec) (resource.Spec, error) {
	if spec.Name == "" {
		return resource.Spec{}, kerrors.Validation(
			"record set for binding %q has no zone name", spec.Binding)
	}
	alias, _ := spec.Config["alias"].(string)
	if alias == "" {
		return resource.Spec{}, kerrors.Validation(
			"dns binding %q names no alias — the cdn binding the apex record points at", spec.Binding)
	}
	zoneID, err := spec.Attribute(key(TypeRoute53HostedZone), "Id")
	if err != nil {
		return resource.Spec{}, kerrors.Wrap(err, kerrors.CodeValidation,
			"resolving the zone for the apex record of %q", spec.Name)
	}
	target, err := spec.Attribute(alias+"."+key(TypeCloudFrontDistribution), "DomainName")
	if err != nil {
		return resource.Spec{}, kerrors.Wrap(err, kerrors.CodeValidation,
			"resolving the distribution the apex record of %q aliases", spec.Name)
	}

	translated := spec
	translated.Config = map[string]any{
		"HostedZoneId": bareHostedZoneID(zoneID),
		"Name":         fqdn(spec.Name),
		"Type":         recordSetType,
		"AliasTarget": map[string]any{
			"DNSName":              target,
			"HostedZoneId":         cloudFrontHostedZoneID,
			"EvaluateTargetHealth": false,
		},
	}
	return translated, nil
}

func (r *recordSetResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	return r.inner.Get(ctx, ref)
}

func (r *recordSetResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	translated, err := r.translate(spec)
	if err != nil {
		return nil, err
	}
	return r.inner.Create(ctx, translated)
}

func (r *recordSetResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	translated, err := r.translate(spec)
	if err != nil {
		return nil, err
	}
	return r.inner.Update(ctx, ref, translated)
}

func (r *recordSetResource) Delete(ctx context.Context, ref resource.Ref) error {
	return r.inner.Delete(ctx, ref)
}

// Diff compares where the record points. The zone and the name are its
// identity and cannot differ for a record that was found; the target is
// the one thing that changes, when the distribution is replaced.
func (r *recordSetResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	translated, err := r.translate(spec)
	if err != nil {
		return resource.Same, err
	}
	want, _ := translated.Config["AliasTarget"].(map[string]any)
	live, _ := state.Attributes["AliasTarget"].(map[string]any)
	if want["DNSName"] != live["DNSName"] {
		return resource.Mutable, nil
	}
	return resource.Same, nil
}
