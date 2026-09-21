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
