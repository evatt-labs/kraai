package db

import (
	"reflect"
	"strings"
	"testing"
)

// The connection URIs below are fabricated fixtures with throwaway
// credentials — a URI parser cannot be tested without them, and gosec's G101
// cannot tell a fixture from a leak.
//
//nolint:gosec // G101: fabricated test credentials, not real ones
func TestParseConnectionURI(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want ConnectionInfo
	}{
		{
			name: "a typical Neon connection string",
			uri:  "postgresql://alice:s3cret@ep-cool-name-123456.us-east-2.aws.neon.tech/appdb?sslmode=require",
			want: ConnectionInfo{
				Scheme: "postgresql", Host: "ep-cool-name-123456.us-east-2.aws.neon.tech",
				Port: 5432, User: "alice", Password: "s3cret", Database: "appdb", SSLMode: "require",
			},
		},
		{
			name: "port defaults to 5432 when omitted",
			uri:  "postgres://u:p@db.example.com/mydb",
			want: ConnectionInfo{Scheme: "postgres", Host: "db.example.com", Port: 5432, User: "u", Password: "p", Database: "mydb", SSLMode: "require"},
		},
		{
			name: "an explicit non-default port is respected",
			uri:  "postgres://u:p@db.example.com:6543/mydb",
			want: ConnectionInfo{Scheme: "postgres", Host: "db.example.com", Port: 6543, User: "u", Password: "p", Database: "mydb", SSLMode: "require"},
		},
		{
			name: "sslmode defaults to require, not libpq's prefer",
			uri:  "postgres://u:p@db.example.com/mydb",
			want: ConnectionInfo{Scheme: "postgres", Host: "db.example.com", Port: 5432, User: "u", Password: "p", Database: "mydb", SSLMode: "require"},
		},
		{
			name: "an explicit sslmode wins",
			uri:  "postgres://u:p@db.example.com/mydb?sslmode=verify-full",
			want: ConnectionInfo{Scheme: "postgres", Host: "db.example.com", Port: 5432, User: "u", Password: "p", Database: "mydb", SSLMode: "verify-full"},
		},
		{
			name: "percent-encoded credentials are decoded",
			uri:  "postgres://user%40tenant:p%40ss%3Aword@db.example.com/mydb",
			want: ConnectionInfo{Scheme: "postgres", Host: "db.example.com", Port: 5432, User: "user@tenant", Password: "p@ss:word", Database: "mydb", SSLMode: "require"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseConnectionURI(tc.uri)
			if err != nil {
				t.Fatalf("ParseConnectionURI: %v", err)
			}
			// ConnectionInfo carries a map, so == does not apply.
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// TestParseConnectionURIErrorsKeepTheCredentialOut is the property that makes
// the rest of this package's care worthwhile: a URI that fails to parse is
// still a credential, and the standard library's own parse error quotes the
// whole input back.
func TestParseConnectionURIErrorsKeepTheCredentialOut(t *testing.T) {
	const secret = "hunter2supersecret"
	_, err := ParseConnectionURI("postgres://u:" + secret + "@db.example.com:notaport/x")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the error echoed the password back: %v", err)
	}
}

func TestConnectionInfoDSN(t *testing.T) {
	conn, err := ParseConnectionURI("postgres://u:p@h.example.com:6543/d?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	got := conn.DSN()
	for _, want := range []string{"h.example.com:6543", "/d", "sslmode=verify-full", "u:p@"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN %q is missing %q", got, want)
		}
	}
}

// TestRedactedCarriesNoCredential is what makes it safe to put a connection
// into an error: Redacted is the only rendering a human ever sees.
func TestRedactedCarriesNoCredential(t *testing.T) {
	conn, err := ParseConnectionURI("postgres://alice:hunter2supersecret@h.example.com:6543/appdb")
	if err != nil {
		t.Fatal(err)
	}
	got := conn.Redacted()
	for _, forbidden := range []string{"hunter2supersecret", "alice"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Redacted leaked %q: %s", forbidden, got)
		}
	}
	if !strings.Contains(got, "h.example.com") || !strings.Contains(got, "appdb") {
		t.Fatalf("Redacted dropped the parts that make it useful: %s", got)
	}
}

// TestDSNRoundTrips is the property that makes DSN safe to build by hand:
// a credential containing characters that are URI syntax must survive being
// re-encoded, or the driver connects with a silently different password.
func TestDSNRoundTrips(t *testing.T) {
	for _, original := range []string{
		"postgres://user:simple@h.example.com:5432/db?sslmode=require",
		"postgres://user%40tenant:p%40ss%3Aword@h.example.com:5432/db?sslmode=require",
		"postgres://u:p%2Fslash%3Fquestion%23hash@h.example.com:5432/db?sslmode=verify-full",
		"postgres://u:tr%25icky%26amp@h.example.com:5432/db?sslmode=require",
	} {
		first, err := ParseConnectionURI(original)
		if err != nil {
			t.Fatalf("parsing %s: %v", original, err)
		}
		second, err := ParseConnectionURI(first.DSN())
		if err != nil {
			t.Fatalf("re-parsing the DSN built from %s: %v", original, err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("round trip changed the connection:\n  first  %+v\n  second %+v", first, second)
		}
	}
}

// TestParseConnectionURIRejectsNonURIs: url.Parse accepts almost anything —
// "garbage" parses without error into an empty scheme and host. Unchecked, a
// truncated DATABASE_URL "succeeds" and fails much later as a connection
// error against ":5432/", pointing at the wrong thing entirely.
func TestParseConnectionURIRejectsNonURIs(t *testing.T) {
	for _, bad := range []string{
		"garbage",
		"",
		"postgres://",
		"/just/a/path",
		"postgres:///nohost",
	} {
		if _, err := ParseConnectionURI(bad); err == nil {
			t.Errorf("ParseConnectionURI accepted %q", bad)
		}
	}
}

// TestExtraQueryParametersSurvive: Neon issues channel_binding=require, and
// some configurations add options=endpoint%3D…. Re-emitting only sslmode
// would connect with different parameters than the provider handed out.
//
//nolint:gosec // G101: a fabricated fixture URI, not a real credential
func TestExtraQueryParametersSurvive(t *testing.T) {
	const uri = "postgres://u:p@h.example.com/d?sslmode=require&channel_binding=require&options=endpoint%3Dep-cool-123"

	conn, err := ParseConnectionURI(uri)
	if err != nil {
		t.Fatalf("ParseConnectionURI: %v", err)
	}
	if got := conn.Extra.Get("channel_binding"); got != "require" {
		t.Fatalf("channel_binding = %q, want it carried through", got)
	}
	if got := conn.Extra.Get("options"); got != "endpoint=ep-cool-123" {
		t.Fatalf("options = %q", got)
	}

	round, err := ParseConnectionURI(conn.DSN())
	if err != nil {
		t.Fatalf("re-parsing the rendered DSN: %v", err)
	}
	if !reflect.DeepEqual(conn, round) {
		t.Fatalf("parameters were lost rendering the DSN:\n  before %+v\n  after  %+v", conn, round)
	}
}
