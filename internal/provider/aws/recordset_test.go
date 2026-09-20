package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

func recordSpec(alias string, attrs map[string]map[string]any) resource.Spec {
	config := map[string]any{"zone": "acme.example"}
	if alias != "" {
		config["alias"] = alias
	}
	return resource.Spec{Binding: "ZONE", Name: "acme.example", Config: config, Attributes: attrs}
}

func recordAttrs(zoneID, target string) map[string]map[string]any {
	attrs := map[string]map[string]any{}
	if zoneID != "" {
		attrs[key(TypeRoute53HostedZone)] = map[string]any{"Id": zoneID}
	}
	if target != "" {
		attrs["EDGE."+key(TypeCloudFrontDistribution)] = map[string]any{"DomainName": target}
	}
	return attrs
}

// The apex of its own zone, aliased to the distribution its entry names:
// the zone id from its own binding (bare key), the target from the
// referenced one (namespaced), and CloudFront's fixed alias zone.
func TestRecordSetTranslateAliasesTheApexToTheDistribution(t *testing.T) {
	rs := newRecordSetResource(&Client{})
	translated, err := rs.translate(recordSpec("EDGE", recordAttrs("/hostedzone/Z123", "d123.cloudfront.net")))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	cfg := translated.Config
	if cfg["HostedZoneId"] != "Z123" || cfg["Name"] != "acme.example." || cfg["Type"] != "A" {
		t.Errorf("HostedZoneId/Name/Type = %v/%v/%v", cfg["HostedZoneId"], cfg["Name"], cfg["Type"])
	}
	alias, _ := cfg["AliasTarget"].(map[string]any)
	if alias["DNSName"] != "d123.cloudfront.net" || alias["HostedZoneId"] != cloudFrontHostedZoneID || alias["EvaluateTargetHealth"] != false {
		t.Errorf("AliasTarget = %v", alias)
	}
}

func TestRecordSetTranslateRefusals(t *testing.T) {
	rs := newRecordSetResource(&Client{})
	for _, c := range []struct {
		name    string
		spec    resource.Spec
		wantErr string
	}{
		{"no alias", recordSpec("", recordAttrs("Z1", "d")), "names no alias"},
		{"zone not published", recordSpec("EDGE", recordAttrs("", "d")), key(TypeRoute53HostedZone)},
		{"distribution not published", recordSpec("EDGE", recordAttrs("Z1", "")), key(TypeCloudFrontDistribution)},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := rs.translate(c.spec)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

// Cloud Control lists this type only within a zone, and accepts the zone's
// name — which is exactly the name this instance carries.
func TestRecordSetGetListsItsOwnZone(t *testing.T) {
	client := &fakeClient{
		list: []string{"Z1|acme.example.|NS", "Z1|acme.example.|A"},
		byIdentifier: map[string]map[string]any{
			"Z1|acme.example.|NS": {"Name": "acme.example.", "Type": "NS"},
			"Z1|acme.example.|A":  {"Name": "acme.example.", "Type": "A", "AliasTarget": map[string]any{"DNSName": "d.cloudfront.net"}},
		},
	}
	rs := newRecordSetResource(&Client{})
	rs.client = client

	state, err := rs.Get(context.Background(), resource.Ref{Provider: Provider, Type: TypeRoute53RecordSet, Name: "acme.example"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil || state.ID != "Z1|acme.example.|A" {
		t.Fatalf("state = %+v, want the apex A record", state)
	}
	if len(client.listModels) != 1 || client.listModels[0]["HostedZoneName"] != "acme.example." {
		t.Fatalf("list scoped by %v, want HostedZoneName acme.example.", client.listModels)
	}
}

func TestRecordSetDiffComparesTheTarget(t *testing.T) {
	rs := newRecordSetResource(&Client{})
	spec := recordSpec("EDGE", recordAttrs("Z1", "d123.cloudfront.net"))
	live := func(target string) *resource.State {
		return &resource.State{Attributes: map[string]any{
			"Name": "acme.example.", "Type": "A", "AliasTarget": map[string]any{"DNSName": target, "HostedZoneId": cloudFrontHostedZoneID},
		}}
	}
	if d, err := rs.Diff(spec, live("d123.cloudfront.net")); err != nil || d != resource.Same {
		t.Fatalf("Diff = %v, %v, want Same", d, err)
	}
	if d, err := rs.Diff(spec, live("old.cloudfront.net")); err != nil || d != resource.Mutable {
		t.Fatalf("Diff = %v, %v, want Mutable after the distribution changed", d, err)
	}
}

func TestFQDN(t *testing.T) {
	for in, want := range map[string]string{"acme.example": "acme.example.", "acme.example.": "acme.example."} {
		if got := fqdn(in); got != want {
			t.Errorf("fqdn(%q) = %q, want %q", in, got, want)
		}
	}
}
