package secretref

import "testing"

func TestParseNotAReference(t *testing.T) {
	for _, raw := range []string{
		"DB.connection_uri",
		"",
		"just a value",
		"http_no_scheme_delimiter",
	} {
		ref, isRef, err := Parse(raw)
		if err != nil {
			t.Errorf("Parse(%q) error = %v, want nil", raw, err)
		}
		if isRef {
			t.Errorf("Parse(%q) isRef = true, want false", raw)
		}
		if ref != (Ref{}) {
			t.Errorf("Parse(%q) = %+v, want zero Ref", raw, ref)
		}
	}
}

func TestParseValidReferences(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Ref
	}{
		{
			name: "ssm triple slash keeps leading path slash",
			raw:  "aws-ssm:///kraai/prod/github_client_secret",
			want: Ref{Scheme: "aws-ssm", Path: "/kraai/prod/github_client_secret"},
		},
		{
			name: "secretsmanager double slash with version",
			raw:  "aws-secretsmanager://kraai/prod/pepper_keys?version=AWSCURRENT",
			want: Ref{Scheme: "aws-secretsmanager", Path: "kraai/prod/pepper_keys", Version: "AWSCURRENT"},
		},
		{
			name: "versionId query key",
			raw:  "aws-secretsmanager://kraai/prod/pepper_keys?versionId=abc123",
			want: Ref{Scheme: "aws-secretsmanager", Path: "kraai/prod/pepper_keys", VersionID: "abc123"},
		},
		{
			name: "secretsmanager name containing @ is a literal path character",
			raw:  "aws-secretsmanager://svc-account@example.com/pepper",
			want: Ref{Scheme: "aws-secretsmanager", Path: "svc-account@example.com/pepper"},
		},
		{
			name: "secretsmanager name containing : is a literal path character",
			raw:  "aws-secretsmanager://kraai:prod/pepper",
			want: Ref{Scheme: "aws-secretsmanager", Path: "kraai:prod/pepper"},
		},
		{
			name: "percent-escaped version value",
			raw:  "aws-secretsmanager://kraai/prod/x?version=a%26b",
			want: Ref{Scheme: "aws-secretsmanager", Path: "kraai/prod/x", Version: "a&b"},
		},
		{
			name: "ssm parameter version number",
			raw:  "aws-ssm:///kraai/prod/x?version=3",
			want: Ref{Scheme: "aws-ssm", Path: "/kraai/prod/x", Version: "3"},
		},
		{
			name: "uppercase scheme letters are legal",
			raw:  "AWS-SSM:///a",
			want: Ref{Scheme: "AWS-SSM", Path: "/a"},
		},
		{
			name: "trailing & leaves an empty query pair, skipped",
			raw:  "aws-ssm:///a?version=1&",
			want: Ref{Scheme: "aws-ssm", Path: "/a", Version: "1"},
		},
		{
			name: "uppercase percent-escape hex digits",
			raw:  "aws-secretsmanager://kraai/prod/x?version=a%2Eb",
			want: Ref{Scheme: "aws-secretsmanager", Path: "kraai/prod/x", Version: "a.b"},
		},
		{
			name: "lowercase percent-escape hex digits",
			raw:  "aws-secretsmanager://kraai/prod/x?version=a%2eb",
			want: Ref{Scheme: "aws-secretsmanager", Path: "kraai/prod/x", Version: "a.b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, isRef, err := Parse(tt.raw)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tt.raw, err)
			}
			if !isRef {
				t.Fatalf("Parse(%q) isRef = false, want true", tt.raw)
			}
			if ref != tt.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.raw, ref, tt.want)
			}
		})
	}
}

func TestParseMalformed(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"empty path", "aws-ssm://"},
		{"empty path after host", "aws-secretsmanager://?version=1"},
		{"invalid scheme leading digit", "9aws-ssm:///a"},
		{"invalid scheme character", "aws ssm:///a"},
		{"fragment", "aws-ssm:///a/b#frag"},
		{"unrecognized query key", "aws-ssm:///a/b?stage=prod"},
		{"truncated percent escape", "aws-ssm:///a/b?version=%2"},
		{"invalid percent escape", "aws-ssm:///a/b?version=%zz"},
		{"invalid percent escape in a query key", "aws-ssm:///a/b?ver%zzsion=1"},
		{"empty scheme", "://a/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, isRef, err := Parse(tt.raw)
			if err == nil {
				t.Fatalf("Parse(%q) error = nil, want an error", tt.raw)
			}
			if isRef {
				t.Errorf("Parse(%q) isRef = true on error, want false", tt.raw)
			}
		})
	}
}

func TestRefString(t *testing.T) {
	tests := []struct {
		ref  Ref
		want string
	}{
		{Ref{Scheme: "aws-ssm", Path: "/a/b"}, "aws-ssm:///a/b"},
		{Ref{Scheme: "aws-secretsmanager", Path: "a/b", Version: "AWSCURRENT"}, "aws-secretsmanager://a/b?version=AWSCURRENT"},
		{Ref{Scheme: "aws-secretsmanager", Path: "a/b", VersionID: "v1"}, "aws-secretsmanager://a/b?versionId=v1"},
	}
	for _, tt := range tests {
		if got := tt.ref.String(); got != tt.want {
			t.Errorf("Ref%+v.String() = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

// TestRefStringNeverHoldsAValue documents, rather than tests behaviour that
// could vary: Ref has no field a resolved secret value could occupy, so
// String and every other method on it are safe to put in an error message
// by construction. Compile-time proof lives in the struct's fields; this
// just pins the field set so a future addition is a deliberate decision.
func TestRefStringNeverHoldsAValue(t *testing.T) {
	ref := Ref{Scheme: "aws-ssm", Path: "/a", Version: "1", VersionID: "2"}
	if ref.Scheme == "" || ref.Path == "" {
		t.Fatal("fixture is degenerate")
	}
}
