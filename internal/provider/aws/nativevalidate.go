package aws

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// ValidateSpec checks the entry's properties before any read, so a typo is
// caught on a fresh environment too: the create request is built as Create
// would build it and validated against the type's own schema, after the
// checks for what kraai owns and the vendor assigns.
//
// A value referencing a resource that does not exist yet is not judged
// against the schema; the property it names on that resource's type is.
func (n *nativeResource) ValidateSpec(spec resource.Spec) error {
	if n.refused != nil {
		return n.refusal()
	}
	properties, err := nativeProperties(spec)
	if err != nil {
		return err
	}
	if err := n.validateMatch(spec, properties); err != nil {
		return err
	}
	if _, grants := spec.Config[nativeGrantKey]; grants {
		if _, ok := arnProperty(n.facts); !ok {
			return kerrors.Validation(
				"%s publishes no ARN to scope a grant to; it cannot be granted to the service's function", n.typeName)
		}
	}
	res, err := resolveReferences(spec, properties, false)
	if err != nil {
		return err
	}
	if err := n.checkReferencedProperties(spec, res.references); err != nil {
		return err
	}
	properties = res.properties
	if n.facts.TypeName == TypeSQSQueue {
		if err := deriveFifoQueueName(spec, properties); err != nil {
			return err
		}
	}
	if p := n.facts.IdentityProperty; n.lookup == resource.LookupByName && p != "" {
		if _, set := properties[p]; set {
			return kerrors.Validation(
				"%s is the identifier kraai derives from the binding name (%q); remove it from properties",
				p, spec.Name)
		}
	}
	var readOnly []string
	for _, pointer := range n.facts.ReadOnly {
		if path := schemaPropertyPath(pointer); len(path) == 1 {
			if _, set := properties[path[0]]; set {
				readOnly = append(readOnly, path[0])
			}
		}
	}
	if len(readOnly) > 0 {
		sort.Strings(readOnly)
		return kerrors.Validation("%s assigns %s itself; remove it from properties",
			n.typeName, strings.Join(readOnly, ", "))
	}
	if n.stampTag != nil && hasIdentityTag(properties, n.facts) {
		return kerrors.Validation("the %s tag is kraai's own; remove it from %s", identityTagKey, n.facts.TagProperty)
	}

	desired := properties
	if n.lookup == resource.LookupByName && n.facts.IdentityProperty != "" {
		desired[n.facts.IdentityProperty] = spec.Name
	}
	if n.stampTag != nil {
		n.stampTag(desired, spec.Name)
	}
	validator, err := n.propertyValidator(context.Background())
	if err != nil {
		return err
	}
	return validator.ValidateIgnoring(desired, res.unknown)
}

// checkReferencedProperties requires the first property each reference
// names to be one the referenced resource's type defines, so a misspelled
// attribute fails at plan even before the resource it names exists. A
// reference to a resource that is not an AWS type is not checked here.
func (n *nativeResource) checkReferencedProperties(spec resource.Spec, refs []reference) error {
	if n.schemas == nil {
		return nil
	}
	for _, ref := range refs {
		vendorType, ok := awsVendorType(spec.References[ref.binding])
		if !ok {
			continue
		}
		target, err := n.schemas.PropertySchema(context.Background(), vendorType)
		if err != nil {
			return err
		}
		if !target.HasProperty(ref.path[0]) {
			return kerrors.Validation(
				"a value names %s.%s, but %s (%s) has no property %s",
				ref.binding, strings.Join(ref.path, "."), ref.binding, vendorType, ref.path[0])
		}
	}
	return nil
}

// awsVendorType is the CloudFormation type an aws registry key drives: the
// key's first three "::" segments, which drops a role suffix such as
// "::Native" or "::ArtifactBucket".
func awsVendorType(refKey string) (string, bool) {
	typ, ok := strings.CutPrefix(refKey, Provider+"/")
	if !ok {
		return "", false
	}
	segments := strings.Split(typ, "::")
	if len(segments) < 3 || segments[0] != "AWS" {
		return "", false
	}
	return strings.Join(segments[:3], "::"), true
}

// propertyValidator returns the type's cached validator, building it on
// first use.
func (n *nativeResource) propertyValidator(ctx context.Context) (*resource.Schema, error) {
	n.validatorMu.Lock()
	defer n.validatorMu.Unlock()
	if n.validator != nil {
		return n.validator, nil
	}
	if n.schemas == nil {
		return nil, kerrors.Validation("no schema source to validate %s properties against", n.typeName)
	}
	validator, err := n.schemas.PropertySchema(ctx, n.typeName)
	if err != nil {
		return nil, err
	}
	n.validator = validator
	return validator, nil
}

// hasIdentityTag reports whether properties already carry kraai's identity
// tag under the type's tag property.
func hasIdentityTag(properties map[string]any, facts cfschema.Facts) bool {
	switch facts.TagShape {
	case cfschema.TagShapeArray:
		tags, _ := properties[facts.TagProperty].([]any)
		for _, t := range tags {
			if m, ok := t.(map[string]any); ok && m["Key"] == identityTagKey {
				return true
			}
		}
	case cfschema.TagShapeMap:
		tags, _ := properties[facts.TagProperty].(map[string]any)
		_, ok := tags[identityTagKey]
		return ok
	case cfschema.TagShapeNone:
	}
	return false
}

// arnProperty is the read-only property holding an instance's ARN: Arn, or
// else the one read-only property whose name ends in Arn (TopicArn).
func arnProperty(facts cfschema.Facts) (string, bool) {
	var candidates []string
	for _, pointer := range facts.ReadOnly {
		path := schemaPropertyPath(pointer)
		if len(path) != 1 {
			continue
		}
		if path[0] == "Arn" {
			return "Arn", true
		}
		if strings.HasSuffix(path[0], "Arn") {
			candidates = append(candidates, path[0])
		}
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return "", false
}

// validateMatch holds an entry's declared match properties to what makes
// them find one instance reliably. An untaggable type must declare them,
// and no other may. Each must be a property the entry sets and a read
// returns: a write-only one would never match, and every apply would create
// another copy. A match value naming another binding may only name a
// property that binding's update cannot change, so a value not known at
// plan means a producer being created or replaced, whose new identifier
// cannot match anything that exists.
func (n *nativeResource) validateMatch(spec resource.Spec, properties map[string]any) error {
	keys := matchKeys(spec)
	if n.lookup != resource.LookupByAttr {
		if len(keys) > 0 {
			return kerrors.Validation("%s is found by its %s; match applies only to a type that can be neither named nor tagged",
				n.typeName, map[resource.LookupStrategy]string{resource.LookupByName: "name", resource.LookupByTag: "tag"}[n.lookup])
		}
		return nil
	}
	if len(keys) == 0 {
		return kerrors.Validation(
			"%s can be neither named nor tagged: declare match, the properties whose values pick out this instance",
			n.typeName)
	}
	for _, key := range keys {
		pointer := "/properties/" + key
		value, set := properties[key]
		switch {
		case !set:
			return kerrors.Validation("match names %s, which properties does not set", key)
		case slices.Contains(n.facts.ReadOnly, pointer):
			return kerrors.Validation("match names %s, which %s assigns itself", key, n.typeName)
		case slices.Contains(n.facts.WriteOnly, pointer):
			return kerrors.Validation("match names %s, which a read of %s never returns, so it could never match", key, n.typeName)
		}
		var refs []reference
		walkStrings(value, func(s string) {
			for _, m := range referencePattern.FindAllStringSubmatch(s, -1) {
				if m[1] != "" {
					refs = append(refs, reference{binding: m[1], path: strings.Split(m[2], ".")})
				}
			}
		})
		for _, ref := range refs {
			if producer, ok := spec.References[ref.binding]; ok && !unchangedByUpdate(producer, ref.path[0]) {
				return kerrors.Validation(
					"match %s names %s.%s, which an update of %s can change; name a property it cannot, such as its identifier",
					key, ref.binding, strings.Join(ref.path, "."), ref.binding)
			}
		}
	}
	return nil
}
