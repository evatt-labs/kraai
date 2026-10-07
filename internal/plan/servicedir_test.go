package plan

import "testing"

// A service's dir is taken against the manifest's directory, never the one
// kraai runs in; an absolute one, or one with no manifest directory to take
// it against, is left as written.
func TestServiceDir(t *testing.T) {
	for _, c := range []struct{ manifestDir, dir, want string }{
		{"/repo/infra", "handler", "/repo/infra/handler"},
		{"/repo/infra", "../services/api", "/repo/services/api"},
		{"/repo/infra", "/opt/code", "/opt/code"},
		{"", "handler", "handler"},
		{"/repo/infra", "", ""},
	} {
		if got := serviceDir(c.manifestDir, c.dir); got != c.want {
			t.Errorf("serviceDir(%q, %q) = %q, want %q", c.manifestDir, c.dir, got, c.want)
		}
	}
}
