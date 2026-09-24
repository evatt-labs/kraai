package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/assemble"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// secretsFixture declares one service with a secrets binding carrying a
// generate entry and an external one, against a fake catalog that accepts
// any binding shape (real schema enforcement is internal/provider/aws's).
func secretsFixture(t *testing.T) string {
	t.Helper()
	return writeFixture(t, map[string]string{
		"kraai.yaml": "version: 1\n\nproviders:\n  secrets:\n    vendor: aws\n",
		"services/services.yaml": "services:\n  api:\n    dir: .\n    secrets:\n" +
			"      - binding: SECRETS\n        provider: aws-ssm\n        entries:\n" +
			"            pepper_key:\n              generate:\n                bytes: 32\n                encoding: base64\n" +
			"            github_client_secret:\n              source: external\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\n",
	})
}

func secretsFixtureResolver(
	_ context.Context, fsys manifest.FS, envName string, setArgs []string,
) (*assemble.Resolved, error) {
	catalog, err := resource.NewCatalog(resource.FuncProvider{ProviderName: "aws", CapabilitiesFunc: func() []resource.CapabilityDef {
		return []resource.CapabilityDef{{Name: manifest.CapabilitySecrets, Summary: "fake secrets store"}}
	}})
	if err != nil {
		return nil, err
	}
	m, err := manifest.NewLoader(fsys, manifest.NewTemplateEngine(fsys), catalog).Load(envName, setArgs)
	if err != nil {
		return nil, err
	}
	return &assemble.Resolved{Manifest: m, Catalog: catalog}, nil
}

func fakeLocateExternal(entry assemble.SecretEntry) SecretLocator {
	return func(_ *manifest.Manifest, _, _, binding, e string) (assemble.SecretEntry, error) {
		entry.Binding, entry.Entry = binding, e
		return entry, nil
	}
}

func execSecretSet(
	t *testing.T, resolve ManifestResolver, locate SecretLocator, set SecretSetter,
	interactive isInteractive, stdin string, args []string,
) (string, error) {
	t.Helper()
	cmd := newSecretSetCommand(resolve, locate, set, interactive)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func neverInteractive(io.Reader) bool { return false }

func TestRunSecretSet_Success(t *testing.T) {
	dir := secretsFixture(t)
	locate := fakeLocateExternal(assemble.SecretEntry{
		ServiceKey: "api", Name: "/dev/api/secrets/github_client_secret", External: true,
	})
	var gotValue string
	var gotLocated assemble.SecretEntry
	set := func(_ context.Context, _ *manifest.Manifest, located assemble.SecretEntry, value string) error {
		gotLocated, gotValue = located, value
		return nil
	}

	out, err := execSecretSet(t, secretsFixtureResolver, locate, set, neverInteractive,
		"the-real-secret\n", []string{testEnvName, "SECRETS.github_client_secret", "--dir", dir})
	if err != nil {
		t.Fatalf("secret set: %v\n%s", err, out)
	}
	if gotValue != "the-real-secret" {
		t.Errorf("value written = %q, want the trailing newline trimmed", gotValue)
	}
	if gotLocated.Name != "/dev/api/secrets/github_client_secret" {
		t.Errorf("located = %+v", gotLocated)
	}
	if !strings.Contains(out, "SECRETS.github_client_secret") {
		t.Errorf("output = %q, want it to name what was set", out)
	}
}

func TestRunSecretSet_RefusesAGenerateEntry(t *testing.T) {
	dir := secretsFixture(t)
	locate := fakeLocateExternal(assemble.SecretEntry{External: false})
	setCalled := false
	set := func(context.Context, *manifest.Manifest, assemble.SecretEntry, string) error {
		setCalled = true
		return nil
	}

	_, err := execSecretSet(t, secretsFixtureResolver, locate, set, neverInteractive,
		"value\n", []string{testEnvName, "SECRETS.pepper_key", "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
	if setCalled {
		t.Fatal("set was called for a generate entry, want it refused before ever reaching the write")
	}
	if !strings.Contains(err.Error(), "source: external") {
		t.Errorf("error %q does not explain why it was refused", err)
	}
}

func TestRunSecretSet_LocateErrorPropagates(t *testing.T) {
	dir := secretsFixture(t)
	locate := func(*manifest.Manifest, string, string, string, string) (assemble.SecretEntry, error) {
		return assemble.SecretEntry{}, kerrors.Validation("no such binding")
	}
	_, err := execSecretSet(t, secretsFixtureResolver, locate, nil, neverInteractive,
		"value\n", []string{testEnvName, "SECRETS.pepper_key", "--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "no such binding") {
		t.Fatalf("err = %v, want the locate error", err)
	}
}

func TestRunSecretSet_SetErrorPropagates(t *testing.T) {
	dir := secretsFixture(t)
	locate := fakeLocateExternal(assemble.SecretEntry{External: true})
	set := func(context.Context, *manifest.Manifest, assemble.SecretEntry, string) error {
		return errors.New("AccessDenied")
	}
	_, err := execSecretSet(t, secretsFixtureResolver, locate, set, neverInteractive,
		"value\n", []string{testEnvName, "SECRETS.pepper_key", "--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("err = %v, want the set error", err)
	}
}

func TestRunSecretSet_InvalidTarget(t *testing.T) {
	dir := secretsFixture(t)
	for _, target := range []string{"SECRETS", ".entry", "SECRETS."} {
		t.Run(target, func(t *testing.T) {
			_, err := execSecretSet(t, secretsFixtureResolver, nil, nil, neverInteractive,
				"value\n", []string{testEnvName, target, "--dir", dir})
			_ = requireCode(t, err, kerrors.CodeValidation)
		})
	}
}

func TestRunSecretSet_InvalidEnvironmentName(t *testing.T) {
	_, err := execSecretSet(t, secretsFixtureResolver, nil, nil, neverInteractive,
		"value\n", []string{"Not Valid", "SECRETS.pepper_key"})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestRunSecretSet_EmptyValueIsRefused(t *testing.T) {
	dir := secretsFixture(t)
	locate := fakeLocateExternal(assemble.SecretEntry{External: true})
	setCalled := false
	set := func(context.Context, *manifest.Manifest, assemble.SecretEntry, string) error {
		setCalled = true
		return nil
	}
	_, err := execSecretSet(t, secretsFixtureResolver, locate, set, neverInteractive,
		"", []string{testEnvName, "SECRETS.pepper_key", "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
	if setCalled {
		t.Fatal("set was called with an empty value")
	}
}

func TestRunSecretSet_ServiceFlagReachesLocate(t *testing.T) {
	dir := secretsFixture(t)
	var gotService string
	locate := func(_ *manifest.Manifest, _, serviceKey, binding, entry string) (assemble.SecretEntry, error) {
		gotService = serviceKey
		return assemble.SecretEntry{Binding: binding, Entry: entry, External: true}, nil
	}
	set := func(context.Context, *manifest.Manifest, assemble.SecretEntry, string) error { return nil }

	_, err := execSecretSet(t, secretsFixtureResolver, locate, set, neverInteractive,
		"value\n", []string{testEnvName, "SECRETS.pepper_key", "--dir", dir, "--service", "api"})
	if err != nil {
		t.Fatalf("secret set: %v", err)
	}
	if gotService != "api" {
		t.Errorf("--service did not reach the locator: got %q", gotService)
	}
}

// --- readSecretValue, unit-tested directly (mirrors confirmProtected's own
// direct tests in apply_test.go, for the same reason: no pty in a test) ---

func TestReadSecretValue_NonInteractive_TrimsOneTrailingNewline(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"value\n", "value"},
		{"value\r\n", "value"},
		{"value", "value"},
		{"value\n\n", "value\n"},
		{"", ""},
	} {
		got, err := readSecretValue(strings.NewReader(c.in), io.Discard, neverInteractive)
		if err != nil {
			t.Fatalf("readSecretValue(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("readSecretValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReadSecretValue_InteractiveNonFileStdinIsAnError(t *testing.T) {
	alwaysInteractive := func(io.Reader) bool { return true }
	_, err := readSecretValue(strings.NewReader("value\n"), io.Discard, alwaysInteractive)
	if err == nil {
		t.Fatal("readSecretValue succeeded with a non-*os.File stdin reported interactive")
	}
}
