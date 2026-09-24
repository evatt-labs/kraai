package aws

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeSSMParameter is AWS::SSM::Parameter's CloudFormation TypeName, used
// here only as a vendor-type label: a SecureString parameter cannot be
// created through Cloud Control or CloudFormation, so nothing in this file
// ever calls it. See TypeSecretParameter.
const TypeSSMParameter = "AWS::SSM::Parameter"

// TypeSecretParameter is this registry's key for a secrets binding's own
// parameters: the "Secret" role AWS::SSM::Parameter plays here, the same
// pattern TypeArtifactBucket uses for a bucket that is not the objects
// capability's. Kept distinct so a future Cloud-Control-backed String or
// StringList parameter type could register the bare vendor type without
// colliding with this one.
var TypeSecretParameter = resource.RoleType(TypeSSMParameter, "Secret")

// SecretsProviderSSM is the one value a secrets binding's `provider` key
// accepts in this slice; secretsBindingSchema's enum names it, and
// bindingSecretsProviderIs gates this package's one registration on it, the
// same two-layer pattern a database binding's `driver` uses. Exported so
// internal/assemble can recognize an aws-ssm secrets binding without
// duplicating the literal.
const SecretsProviderSSM = "aws-ssm"

// The values a `generate` entry's `encoding` key accepts.
const (
	secretEncodingBase64 = "base64"
	secretEncodingHex    = "hex"
)

// secretSourceExternal is the one value a `source` entry accepts: kraai
// creates the parameter with a random placeholder value, and the operator
// sets the real one with `kraai secret set`.
const secretSourceExternal = "external"

// secretGenerateBytesMin and secretGenerateBytesMax bound a `generate`
// entry's `bytes`: enough for a real key even in hex (16 bytes is 128 bits),
// and small enough that a manifest typo does not ask for an unreasonably
// large SecureString. SSM's own parameter value ceiling is far higher.
const (
	secretGenerateBytesMin = 16
	secretGenerateBytesMax = 1024
)

// secretEntryNamePattern constrains an entry's key to what an environment
// variable name, and `kraai secret set`'s BINDING.entry argument, can carry
// without ambiguity: a leading letter or underscore, then letters, digits
// and underscores. No dots, so BINDING.entry can always be split on the
// first one.
const secretEntryNamePattern = `^[A-Za-z_][A-Za-z0-9_]*$` //nolint:gosec // a regex pattern, not a credential

// externalPlaceholderBytes sizes the random value kraai writes for a
// `source: external` entry before the operator sets the real one: enough
// that the placeholder itself could never plausibly be mistaken for a real
// low-entropy secret if it leaked before being overwritten.
const externalPlaceholderBytes = 32

// secretEntryTagKey is the tag a secrets binding's own parameters carry
// their bare entry name under (SecretProducer's map key, "pepper_key" in
// envSecrets' "SECRETS.pepper_key"). Ref.Name is the derived path
// (internal/naming's Namer.Entry), which is lossy to reverse — slugify
// lowercases and collapses characters a manifest's entry name may not have
// — so Get reads the entry name back from this tag rather than parsing it
// out of the name.
const secretEntryTagKey = "kraai:secret-entry" //nolint:gosec // a tag key, not a credential

// bindingSecretsProviderIs is satisfied when the binding entry's provider is
// want, the same pattern bindingDriverIs uses for a database binding's
// driver.
func bindingSecretsProviderIs(want string) resource.Applicability {
	return func(ctx resource.ApplicabilityContext) bool {
		declared, _ := ctx.Binding["provider"].(string)
		return declared == want
	}
}

// secretEntrySpec is one entry's decoded desired state: its bare name, and
// either how to generate its value or that its value comes from outside
// kraai.
type secretEntrySpec struct {
	Entry    string
	Generate *secretGenerateSpec
	External bool
}

// secretGenerateSpec is a `generate` entry's decoded desired state.
type secretGenerateSpec struct {
	Bytes    int
	Encoding string
}

// decodeSecretEntrySpec reads one fanned-out secrets item's Spec.Config:
// the entry name expandEntries always sets, and exactly one of generate or
// source, which secretsBindingSchema cannot enforce (see secretEntrySchema)
// and so is checked here instead. Every other shape secretsBindingSchema
// already guarantees by the time this runs; the type assertions below
// still fail closed rather than panic if that ever stops being true.
func decodeSecretEntrySpec(spec resource.Spec) (secretEntrySpec, error) {
	entry, _ := spec.Config["entry"].(string)
	if entry == "" {
		return secretEntrySpec{}, kerrors.Validation(
			"binding %q: secret entry carries no entry name", spec.Binding)
	}

	generate, hasGenerate := spec.Config["generate"]
	source, hasSource := spec.Config["source"]
	switch {
	case hasGenerate && hasSource:
		return secretEntrySpec{}, kerrors.Validation(
			"binding %q entry %q: declares both generate and source; exactly one is required", spec.Binding, entry)
	case !hasGenerate && !hasSource:
		return secretEntrySpec{}, kerrors.Validation(
			"binding %q entry %q: declares neither generate nor source; exactly one is required", spec.Binding, entry)
	case hasGenerate:
		raw, _ := generate.(map[string]any)
		bytesVal, err := decodeSecretBytes(raw)
		if err != nil {
			return secretEntrySpec{}, kerrors.Wrap(err, kerrors.CodeValidation, "binding %q entry %q", spec.Binding, entry)
		}
		encoding, _ := raw["encoding"].(string)
		if encoding != secretEncodingBase64 && encoding != secretEncodingHex {
			return secretEntrySpec{}, kerrors.Validation(
				"binding %q entry %q: generate.encoding must be %q or %q, got %q",
				spec.Binding, entry, secretEncodingBase64, secretEncodingHex, encoding)
		}
		return secretEntrySpec{Entry: entry, Generate: &secretGenerateSpec{Bytes: bytesVal, Encoding: encoding}}, nil
	default:
		str, _ := source.(string)
		if str != secretSourceExternal {
			return secretEntrySpec{}, kerrors.Validation(
				"binding %q entry %q: source must be %q, got %q", spec.Binding, entry, secretSourceExternal, str)
		}
		return secretEntrySpec{Entry: entry, External: true}, nil
	}
}

func decodeSecretBytes(raw map[string]any) (int, error) {
	switch v := raw["bytes"].(type) {
	case int:
		return v, nil
	case float64:
		return int(v), nil
	default:
		return 0, kerrors.Validation("generate.bytes must be an integer, got %T", raw["bytes"])
	}
}

// secretValue produces the value Create writes: CSPRNG-generated bytes in
// the declared encoding for a generate entry, or a CSPRNG-generated
// placeholder for an external one. Neither is ever produced again after
// Create — this is the one place in this file that reads crypto/rand.
func secretValue(es secretEntrySpec) (string, error) {
	if es.Generate != nil {
		return randomEncoded(es.Generate.Bytes, es.Generate.Encoding)
	}
	return randomEncoded(externalPlaceholderBytes, secretEncodingBase64)
}

func randomEncoded(n int, encoding string) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "generating a secret value")
	}
	switch encoding {
	case secretEncodingHex:
		return hex.EncodeToString(buf), nil
	default:
		return base64.StdEncoding.EncodeToString(buf), nil
	}
}

// secretParameterResource provisions one SSM SecureString parameter per
// entry of a secrets binding declaring provider aws-ssm.
//
// Unlike every other type in this package, it never calls Cloud Control:
// Cloud Control and CloudFormation cannot create a SecureString parameter,
// so every verb here calls the SSM API directly. Get, Diff and Update never
// read or write a parameter's Value — the value is never this type's
// desired state, only Create's one-time output — so the value a manifest
// author wrote (nothing) and the value SSM holds can never be compared,
// only the parameter's existence and its entry-name tag.
type secretParameterResource struct {
	client *Client
}

func newSecretParameterResource(client *Client) *secretParameterResource {
	return &secretParameterResource{client: client}
}

// Get reads a parameter's identity and metadata — never its value.
// DescribeParameters, not GetParameter: GetParameter's response carries a
// Value field even when WithDecryption is false (the ciphertext), and this
// type must never hold that, not even encrypted, outside the one call
// Secrets' producer makes at the moment of use.
//
// foreign resource in a globally unique namespace (S3 bucket names); an SSM
// parameter name is unique per account and region, so no other account can
// ever hold the name this type derives, and there is no "owns" question to
// ask. The entry-tag check below is this type's own, narrower ownership
// check: not "is this ours", but "which entry is this".
//
//nolint:gocritic // this package's byName ownership rule guards against a
func (r *secretParameterResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	out, err := r.client.ssm.DescribeParameters(ctx, &ssm.DescribeParametersInput{
		ParameterFilters: []ssmtypes.ParameterStringFilter{
			{Key: aws.String("Name"), Option: aws.String("Equals"), Values: []string{ref.Name}},
		},
	})
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "describing SSM parameter %q", ref.Name)
	}
	if len(out.Parameters) == 0 {
		return nil, nil
	}
	meta := out.Parameters[0]

	entry, err := r.entryTag(ctx, ref.Name)
	if err != nil {
		return nil, err
	}
	if entry == "" {
		return nil, kerrors.Validation(
			"SSM parameter %q exists but carries no %q tag, so kraai cannot tell which secrets "+
				"binding entry it belongs to — it was not created by kraai at this name; remove it or "+
				"choose a different binding or entry name", ref.Name, secretEntryTagKey)
	}
	if meta.Type != ssmtypes.ParameterTypeSecureString {
		return nil, kerrors.Validation(
			"SSM parameter %q is type %q, not SecureString; kraai cannot reconcile that without writing "+
				"a value, so it must be fixed or removed by hand", ref.Name, meta.Type)
	}

	return &resource.State{
		Ref: ref,
		ID:  ref.Name,
		Attributes: map[string]any{
			"Type":    string(meta.Type),
			"Version": meta.Version,
			"Entry":   entry,
		},
	}, nil
}

// entryTag reads the entry name back from secretEntryTagKey, empty when the
// parameter carries none.
func (r *secretParameterResource) entryTag(ctx context.Context, name string) (string, error) {
	out, err := r.client.ssm.ListTagsForResource(ctx, &ssm.ListTagsForResourceInput{
		ResourceType: ssmtypes.ResourceTypeForTaggingParameter,
		ResourceId:   aws.String(name),
	})
	if err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "reading tags for SSM parameter %q", name)
	}
	for _, tag := range out.TagList {
		if aws.ToString(tag.Key) == secretEntryTagKey {
			return aws.ToString(tag.Value), nil
		}
	}
	return "", nil
}

// ValidateSpec implements plan.SpecValidator: the one check
// secretsBindingSchema cannot express (see secretEntrySchema), run with no
// I/O so a fresh environment's first plan still catches it.
func (r *secretParameterResource) ValidateSpec(spec resource.Spec) error {
	_, err := decodeSecretEntrySpec(spec)
	return err
}

// Diff never reports Immutable: nothing about a secret entry's identity can
// change without becoming a different Ref (a different derived name), so
// there is nothing here a replace could ever be the right answer for, and a
// replace would delete a value no later apply could ever restore. Type
// drift away from SecureString is reported as a validation failure instead
// of a reconciliation, because reconciling it would need PutParameter,
// which needs a Value this type must never hold. The only Mutable case is
// the entry-name tag itself, which Update can fix without touching Value.
func (r *secretParameterResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	if state == nil {
		return resource.Same, nil
	}
	if typ, _ := state.Attributes["Type"].(string); typ != string(ssmtypes.ParameterTypeSecureString) {
		return resource.Same, kerrors.Validation(
			"SSM parameter %q is type %q, not SecureString; kraai cannot reconcile that without writing "+
				"a value, so it must be fixed or removed by hand", state.Ref.Name, typ)
	}
	entry, _ := spec.Config["entry"].(string)
	existing, _ := state.Attributes["Entry"].(string)
	if entry == "" || entry == existing {
		return resource.Same, nil
	}
	return resource.Mutable, nil
}

// Create writes the entry's value exactly once: a CSPRNG-generated value for
// a generate entry, or a CSPRNG-generated placeholder for an external one,
// which `kraai secret set` later overwrites. Overwrite is never set, so a
// name that already exists fails the call rather than silently replacing
// whatever value is already there — the one place this type could break its
// own invariant if that ever changed. Tags ride in this same call: SSM
// refuses Tags together with Overwrite, and Create never sets Overwrite, so
// there is no conflict and no follow-up write to orphan if this call is
// interrupted after creating but before tagging.
func (r *secretParameterResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	es, err := decodeSecretEntrySpec(spec)
	if err != nil {
		return nil, err
	}
	value, err := secretValue(es)
	if err != nil {
		return nil, err
	}

	_, err = r.client.ssm.PutParameter(ctx, &ssm.PutParameterInput{
		Name:  aws.String(spec.Name),
		Type:  ssmtypes.ParameterTypeSecureString,
		Value: aws.String(value),
		Tags:  []ssmtypes.Tag{{Key: aws.String(secretEntryTagKey), Value: aws.String(es.Entry)}},
	})
	if err != nil {
		if parameterAlreadyExists(err) {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation,
				"SSM parameter %q already exists; kraai never overwrites an existing secret's value", spec.Name)
		}
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "creating SSM parameter %q", spec.Name)
	}

	return &resource.State{
		Ref: resource.Ref{Provider: Provider, Type: TypeSecretParameter, Name: spec.Name},
		ID:  spec.Name,
		Attributes: map[string]any{
			"Type":  string(ssmtypes.ParameterTypeSecureString),
			"Entry": es.Entry,
		},
	}, nil
}

// Update never calls PutParameter — the only call that can write a Value —
// so no code path here can ever write one. The entry-name tag is the only
// thing Update reconciles; see Diff for when that is reachable at all.
func (r *secretParameterResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	es, err := decodeSecretEntrySpec(spec)
	if err != nil {
		return nil, err
	}
	_, err = r.client.ssm.AddTagsToResource(ctx, &ssm.AddTagsToResourceInput{
		ResourceType: ssmtypes.ResourceTypeForTaggingParameter,
		ResourceId:   aws.String(ref.Name),
		Tags:         []ssmtypes.Tag{{Key: aws.String(secretEntryTagKey), Value: aws.String(es.Entry)}},
	})
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "tagging SSM parameter %q", ref.Name)
	}
	return &resource.State{
		Ref:        ref,
		ID:         ref.Name,
		Attributes: map[string]any{"Type": string(ssmtypes.ParameterTypeSecureString), "Entry": es.Entry},
	}, nil
}

// Delete removes the parameter. A parameter already gone is success, the
// same contract as every other type here.
func (r *secretParameterResource) Delete(ctx context.Context, ref resource.Ref) error {
	_, err := r.client.ssm.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(ref.Name)})
	if err != nil && !parameterNotFound(err) {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting SSM parameter %q", ref.Name)
	}
	return nil
}

// Secrets implements resource.SecretProducer: the entry's value, read with
// decryption only when a consumer resolves it, never before and never
// twice into anything held. The one place this type ever reads a parameter's
// Value at all.
func (r *secretParameterResource) Secrets(state *resource.State) map[string]resource.Secret {
	if state == nil {
		return nil
	}
	entry, _ := state.Attributes["Entry"].(string)
	if entry == "" {
		return nil
	}
	name := state.Ref.Name
	client := r.client
	return map[string]resource.Secret{
		entry: func(ctx context.Context) (string, error) {
			out, err := client.ssm.GetParameter(ctx, &ssm.GetParameterInput{
				Name: aws.String(name), WithDecryption: aws.Bool(true),
			})
			if err != nil {
				return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "reading SSM parameter %q", name)
			}
			if out.Parameter == nil || out.Parameter.Value == nil || *out.Parameter.Value == "" {
				return "", kerrors.Validation("SSM parameter %q has no value", name)
			}
			return *out.Parameter.Value, nil
		},
	}
}

// SetSecretParameter overwrites an existing SecureString parameter's value
// under the caller's own credentials: the write side of `kraai secret set`.
// It never creates a parameter — Overwrite is always set, and SSM's
// PutParameter refuses Tags together with Overwrite, so a parameter this
// call creates fresh would carry no secretEntryTagKey and become
// unmanageable the moment a later plan tried to Get it. Refusing to create
// one keeps that tag, written once at Create, the only way a parameter this
// package manages ever gets it.
func (c *Client) SetSecretParameter(ctx context.Context, name, value string) error {
	described, err := c.ssm.DescribeParameters(ctx, &ssm.DescribeParametersInput{
		ParameterFilters: []ssmtypes.ParameterStringFilter{
			{Key: aws.String("Name"), Option: aws.String("Equals"), Values: []string{name}},
		},
	})
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "describing SSM parameter %q", name)
	}
	if len(described.Parameters) == 0 {
		return kerrors.Validation(
			"SSM parameter %q does not exist yet; run `kraai apply` first to create it", name)
	}

	_, err = c.ssm.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(name),
		Type:      ssmtypes.ParameterTypeSecureString,
		Value:     aws.String(value),
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "setting SSM parameter %q", name)
	}
	return nil
}

// parameterAlreadyExists and parameterNotFound recognize SSM's error codes
// for a name collision on create and an absent parameter on delete.
func parameterAlreadyExists(err error) bool {
	var already *ssmtypes.ParameterAlreadyExists
	return errors.As(err, &already)
}

func parameterNotFound(err error) bool {
	var notFound *ssmtypes.ParameterNotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "ParameterNotFound"
}
