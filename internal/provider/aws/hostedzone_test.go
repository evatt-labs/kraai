package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// The zone's desired state carries its name and kraai's tag — the two
// things the bare engine never wrote, which is how a zone could be created
// that nothing could find again (evatt-labs/kraai#117).
func TestHostedZoneCreateWritesTheNameAndTheTag(t *testing.T) {
	client := &fakeClient{createID: "Z123", createProps: map[string]any{"Id": "Z123", "Name": "acme.example."}}
	zone := newHostedZoneResource(&Client{})
	zone.client = client

	state, err := zone.Create(context.Background(), resource.Spec{Binding: "ZONE", Name: "acme.example"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(client.createCalls) != 1 {
		t.Fatalf("createCalls = %d, want 1", len(client.createCalls))
	}
	desired := client.createCalls[0]
	if desired["Name"] != "acme.example" {
		t.Errorf("Name = %v, want the zone the entry declared", desired["Name"])
	}
	if !arrayTagsMatchIn(desired, hostedZoneTagsProperty, "acme.example") {
		t.Errorf("desired = %v, want kraai's identity tag under %s", desired, hostedZoneTagsProperty)
	}
	if state == nil || state.ID != "Z123" {
		t.Errorf("state = %+v, want the created zone", state)
	}
}

func TestHostedZoneCreateRefusesANamelessSpec(t *testing.T) {
	client := &fakeClient{}
	zone := newHostedZoneResource(&Client{})
	zone.client = client

	_, err := zone.Create(context.Background(), resource.Spec{Binding: "ZONE"})
	if err == nil {
		t.Fatal("a zone was created with no name — the silent-leak case")
	}
	if len(client.createCalls) != 0 {
		t.Fatal("a create was submitted despite the missing name")
	}
}

// The zone name is the identity, and Route 53 reports it with a trailing
// dot; a plan must read that as unchanged, not as a replacement every run.
func TestHostedZoneDiffIgnoresTheTrailingDot(t *testing.T) {
	zone := newHostedZoneResource(&Client{})
	spec := resource.Spec{Binding: "ZONE", Name: "acme.example"}

	d, err := zone.Diff(spec, &resource.State{Attributes: map[string]any{"Name": "acme.example."}})
	if err != nil || d != resource.Same {
		t.Fatalf("Diff = %v, %v, want Same for the same zone with a trailing dot", d, err)
	}
	d, err = zone.Diff(spec, &resource.State{Attributes: map[string]any{"Name": "other.example."}})
	if err != nil || d != resource.Immutable {
		t.Fatalf("Diff = %v, %v, want Immutable for a different zone", d, err)
	}
}

func TestBareHostedZoneID(t *testing.T) {
	for in, want := range map[string]string{"/hostedzone/Z123": "Z123", "Z123": "Z123"} {
		if got := bareHostedZoneID(in); got != want {
			t.Errorf("bareHostedZoneID(%q) = %q, want %q", in, got, want)
		}
	}
	if !strings.HasPrefix(hostedZoneIDPrefix, "/") {
		t.Error("the prefix is not the path form Route 53 uses")
	}
}
