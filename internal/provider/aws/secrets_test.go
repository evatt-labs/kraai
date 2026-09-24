package aws

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

func generateSpec(entry string, bytesN int, encoding string) resource.Spec {
	return resource.Spec{
		Binding: "SECRETS", Name: "/dev/api/secrets/" + entry,
		Config: map[string]any{
			"entry":    entry,
			"generate": map[string]any{"bytes": bytesN, "encoding": encoding},
		},
	}
}

func externalSpec(entry string) resource.Spec {
	return resource.Spec{
		Binding: "SECRETS", Name: "/dev/api/secrets/" + entry,
		Config: map[string]any{"entry": entry, "source": "external"},
	}
}

// --- decodeSecretEntrySpec -------------------------------------------------

func TestDecodeSecretEntrySpec(t *testing.T) {
	t.Run("generate", func(t *testing.T) {
		es, err := decodeSecretEntrySpec(generateSpec("pepper_key", 32, "base64"))
		if err != nil {
			t.Fatalf("decodeSecretEntrySpec: %v", err)
		}
		if es.Entry != "pepper_key" || es.Generate == nil || es.Generate.Bytes != 32 || es.Generate.Encoding != "base64" {
			t.Fatalf("decoded = %+v", es)
		}
	})

	t.Run("external", func(t *testing.T) {
		es, err := decodeSecretEntrySpec(externalSpec("github_client_secret"))
		if err != nil {
			t.Fatalf("decodeSecretEntrySpec: %v", err)
		}
		if es.Entry != "github_client_secret" || !es.External || es.Generate != nil {
			t.Fatalf("decoded = %+v", es)
		}
	})

	t.Run("both generate and source is refused", func(t *testing.T) {
		spec := generateSpec("x", 32, "base64")
		spec.Config["source"] = "external"
		if _, err := decodeSecretEntrySpec(spec); err == nil {
			t.Fatal("declaring both generate and source was accepted")
		}
	})

	t.Run("neither generate nor source is refused", func(t *testing.T) {
		spec := resource.Spec{Binding: "SECRETS", Config: map[string]any{"entry": "x"}}
		if _, err := decodeSecretEntrySpec(spec); err == nil {
			t.Fatal("declaring neither generate nor source was accepted")
		}
	})

	t.Run("no entry name is refused", func(t *testing.T) {
		spec := resource.Spec{Binding: "SECRETS", Config: map[string]any{"source": "external"}}
		if _, err := decodeSecretEntrySpec(spec); err == nil {
			t.Fatal("a spec with no entry name was accepted")
		}
	})

	t.Run("source other than external is refused", func(t *testing.T) {
		spec := resource.Spec{Binding: "SECRETS", Config: map[string]any{"entry": "x", "source": "vault"}}
		if _, err := decodeSecretEntrySpec(spec); err == nil {
			t.Fatal("source: vault was accepted")
		}
	})

	t.Run("encoding other than base64 or hex is refused", func(t *testing.T) {
		spec := generateSpec("x", 32, "rot13")
		if _, err := decodeSecretEntrySpec(spec); err == nil {
			t.Fatal("an unknown encoding was accepted")
		}
	})

	t.Run("bytes of the wrong type is refused", func(t *testing.T) {
		spec := resource.Spec{Binding: "SECRETS", Config: map[string]any{
			"entry": "x", "generate": map[string]any{"bytes": "32", "encoding": "hex"},
		}}
		if _, err := decodeSecretEntrySpec(spec); err == nil {
			t.Fatal("a string bytes value was accepted")
		}
	})

	t.Run("float64 bytes (a JSON --set round trip) decodes", func(t *testing.T) {
		spec := resource.Spec{Binding: "SECRETS", Config: map[string]any{
			"entry": "x", "generate": map[string]any{"bytes": float64(32), "encoding": "hex"},
		}}
		es, err := decodeSecretEntrySpec(spec)
		if err != nil || es.Generate.Bytes != 32 {
			t.Fatalf("decodeSecretEntrySpec = %+v, %v", es, err)
		}
	})
}

// ValidateSpec implements plan.SpecValidator by delegating to
// decodeSecretEntrySpec; this is the one path secretsBindingSchema cannot
// cover (see secretEntrySchema).
func TestSecretParameterResource_ValidateSpec(t *testing.T) {
	r := newSecretParameterResource(&Client{})
	if err := r.ValidateSpec(generateSpec("x", 32, "base64")); err != nil {
		t.Errorf("ValidateSpec(generate) = %v, want nil", err)
	}
	spec := generateSpec("x", 32, "base64")
	spec.Config["source"] = "external"
	if err := r.ValidateSpec(spec); err == nil {
		t.Error("ValidateSpec accepted both generate and source")
	}
}

// --- secretValue -------------------------------------------------------

func TestSecretValue_Generate(t *testing.T) {
	for _, c := range []struct {
		encoding string
		decode   func(string) ([]byte, error)
	}{
		{secretEncodingBase64, base64.StdEncoding.DecodeString},
		{secretEncodingHex, hex.DecodeString},
	} {
		t.Run(c.encoding, func(t *testing.T) {
			value, err := secretValue(secretEntrySpec{Entry: "x", Generate: &secretGenerateSpec{Bytes: 32, Encoding: c.encoding}})
			if err != nil {
				t.Fatalf("secretValue: %v", err)
			}
			decoded, err := c.decode(value)
			if err != nil {
				t.Fatalf("value %q does not decode as %s: %v", value, c.encoding, err)
			}
			if len(decoded) != 32 {
				t.Errorf("decoded length = %d, want 32", len(decoded))
			}
		})
	}
}

func TestSecretValue_ExternalIsARandomPlaceholder(t *testing.T) {
	a, err := secretValue(secretEntrySpec{Entry: "x", External: true})
	if err != nil {
		t.Fatalf("secretValue: %v", err)
	}
	b, err := secretValue(secretEntrySpec{Entry: "x", External: true})
	if err != nil {
		t.Fatalf("secretValue: %v", err)
	}
	if a == b {
		t.Fatal("two placeholder values were identical; want independently random values")
	}
	if a == "" {
		t.Fatal("placeholder value is empty")
	}
}

// --- Create ----------------------------------------------------------------

func TestSecretParameterResource_Create(t *testing.T) {
	f := &fakeSSM{}
	r := newSecretParameterResource(&Client{ssm: f})

	state, err := r.Create(context.Background(), generateSpec("pepper_key", 32, "base64"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if state.Ref.Name != "/dev/api/secrets/pepper_key" || state.Attributes["Entry"] != "pepper_key" {
		t.Fatalf("state = %+v", state)
	}

	if len(f.putParameterIn) != 1 {
		t.Fatalf("PutParameter called %d times, want 1", len(f.putParameterIn))
	}
	put := f.putParameterIn[0]
	if put.Type != ssmtypes.ParameterTypeSecureString {
		t.Errorf("Type = %s, want SecureString", put.Type)
	}
	if aws.ToString(put.Value) == "" {
		t.Error("Value is empty")
	}
	if put.Overwrite != nil && *put.Overwrite {
		t.Error("Overwrite was set on Create; a create must never overwrite an existing value")
	}
	if len(put.Tags) != 1 || aws.ToString(put.Tags[0].Key) != secretEntryTagKey || aws.ToString(put.Tags[0].Value) != "pepper_key" {
		t.Errorf("Tags = %+v, want one secretEntryTagKey=pepper_key tag", put.Tags)
	}
}

func TestSecretParameterResource_Create_AlreadyExistsIsAValidationError(t *testing.T) {
	f := &fakeSSM{putParameterErr: &ssmtypes.ParameterAlreadyExists{}}
	r := newSecretParameterResource(&Client{ssm: f})

	_, err := r.Create(context.Background(), externalSpec("x"))
	if err == nil {
		t.Fatal("Create succeeded over an existing parameter, want an error")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
		t.Errorf("error = %v, want a CodeValidation *KError", err)
	}
	if !strings.Contains(err.Error(), "never overwrites") {
		t.Errorf("error %q does not explain the invariant", err)
	}
}

func TestSecretParameterResource_Create_DecodeErrorPropagates(t *testing.T) {
	f := &fakeSSM{}
	r := newSecretParameterResource(&Client{ssm: f})
	spec := resource.Spec{Binding: "SECRETS", Config: map[string]any{}} // no entry name
	if _, err := r.Create(context.Background(), spec); err == nil {
		t.Fatal("Create succeeded with no entry name, want an error")
	}
	if len(f.putParameterIn) != 0 {
		t.Fatal("PutParameter was called despite the decode failing")
	}
}

func TestSecretParameterResource_Create_UnexpectedErrorIsWrapped(t *testing.T) {
	f := &fakeSSM{putParameterErr: errors.New("throttled")}
	r := newSecretParameterResource(&Client{ssm: f})

	_, err := r.Create(context.Background(), externalSpec("x"))
	if err == nil {
		t.Fatal("Create succeeded, want the throttled error")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeUnexpected {
		t.Errorf("error = %v, want a CodeUnexpected *KError", err)
	}
}

// --- Get ---------------------------------------------------------------

func TestSecretParameterResource_Get_NotFound(t *testing.T) {
	f := &fakeSSM{}
	r := newSecretParameterResource(&Client{ssm: f})

	state, err := r.Get(context.Background(), resource.Ref{Name: "/dev/api/secrets/x"})
	if err != nil || state != nil {
		t.Fatalf("Get = %+v, %v, want nil, nil", state, err)
	}
}

func TestSecretParameterResource_Get_Found(t *testing.T) {
	f := &fakeSSM{
		describeParameters: []ssmtypes.ParameterMetadata{{Type: ssmtypes.ParameterTypeSecureString, Version: 1}},
		tags:               []ssmtypes.Tag{{Key: aws.String(secretEntryTagKey), Value: aws.String("pepper_key")}},
	}
	r := newSecretParameterResource(&Client{ssm: f})

	ref := resource.Ref{Name: "/dev/api/secrets/pepper_key"}
	state, err := r.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil || state.Attributes["Entry"] != "pepper_key" {
		t.Fatalf("state = %+v", state)
	}
	if state.Attributes["Type"] != string(ssmtypes.ParameterTypeSecureString) {
		t.Errorf("Type = %v", state.Attributes["Type"])
	}
}

func TestSecretParameterResource_Get_NoEntryTagIsRefused(t *testing.T) {
	f := &fakeSSM{
		describeParameters: []ssmtypes.ParameterMetadata{{Type: ssmtypes.ParameterTypeSecureString, Version: 1}},
		// No tags: not a parameter kraai created at this name.
	}
	r := newSecretParameterResource(&Client{ssm: f})

	_, err := r.Get(context.Background(), resource.Ref{Name: "/dev/api/secrets/pepper_key"})
	if err == nil {
		t.Fatal("Get succeeded over an untagged parameter, want an error")
	}
}

func TestSecretParameterResource_Get_WrongTypeIsRefused(t *testing.T) {
	f := &fakeSSM{
		describeParameters: []ssmtypes.ParameterMetadata{{Type: ssmtypes.ParameterTypeString, Version: 1}},
		tags:               []ssmtypes.Tag{{Key: aws.String(secretEntryTagKey), Value: aws.String("pepper_key")}},
	}
	r := newSecretParameterResource(&Client{ssm: f})

	_, err := r.Get(context.Background(), resource.Ref{Name: "/dev/api/secrets/pepper_key"})
	if err == nil {
		t.Fatal("Get succeeded over a non-SecureString parameter, want an error")
	}
}

// TestSecretParameterResource_Get_NeverDecrypts is the plan-never-decrypts
// invariant: reading a parameter's identity and metadata, the only thing a
// plan does, must never call GetParameter (the one call that can return a
// value) at all — not even with WithDecryption false, which still carries
// the ciphertext. Broken deliberately and restored to confirm this test can
// fail; see the PR description for the failure output.
func TestSecretParameterResource_Get_NeverDecrypts(t *testing.T) {
	f := &fakeSSM{
		describeParameters: []ssmtypes.ParameterMetadata{{Type: ssmtypes.ParameterTypeSecureString, Version: 1}},
		tags:               []ssmtypes.Tag{{Key: aws.String(secretEntryTagKey), Value: aws.String("pepper_key")}},
	}
	r := newSecretParameterResource(&Client{ssm: f})

	if _, err := r.Get(context.Background(), resource.Ref{Name: "/dev/api/secrets/pepper_key"}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(f.inputs) != 0 {
		t.Fatalf("Get called GetParameter %d times, want 0: plan must never read a value", len(f.inputs))
	}
}

func TestSecretParameterResource_Get_DescribeParametersErrorPropagates(t *testing.T) {
	f := &fakeSSM{describeParametersErr: errors.New("throttled")}
	r := newSecretParameterResource(&Client{ssm: f})
	if _, err := r.Get(context.Background(), resource.Ref{Name: "/dev/api/secrets/x"}); err == nil {
		t.Fatal("Get succeeded, want the DescribeParameters error")
	}
}

func TestSecretParameterResource_Get_ListTagsErrorPropagates(t *testing.T) {
	f := &fakeSSM{
		describeParameters: []ssmtypes.ParameterMetadata{{Type: ssmtypes.ParameterTypeSecureString}},
		listTagsErr:        errors.New("throttled"),
	}
	r := newSecretParameterResource(&Client{ssm: f})
	if _, err := r.Get(context.Background(), resource.Ref{Name: "/dev/api/secrets/x"}); err == nil {
		t.Fatal("Get succeeded, want the ListTagsForResource error")
	}
}

// --- Diff --------------------------------------------------------------

func TestSecretParameterResource_Diff(t *testing.T) {
	r := newSecretParameterResource(&Client{})

	t.Run("no state is Same", func(t *testing.T) {
		diff, err := r.Diff(externalSpec("x"), nil)
		if err != nil || diff != resource.Same {
			t.Fatalf("Diff(nil) = %v, %v, want Same, nil", diff, err)
		}
	})

	t.Run("matching entry is Same", func(t *testing.T) {
		state := &resource.State{Attributes: map[string]any{
			"Type": string(ssmtypes.ParameterTypeSecureString), "Entry": "pepper_key",
		}}
		diff, err := r.Diff(generateSpec("pepper_key", 32, "base64"), state)
		if err != nil || diff != resource.Same {
			t.Fatalf("Diff = %v, %v, want Same, nil", diff, err)
		}
	})

	t.Run("drifted entry tag is Mutable, not Immutable", func(t *testing.T) {
		state := &resource.State{Attributes: map[string]any{
			"Type": string(ssmtypes.ParameterTypeSecureString), "Entry": "someone-changed-this",
		}}
		diff, err := r.Diff(generateSpec("pepper_key", 32, "base64"), state)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if diff != resource.Mutable {
			t.Fatalf("Diff = %v, want Mutable", diff)
		}
	})

	t.Run("wrong type is an error, not a reconciliation", func(t *testing.T) {
		state := &resource.State{Attributes: map[string]any{
			"Type": string(ssmtypes.ParameterTypeString), "Entry": "pepper_key",
		}}
		diff, err := r.Diff(generateSpec("pepper_key", 32, "base64"), state)
		if err == nil {
			t.Fatal("Diff over a non-SecureString parameter succeeded, want an error")
		}
		if diff == resource.Immutable {
			t.Fatal("Diff returned Immutable, which would replace (delete) the secret")
		}
	})
}

// TestSecretParameterResource_Diff_NeverImmutable is the property this
// type's whole design rests on: whatever spec and state Diff is handed, it
// must never answer Immutable, because a replace deletes the parameter
// first and no later apply can ever restore a value that was never desired
// state. Every branch Diff has is exercised directly rather than through a
// property-testing library, since the input space here is a handful of
// fixed shapes, not an unbounded one.
func TestSecretParameterResource_Diff_NeverImmutable(t *testing.T) {
	r := newSecretParameterResource(&Client{})
	states := []*resource.State{
		nil,
		{Attributes: map[string]any{"Type": string(ssmtypes.ParameterTypeSecureString), "Entry": "pepper_key"}},
		{Attributes: map[string]any{"Type": string(ssmtypes.ParameterTypeSecureString), "Entry": "other"}},
		{Attributes: map[string]any{"Type": string(ssmtypes.ParameterTypeString), "Entry": "pepper_key"}},
		{Attributes: map[string]any{}},
	}
	specs := []resource.Spec{generateSpec("pepper_key", 32, "base64"), externalSpec("pepper_key")}
	for _, spec := range specs {
		for _, state := range states {
			diff, _ := r.Diff(spec, state)
			if diff == resource.Immutable {
				t.Fatalf("Diff(%+v, %+v) = Immutable", spec.Config, state)
			}
		}
	}
}

// --- Update ------------------------------------------------------------

// TestSecretParameterResource_Update_NeverWritesAValue is the never-
// desired-state invariant's direct proof: Update must never call
// PutParameter, the only call capable of writing a Value, under any input.
// Broken deliberately and restored to confirm this test can fail; see the
// PR description for the failure output.
func TestSecretParameterResource_Update_NeverWritesAValue(t *testing.T) {
	f := &fakeSSM{}
	r := newSecretParameterResource(&Client{ssm: f})

	ref := resource.Ref{Name: "/dev/api/secrets/pepper_key"}
	if _, err := r.Update(context.Background(), ref, generateSpec("pepper_key", 32, "base64")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(f.putParameterIn) != 0 {
		t.Fatalf("Update called PutParameter %d times, want 0: Update must never write a value", len(f.putParameterIn))
	}
	if len(f.addTagsIn) != 1 {
		t.Fatalf("Update called AddTagsToResource %d times, want 1", len(f.addTagsIn))
	}
	tags := f.addTagsIn[0].Tags
	if len(tags) != 1 || aws.ToString(tags[0].Key) != secretEntryTagKey || aws.ToString(tags[0].Value) != "pepper_key" {
		t.Errorf("Tags = %+v", tags)
	}
}

func TestSecretParameterResource_Update_DecodeErrorPropagates(t *testing.T) {
	f := &fakeSSM{}
	r := newSecretParameterResource(&Client{ssm: f})
	spec := resource.Spec{Binding: "SECRETS", Config: map[string]any{}} // no entry name
	if _, err := r.Update(context.Background(), resource.Ref{Name: "/dev/api/secrets/x"}, spec); err == nil {
		t.Fatal("Update succeeded with no entry name, want an error")
	}
	if len(f.addTagsIn) != 0 {
		t.Fatal("AddTagsToResource was called despite the decode failing")
	}
}

func TestSecretParameterResource_Update_AddTagsErrorPropagates(t *testing.T) {
	f := &fakeSSM{addTagsErr: errors.New("throttled")}
	r := newSecretParameterResource(&Client{ssm: f})
	_, err := r.Update(context.Background(), resource.Ref{Name: "/dev/api/secrets/x"}, externalSpec("x"))
	if err == nil {
		t.Fatal("Update succeeded, want the AddTagsToResource error")
	}
}

// --- Delete ------------------------------------------------------------

func TestSecretParameterResource_Delete(t *testing.T) {
	f := &fakeSSM{}
	r := newSecretParameterResource(&Client{ssm: f})
	if err := r.Delete(context.Background(), resource.Ref{Name: "/dev/api/secrets/pepper_key"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(f.deleteParameterIn) != 1 || aws.ToString(f.deleteParameterIn[0].Name) != "/dev/api/secrets/pepper_key" {
		t.Fatalf("DeleteParameter calls = %+v", f.deleteParameterIn)
	}
}

func TestSecretParameterResource_Delete_AlreadyGoneIsSuccess(t *testing.T) {
	for label, err := range map[string]error{
		"typed ParameterNotFound":         &ssmtypes.ParameterNotFound{},
		"generic smithy error, same code": &smithy.GenericAPIError{Code: "ParameterNotFound"},
	} {
		t.Run(label, func(t *testing.T) {
			f := &fakeSSM{deleteParameterErr: err}
			r := newSecretParameterResource(&Client{ssm: f})
			if err := r.Delete(context.Background(), resource.Ref{Name: "/dev/api/secrets/pepper_key"}); err != nil {
				t.Fatalf("Delete over an already-gone parameter = %v, want nil", err)
			}
		})
	}
}

func TestSecretParameterResource_Delete_UnexpectedErrorPropagates(t *testing.T) {
	f := &fakeSSM{deleteParameterErr: errors.New("throttled")}
	r := newSecretParameterResource(&Client{ssm: f})
	if err := r.Delete(context.Background(), resource.Ref{Name: "/dev/api/secrets/x"}); err == nil {
		t.Fatal("Delete succeeded, want the throttled error")
	}
}

// --- Secrets (SecretProducer) -------------------------------------------

func TestSecretParameterResource_Secrets_NilState(t *testing.T) {
	r := newSecretParameterResource(&Client{})
	if got := r.Secrets(nil); got != nil {
		t.Errorf("Secrets(nil) = %v, want nil", got)
	}
}

func TestSecretParameterResource_Secrets_NoEntryAttribute(t *testing.T) {
	r := newSecretParameterResource(&Client{})
	state := &resource.State{Ref: resource.Ref{Name: "/dev/api/secrets/x"}, Attributes: map[string]any{}}
	if got := r.Secrets(state); got != nil {
		t.Errorf("Secrets(state with no Entry) = %v, want nil", got)
	}
}

// TestSecretParameterResource_Secrets_LazyAndKeyedByEntry proves two things
// the envSecrets consumer path depends on: the map SecretProducer returns is
// keyed by the entry's bare name (what "SECRETS.pepper_key" resolves
// against, via apply's secretIndex), and building that map never calls
// GetParameter — only invoking the returned producer does.
func TestSecretParameterResource_Secrets_LazyAndKeyedByEntry(t *testing.T) {
	f := &fakeSSM{value: "s3cr3t-value"}
	r := newSecretParameterResource(&Client{ssm: f})

	state := &resource.State{
		Ref:        resource.Ref{Name: "/dev/api/secrets/pepper_key"},
		Attributes: map[string]any{"Entry": "pepper_key"},
	}
	secrets := r.Secrets(state)
	if len(secrets) != 1 {
		t.Fatalf("Secrets = %+v, want exactly one entry", secrets)
	}
	producer, ok := secrets["pepper_key"]
	if !ok {
		t.Fatalf("Secrets keys = %v, want \"pepper_key\"", keys(secrets))
	}
	if len(f.inputs) != 0 {
		t.Fatalf("Secrets() itself called GetParameter %d times, want 0 until the producer is invoked", len(f.inputs))
	}

	value, err := producer(context.Background())
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	if value != "s3cr3t-value" {
		t.Errorf("value = %q", value)
	}
	if len(f.inputs) != 1 {
		t.Fatalf("producer called GetParameter %d times, want exactly 1", len(f.inputs))
	}
	if !aws.ToBool(f.inputs[0].WithDecryption) {
		t.Error("WithDecryption was not set")
	}
}

func TestSecretParameterResource_Secrets_ProducerErrorPropagates(t *testing.T) {
	f := &fakeSSM{err: errors.New("access denied")}
	r := newSecretParameterResource(&Client{ssm: f})
	state := &resource.State{Ref: resource.Ref{Name: "/dev/api/secrets/x"}, Attributes: map[string]any{"Entry": "x"}}
	producer := r.Secrets(state)["x"]
	if _, err := producer(context.Background()); err == nil {
		t.Fatal("producer succeeded, want the GetParameter error")
	}
}

func TestSecretParameterResource_Secrets_EmptyValueIsAnError(t *testing.T) {
	f := &fakeSSM{value: ""}
	r := newSecretParameterResource(&Client{ssm: f})
	state := &resource.State{Ref: resource.Ref{Name: "/dev/api/secrets/x"}, Attributes: map[string]any{"Entry": "x"}}
	producer := r.Secrets(state)["x"]
	if _, err := producer(context.Background()); err == nil {
		t.Fatal("producer succeeded with an empty value, want an error")
	}
}

func keys(m map[string]resource.Secret) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- Client.SetSecretParameter (kraai secret set's write) -----------------

func TestClient_SetSecretParameter(t *testing.T) {
	f := &fakeSSM{describeParameters: []ssmtypes.ParameterMetadata{{Type: ssmtypes.ParameterTypeSecureString}}}
	c := &Client{ssm: f}

	if err := c.SetSecretParameter(context.Background(), "/dev/api/secrets/x", "the-real-value"); err != nil {
		t.Fatalf("SetSecretParameter: %v", err)
	}
	if len(f.putParameterIn) != 1 {
		t.Fatalf("PutParameter called %d times, want 1", len(f.putParameterIn))
	}
	put := f.putParameterIn[0]
	if !aws.ToBool(put.Overwrite) {
		t.Error("Overwrite was not set")
	}
	if aws.ToString(put.Value) != "the-real-value" {
		t.Errorf("Value = %q", aws.ToString(put.Value))
	}
	if len(put.Tags) != 0 {
		t.Errorf("Tags = %+v, want none: PutParameter refuses Tags with Overwrite", put.Tags)
	}
}

func TestClient_SetSecretParameter_DescribeParametersErrorPropagates(t *testing.T) {
	f := &fakeSSM{describeParametersErr: errors.New("throttled")}
	c := &Client{ssm: f}
	if err := c.SetSecretParameter(context.Background(), "/dev/api/secrets/x", "value"); err == nil {
		t.Fatal("SetSecretParameter succeeded, want the DescribeParameters error")
	}
}

func TestClient_SetSecretParameter_PutParameterErrorPropagates(t *testing.T) {
	f := &fakeSSM{
		describeParameters: []ssmtypes.ParameterMetadata{{Type: ssmtypes.ParameterTypeSecureString}},
		putParameterErr:    errors.New("throttled"),
	}
	c := &Client{ssm: f}
	if err := c.SetSecretParameter(context.Background(), "/dev/api/secrets/x", "value"); err == nil {
		t.Fatal("SetSecretParameter succeeded, want the PutParameter error")
	}
}

func TestClient_SetSecretParameter_RefusesToCreate(t *testing.T) {
	f := &fakeSSM{} // DescribeParameters returns no parameters: it does not exist yet
	c := &Client{ssm: f}

	err := c.SetSecretParameter(context.Background(), "/dev/api/secrets/x", "value")
	if err == nil {
		t.Fatal("SetSecretParameter succeeded over a parameter that does not exist, want an error")
	}
	if len(f.putParameterIn) != 0 {
		t.Fatalf("PutParameter was called %d times, want 0", len(f.putParameterIn))
	}
	if !strings.Contains(err.Error(), "kraai apply") {
		t.Errorf("error %q does not point at the fix", err)
	}
}

// --- Client.SecretsPolicyStatements (kraai iam-policy's operator grant) ---

func TestClient_SecretsPolicyStatements(t *testing.T) {
	c := &Client{sts: &fakeSTS{account: "123456789012"}, region: "us-east-1"}

	grants, err := c.SecretsPolicyStatements(context.Background(), []string{"/dev/api/secrets/pepper_key"})
	if err != nil {
		t.Fatalf("SecretsPolicyStatements: %v", err)
	}

	wantARN := "arn:aws:ssm:us-east-1:123456789012:parameter/dev/api/secrets/pepper_key"
	byAction := map[string][]string{}
	for _, g := range grants {
		byAction[g.Action] = append(byAction[g.Action], g.Resource)
	}
	for _, action := range secretsParameterActions {
		if resources := byAction[action]; len(resources) != 1 || resources[0] != wantARN {
			t.Errorf("%s resources = %v, want [%s]", action, resources, wantARN)
		}
	}
	if resources := byAction["ssm:DescribeParameters"]; len(resources) != 1 || resources[0] != "*" {
		t.Errorf("ssm:DescribeParameters resources = %v, want [*]: DescribeParameters cannot be scoped to a name", resources)
	}
}

func TestClient_SecretsPolicyStatements_Empty(t *testing.T) {
	c := &Client{}
	grants, err := c.SecretsPolicyStatements(context.Background(), nil)
	if err != nil || grants != nil {
		t.Fatalf("SecretsPolicyStatements(nil) = %v, %v, want nil, nil", grants, err)
	}
}

// --- register.go: Applicability --------------------------------------------

func TestBindingSecretsProviderIs(t *testing.T) {
	cond := bindingSecretsProviderIs(SecretsProviderSSM)
	if !cond(resource.ApplicabilityContext{Binding: map[string]any{"provider": "aws-ssm"}}) {
		t.Error("aws-ssm did not satisfy bindingSecretsProviderIs(aws-ssm)")
	}
	if cond(resource.ApplicabilityContext{Binding: map[string]any{"provider": "aws-secretsmanager"}}) {
		t.Error("a different provider satisfied bindingSecretsProviderIs(aws-ssm)")
	}
	if cond(resource.ApplicabilityContext{}) {
		t.Error("no binding at all satisfied bindingSecretsProviderIs(aws-ssm)")
	}
}
