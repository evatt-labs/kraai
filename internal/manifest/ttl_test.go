package manifest

import (
	"strings"
	"testing"
	"time"
)

// A ttl belongs to an ephemeral environment only, as a positive duration;
// what it means (an absolute deadline written at apply) is the status
// record's business, but the field is checked here where every other
// environment field is.
func TestEnvironmentTTL(t *testing.T) {
	cases := map[string]struct {
		kind, ttl string
		wantErr   string
		want      time.Duration
	}{
		"ephemeral with ttl":  {kind: EnvironmentKindEphemeral, ttl: "72h", want: 72 * time.Hour},
		"ephemeral without":   {kind: EnvironmentKindEphemeral},
		"persistent with ttl": {kind: EnvironmentKindPersistent, ttl: "72h", wantErr: "ttl: only an ephemeral"},
		"not a duration":      {kind: EnvironmentKindEphemeral, ttl: "three days", wantErr: "positive duration"},
		"negative":            {kind: EnvironmentKindEphemeral, ttl: "-1h", wantErr: "positive duration"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			env := &Environment{Kind: c.kind, TTL: c.ttl}
			err := validateEnvironment("environments/x.yaml", env)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateEnvironment: %v", err)
			}
			if got := env.TTLDuration(); got != c.want {
				t.Fatalf("TTLDuration = %s, want %s", got, c.want)
			}
		})
	}
}

// A policy set name becomes a directory joined onto --policy paths read
// outside the manifest root, so anything but one plain segment is refused.
func TestEnvironmentPolicySets(t *testing.T) {
	cases := map[string]struct {
		sets    []string
		wantErr string
	}{
		"none":             {},
		"two":              {sets: []string{"production", "pci_scope-2"}},
		"a parent segment": {sets: []string{".."}, wantErr: "must match"},
		"a traversal":      {sets: []string{"../../etc"}, wantErr: "must match"},
		"a separator":      {sets: []string{"prod/extra"}, wantErr: "must match"},
		"an absolute path": {sets: []string{"/etc"}, wantErr: "must match"},
		"empty":            {sets: []string{""}, wantErr: "must match"},
		"uppercase":        {sets: []string{"Production"}, wantErr: "must match"},
		"named twice":      {sets: []string{"production", "production"}, wantErr: "named twice"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			err := validateEnvironment("environments/x.yaml", &Environment{Kind: EnvironmentKindPersistent, Policies: c.sets})
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("validateEnvironment: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
			}
		})
	}
}
