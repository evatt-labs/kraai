package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// fakeCatalogAssembler returns a small, fixed catalog — no test in this
// file imports internal/assemble or any internal/provider/* package, and
// none reaches a network or needs a credential, proving `kraai
// capabilities` itself is exercisable with none configured.
func fakeCatalogAssembler() (*resource.Catalog, error) {
	return resource.NewCatalog(
		resource.FuncProvider{ProviderName: "aws", CapabilitiesFunc: func() []resource.CapabilityDef {
			return []resource.CapabilityDef{
				{Name: "objects", Summary: "S3 bucket"},
				{Name: "compute", Summary: "Lambda function"},
			}
		}},
		resource.FuncProvider{ProviderName: "cloudflare", CapabilitiesFunc: func() []resource.CapabilityDef {
			return []resource.CapabilityDef{{Name: "objects", Summary: "R2 bucket"}}
		}},
	)
}

// failingCatalogAssembler fails, proving runCapabilities propagates an
// assembler error rather than swallowing it.
func failingCatalogAssembler() (*resource.Catalog, error) {
	return nil, kerrors.Validation("provider %q declares capability %q more than once", "dup", "objects")
}

// execCapabilities builds a standalone capabilities command (not through
// the root command) and executes it with args, capturing stdout.
func execCapabilities(t *testing.T, assembler CatalogAssembler, args []string) (string, error) {
	t.Helper()
	cmd := newCapabilitiesCommand(assembler)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestRunCapabilities_TextOutput(t *testing.T) {
	out, err := execCapabilities(t, fakeCatalogAssembler, nil)
	if err != nil {
		t.Fatalf("execCapabilities: %v", err)
	}

	for _, want := range []string{"compute", "objects", "aws", "cloudflare", "S3 bucket", "R2 bucket", "Lambda function"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestRunCapabilities_JSONOutput(t *testing.T) {
	out, err := execCapabilities(t, fakeCatalogAssembler, []string{"--json"})
	if err != nil {
		t.Fatalf("execCapabilities: %v", err)
	}

	var doc capabilitiesDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("Unmarshal: %v\noutput: %s", err, out)
	}
	if len(doc.Capabilities) != 2 {
		t.Fatalf("Capabilities = %+v, want 2 entries (compute, objects)", doc.Capabilities)
	}

	byName := map[string]capabilityJSON{}
	for _, c := range doc.Capabilities {
		byName[c.Name] = c
	}

	compute, ok := byName["compute"]
	if !ok || len(compute.Providers) != 1 || compute.Providers[0].Provider != "aws" {
		t.Fatalf("compute = %+v, want exactly one aws provider", compute)
	}

	objects, ok := byName["objects"]
	if !ok || len(objects.Providers) != 2 {
		t.Fatalf("objects = %+v, want two providers", objects)
	}
	gotProviders := map[string]string{}
	for _, p := range objects.Providers {
		gotProviders[p.Provider] = p.Summary
	}
	if gotProviders["aws"] != "S3 bucket" || gotProviders["cloudflare"] != "R2 bucket" {
		t.Fatalf("objects providers = %+v, want aws=S3 bucket, cloudflare=R2 bucket", gotProviders)
	}
}

func TestRunCapabilities_PropagatesAssemblerError(t *testing.T) {
	_, err := execCapabilities(t, failingCatalogAssembler, nil)
	if err == nil {
		t.Fatal("expected the assembler's error to propagate")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) {
		t.Fatalf("expected a *kerrors.KError, got %T (%v)", err, err)
	}
}

func TestRunCapabilities_RejectsArgs(t *testing.T) {
	if _, err := execCapabilities(t, fakeCatalogAssembler, []string{"unexpected"}); err == nil {
		t.Error("execCapabilities with an unexpected positional arg = nil error, want an error")
	}
}

func TestNewRootCommand_HasCapabilitiesCommand(t *testing.T) {
	root := NewRootCommand()
	cmd, _, err := root.Find([]string{"capabilities"})
	if err != nil {
		t.Fatalf("Find(capabilities): %v", err)
	}
	if cmd.Name() != "capabilities" {
		t.Errorf("Find(capabilities) = %q, want \"capabilities\"", cmd.Name())
	}
}

// TestNewRootCommand_CapabilitiesWorksWithNoCredentials is the CLI-level
// proof of this workstream's central constraint: `kraai capabilities`
// through the real root command (wired to assemble.Capabilities, not a
// fake) succeeds with no provider credentials configured at all.
func TestNewRootCommand_CapabilitiesWorksWithNoCredentials(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("NEON_API_KEY", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"capabilities"})

	if err := root.Execute(); err != nil {
		t.Fatalf("kraai capabilities with no credentials: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestWriteCapabilitiesText_EmptyCatalog(t *testing.T) {
	cat, err := resource.NewCatalog()
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	var out bytes.Buffer
	if err := writeCapabilitiesText(&out, cat); err != nil {
		t.Fatalf("writeCapabilitiesText: %v", err)
	}
	if out.String() != "no capabilities registered\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestToCapabilitiesDocument_NilCatalog(t *testing.T) {
	doc := toCapabilitiesDocument(nil)
	if len(doc.Capabilities) != 0 {
		t.Fatalf("Capabilities = %+v, want empty", doc.Capabilities)
	}
}
