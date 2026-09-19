package aws

import (
	"context"
	"strings"
	"testing"
)

func TestApigatewayv2Match(t *testing.T) {
	cases := []struct {
		name       string
		properties map[string]any
		want       string
		match      bool
	}{
		{
			name:       "matches the identity tag",
			properties: map[string]any{"Tags": map[string]any{identityTagKey: "my-api"}},
			want:       "my-api",
			match:      true,
		},
		{
			name:       "a different tag value does not match",
			properties: map[string]any{"Tags": map[string]any{identityTagKey: "other-api"}},
			want:       "my-api",
			match:      false,
		},
		{
			name:       "an untagged API cannot be found this way",
			properties: map[string]any{"Tags": map[string]any{}},
			want:       "my-api",
			match:      false,
		},
		{
			name:       "Tags missing entirely",
			properties: map[string]any{},
			want:       "my-api",
			match:      false,
		},
		{
			name:       "Tags present but not a map",
			properties: map[string]any{"Tags": "not-a-map"},
			want:       "my-api",
			match:      false,
		},
		{
			name:       "the identity tag present but not a string",
			properties: map[string]any{"Tags": map[string]any{identityTagKey: 42}},
			want:       "my-api",
			match:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := apigatewayv2Match(tc.properties, tc.want); got != tc.match {
				t.Fatalf("apigatewayv2Match = %v, want %v", got, tc.match)
			}
		})
	}
}

func TestApigatewayv2StampTag(t *testing.T) {
	t.Run("adds the tag to an empty desired state", func(t *testing.T) {
		desired := map[string]any{}
		apigatewayv2StampTag(desired, "my-api")

		tags, ok := desired["Tags"].(map[string]any)
		if !ok || tags[identityTagKey] != "my-api" {
			t.Fatalf("Tags = %+v", desired["Tags"])
		}
	})

	t.Run("adds the tag alongside existing tags", func(t *testing.T) {
		desired := map[string]any{"Tags": map[string]any{"env": "prod"}}
		apigatewayv2StampTag(desired, "my-api")

		tags, _ := desired["Tags"].(map[string]any)
		if tags["env"] != "prod" || tags[identityTagKey] != "my-api" {
			t.Fatalf("Tags = %+v", tags)
		}
	})

	t.Run("overwrites a stale prior value rather than leaving two", func(t *testing.T) {
		desired := map[string]any{"Tags": map[string]any{identityTagKey: "stale-name"}}
		apigatewayv2StampTag(desired, "my-api")

		tags, _ := desired["Tags"].(map[string]any)
		if tags[identityTagKey] != "my-api" {
			t.Fatalf("Tags = %+v", tags)
		}
	})
}

func TestCertificateMatch(t *testing.T) {
	cases := []struct {
		name       string
		properties map[string]any
		want       string
		match      bool
	}{
		{
			name: "matches the identity tag in the array shape",
			properties: map[string]any{"Tags": []any{
				map[string]any{"Key": "env", "Value": "prod"},
				map[string]any{"Key": identityTagKey, "Value": "my-cert"},
			}},
			want:  "my-cert",
			match: true,
		},
		{
			name:       "a different tag value does not match",
			properties: map[string]any{"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "other-cert"}}},
			want:       "my-cert",
			match:      false,
		},
		{
			name:       "Tags missing entirely",
			properties: map[string]any{},
			want:       "my-cert",
			match:      false,
		},
		{
			name:       "Tags present but not an array (the ApiGatewayV2 flat-map shape) does not match",
			properties: map[string]any{"Tags": map[string]any{identityTagKey: "my-cert"}},
			want:       "my-cert",
			match:      false,
		},
		{
			name:       "a non-map element in Tags is skipped, not fatal",
			properties: map[string]any{"Tags": []any{"not-a-map", map[string]any{"Key": identityTagKey, "Value": "my-cert"}}},
			want:       "my-cert",
			match:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := certificateMatch(tc.properties, tc.want); got != tc.match {
				t.Fatalf("certificateMatch = %v, want %v", got, tc.match)
			}
		})
	}
}

func TestCertificateStampTag(t *testing.T) {
	t.Run("adds the tag to an empty desired state", func(t *testing.T) {
		desired := map[string]any{}
		certificateStampTag(desired, "my-cert")

		if !certificateMatch(desired, "my-cert") {
			t.Fatalf("Tags = %+v, want the stamped tag to round-trip through certificateMatch", desired["Tags"])
		}
	})

	t.Run("preserves existing tags and replaces a stale identity tag rather than duplicating it", func(t *testing.T) {
		desired := map[string]any{"Tags": []any{
			map[string]any{"Key": "env", "Value": "prod"},
			map[string]any{"Key": identityTagKey, "Value": "stale-name"},
		}}
		certificateStampTag(desired, "my-cert")

		tags, _ := desired["Tags"].([]any)
		if len(tags) != 2 {
			t.Fatalf("Tags = %+v, want exactly 2 entries (env preserved, identity tag replaced not duplicated)", tags)
		}
		if !certificateMatch(desired, "my-cert") {
			t.Fatalf("Tags = %+v", tags)
		}
	})
}

func TestHostedZoneMatch(t *testing.T) {
	cases := []struct {
		name       string
		properties map[string]any
		want       string
		match      bool
	}{
		{name: "matches the zone Name", properties: map[string]any{"Name": "example.com."}, want: "example.com.", match: true},
		// Route 53 reports the name with a trailing dot; a manifest's zone
		// will usually not have one. The registration is NameFromEntry, so
		// name here is exactly what the manifest wrote.
		{name: "the manifest's zone without a trailing dot", properties: map[string]any{"Name": "example.com."}, want: "example.com", match: true},
		{name: "a subdomain is not its parent", properties: map[string]any{"Name": "www.example.com."}, want: "example.com", match: false},
		{name: "a different name does not match", properties: map[string]any{"Name": "other.com."}, want: "example.com.", match: false},
		{name: "Name missing entirely", properties: map[string]any{}, want: "example.com.", match: false},
		{name: "Name present but not a string", properties: map[string]any{"Name": 42}, want: "example.com.", match: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostedZoneMatch(tc.properties, tc.want); got != tc.match {
				t.Fatalf("hostedZoneMatch = %v, want %v", got, tc.match)
			}
		})
	}
}

// Among one zone's records (the list is scoped to it), the apex A record —
// name is the zone, so the apex is the record named exactly that.
func TestRecordSetMatch(t *testing.T) {
	cases := []struct {
		name       string
		properties map[string]any
		want       string
		match      bool
	}{
		{name: "the apex A record", properties: map[string]any{"Name": "example.com.", "Type": "A"}, want: "example.com", match: true},
		{name: "the apex AAAA record is a different resource", properties: map[string]any{"Name": "example.com.", "Type": "AAAA"}, want: "example.com", match: false},
		{name: "a subdomain is not the apex", properties: map[string]any{"Name": "www.example.com.", "Type": "A"}, want: "example.com", match: false},
		{name: "the zone's NS record", properties: map[string]any{"Name": "example.com.", "Type": "NS"}, want: "example.com", match: false},
		{name: "Name missing entirely", properties: map[string]any{"Type": "A"}, want: "example.com", match: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recordSetMatch(tc.properties, tc.want); got != tc.match {
				t.Fatalf("recordSetMatch = %v, want %v", got, tc.match)
			}
		})
	}
}

// A zone is found by name, which proves nothing about who made it; the tag
// kraai stamps on create is what does. A zone with no tag is refused, not
// reported absent — absent would have plan create a second zone of the same
// name, which Route 53 allows and which shadows the real one.
func TestHostedZoneOwned(t *testing.T) {
	tagged := func(value string) []any {
		return []any{map[string]any{"Key": identityTagKey, "Value": value}}
	}
	cases := []struct {
		name       string
		properties map[string]any
		owned      bool
		wantErr    string
	}{
		{"kraai's own zone", map[string]any{"Name": "acme.example.", hostedZoneTagsProperty: tagged("acme.example")}, true, ""},
		{"tagged with a trailing dot", map[string]any{"Name": "acme.example.", hostedZoneTagsProperty: tagged("acme.example.")}, true, ""},
		{"kraai's tag for a different zone", map[string]any{"Name": "acme.example.", hostedZoneTagsProperty: tagged("other.example")}, false, ""},
		{"no kraai tag", map[string]any{"Name": "acme.example.", hostedZoneTagsProperty: []any{map[string]any{"Key": "team", "Value": "web"}}}, false, "not created by kraai"},
		{"no tags at all", map[string]any{"Name": "acme.example."}, false, "not created by kraai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owned, err := hostedZoneOwned(context.Background(), "Z123", tc.properties)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to say %q", err, tc.wantErr)
				}
				// The way out is named: adopt it, under the zone's real name.
				for _, want := range []string{"resources:", `{name: "acme.example"}`, "Z123"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error should mention %q: %v", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("hostedZoneOwned: %v", err)
			}
			if owned != tc.owned {
				t.Fatalf("owned = %v, want %v", owned, tc.owned)
			}
		})
	}
}

func TestHostedZoneStampTagWritesHostedZoneTags(t *testing.T) {
	desired := map[string]any{"Name": "acme.example"}
	hostedZoneStampTag(desired, "acme.example")
	if _, hasTags := desired["Tags"]; hasTags {
		t.Error("the tag landed under Tags, which this type does not have")
	}
	if !arrayTagsMatchIn(desired, hostedZoneTagsProperty, "acme.example") {
		t.Errorf("desired = %v, want the identity tag under %s", desired, hostedZoneTagsProperty)
	}
}
