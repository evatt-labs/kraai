package aws

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// hostedZoneIDPrefix is what Route 53's own API puts in front of a zone id
// in some responses ("/hostedzone/Z123"); every property that takes a zone
// id wants the bare form.
const hostedZoneIDPrefix = "/hostedzone/"

// hostedZoneResource provisions a Route 53 public hosted zone named by the
// dns entry's zone.
//
// The bare engine could not create one: this type is LookupByAPI, so the
// generic name injection (byName only) never ran, and Name is not a
// required property, so an empty desired state was accepted and produced a
// zone kraai could never find again — the silent-leak case
// evatt-labs/kraai#117 named. The translate here is what closes it.
type hostedZoneResource struct {
	inner *resourceType
}

func newHostedZoneResource(client *Client) *hostedZoneResource {
	return &hostedZoneResource{
		inner: &resourceType{
			provider: Provider, typeName: TypeRoute53HostedZone,
			lookup: resource.LookupByAPI, client: client, match: hostedZoneMatch,
			stampTag: hostedZoneStampTag, owns: hostedZoneOwned,
		},
	}
}

// translate builds the zone's desired state: its name, which is the
// instance's own name (the registration is NameFromEntry on "zone"). The
// identity tag is stamped by the engine's Create.
func (h *hostedZoneResource) translate(spec resource.Spec) (resource.Spec, error) {
	if spec.Name == "" {
		return resource.Spec{}, kerrors.Validation(
			"hosted zone for binding %q has no zone name", spec.Binding)
	}
	translated := spec
	translated.Config = map[string]any{"Name": spec.Name}
	return translated, nil
}

func (h *hostedZoneResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	return h.inner.Get(ctx, ref)
}

func (h *hostedZoneResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	translated, err := h.translate(spec)
	if err != nil {
		return nil, err
	}
	return h.inner.Create(ctx, translated)
}

func (h *hostedZoneResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	translated, err := h.translate(spec)
	if err != nil {
		return nil, err
	}
	return h.inner.Update(ctx, ref, translated)
}

func (h *hostedZoneResource) Delete(ctx context.Context, ref resource.Ref) error {
	return h.inner.Delete(ctx, ref)
}

// Diff compares the one property kraai sets. Name is createOnly, so a
// renamed zone is a replacement — which the manifest cannot express anyway,
// since the zone name is the instance's identity.
func (h *hostedZoneResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	if _, err := h.translate(spec); err != nil {
		return resource.Same, err
	}
	// Route 53 reports the name with a trailing dot; compare as the zone
	// name it is, not as the string the engine would.
	liveName, _ := state.Attributes["Name"].(string)
	if !zoneNamesEqual(liveName, spec.Name) {
		return resource.Immutable, nil
	}
	return resource.Same, nil
}

// bareHostedZoneID strips Route 53's "/hostedzone/" prefix, which some
// responses carry and no property that takes a zone id accepts.
func bareHostedZoneID(id string) string {
	return strings.TrimPrefix(id, hostedZoneIDPrefix)
}
