package naming

import (
	"regexp"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestResourceName_Golden pins the JavaScript CLI's names.test.mjs
// resourceName fixtures.
func TestResourceName_Golden(t *testing.T) {
	cases := []struct {
		name, env, svc, binding, want string
	}{
		{
			name: "joins env, service key, and lowercased binding with hyphens",
			env:  "blue-honey-badger-12345", svc: "api", binding: "DB",
			want: "blue-honey-badger-12345-api-db",
		},
		{
			name: "replaces underscores and other non-alphanumerics with hyphens",
			env:  "n", svc: "api", binding: "MY_QUEUE",
			want: "n-api-my-queue",
		},
		{
			name: "collapses runs of separators rather than leaving them adjacent",
			env:  "n", svc: "api", binding: "MY__WEIRD--BINDING",
			want: "n-api-my-weird-binding",
		},
		{
			name: "strips a leading or trailing separator produced by the binding name itself",
			env:  "n", svc: "api", binding: "_LEADING",
			want: "n-api-leading",
		},
	}
	for _, c := range cases {
		got := ResourceName(c.env, c.svc, c.binding)
		if got != c.want {
			t.Errorf("%s: ResourceName(%q, %q, %q) = %q, want %q", c.name, c.env, c.svc, c.binding, got, c.want)
		}
	}
}

// TestResourceName_TruncatesTo63WithoutTrailingHyphen pins the JS
// suite's "truncates to 63 characters without leaving a trailing
// hyphen".
func TestResourceName_TruncatesTo63WithoutTrailingHyphen(t *testing.T) {
	long := ResourceName("n", "api", strings.Repeat("A", 80))
	if len(long) > 63 {
		t.Fatalf("len(%q) = %d, want <= 63", long, len(long))
	}
	if strings.HasSuffix(long, "-") {
		t.Fatalf("%q ends with a trailing hyphen", long)
	}
}

// TestResourceName_EmptySlugCanLeaveTrailingHyphenWhenUntruncated
// documents a real, inherited quirk found while porting: 0.5.0's
// trailing-hyphen strip only runs on the truncation branch
// (`name.slice(0, 63).replace(/-+$/, "")`), so a binding that slugs to
// the empty string (all separator characters) produces a name ending in
// a bare hyphen whenever the untruncated name is <= 63 bytes. This is
// not a bug introduced by the Go port — this package's job is matching
// 0.5.0's behavior byte for byte, not improving on it — but it's worth
// having pinned down rather than accidentally "fixed" by a future edit.
func TestResourceName_EmptySlugCanLeaveTrailingHyphenWhenUntruncated(t *testing.T) {
	got := ResourceName("blue-honey-badger-12345", "api", "___")
	want := "blue-honey-badger-12345-api-"
	if got != want {
		t.Fatalf("ResourceName(..., \"___\") = %q, want %q", got, want)
	}
}

// TestResourceName_NonASCIIEnvironmentNameTruncatesByBytesNotRunes locks
// in the UTF-16-vs-bytes divergence documented on ResourceName: a
// non-ASCII environmentName or serviceKey (never produced by either
// grammar this package owns, but not rejected by ResourceName itself
// either, since it doesn't re-validate its own arguments) is truncated
// by byte count, which can split a multi-byte rune. That's an accepted,
// deliberate consequence of choosing the 63-*byte* DNS/R2 limit over
// mirroring JS's UTF-16-code-unit slice/count — this test only pins down
// that it doesn't panic and still respects the 63-byte cap, not that the
// output is valid UTF-8.
func TestResourceName_NonASCIIEnvironmentNameTruncatesByBytesNotRunes(t *testing.T) {
	env := strings.Repeat("é", 40) // 2 bytes per rune in UTF-8: 80 bytes
	got := ResourceName(env, "a", "")
	if len(got) > 63 {
		t.Fatalf("len(%q) = %d, want <= 63", got, len(got))
	}
}

// slugPattern is slugify's own frozen output grammar: either empty, or
// one or more alphanumeric runs joined by single hyphens — never a
// leading/trailing hyphen, never a doubled hyphen, regardless of what
// nonsense the input binding contains.
var slugPattern = regexp.MustCompile(`^([a-z0-9]+(-[a-z0-9]+)*)?$`)

// TestRapid_Slugify_AlwaysMatchesFrozenPatternAndIsStable is a
// property-based rapid target for the binding-slugging half of naming
// derivation: for any input string whatsoever (empty, unicode, all separators, mixed
// case, arbitrarily long), slugify's output always matches slugPattern,
// and calling it twice on the same input always yields the same output.
func TestRapid_Slugify_AlwaysMatchesFrozenPatternAndIsStable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "binding")

		got1 := slugify(s)
		got2 := slugify(s)
		if got1 != got2 {
			t.Fatalf("slugify(%q) not stable: %q vs %q", s, got1, got2)
		}
		if !slugPattern.MatchString(got1) {
			t.Fatalf("slugify(%q) = %q, does not match frozen slug pattern", s, got1)
		}
	})
}

// environmentNamePattern generates strings for the rapid tests below
// that actually satisfy NamePattern, so ResourceName is exercised with
// realistic environmentName inputs rather than arbitrary garbage that
// could never reach it in practice (this package's own environment-name
// grammars are the only real source of environmentName values).
var environmentNamePattern = `[a-z]{2,15}-[a-z]{2,15}-[a-z]{2,15}-[0-9]{5}`

// serviceKeyPattern generates realistic manifest service keys: a
// services/*.yaml map key, which in practice is a short lowercase
// identifier.
var serviceKeyPattern = `[a-z][a-z0-9]{0,20}`

// TestRapid_ResourceName_BoundedAndStable is a property-based rapid
// target for resourceName's 63-byte bound and determinism guarantees,
// exercised over realistic environmentName/serviceKey values and fully
// arbitrary binding strings (to stress slugify).
func TestRapid_ResourceName_BoundedAndStable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		env := rapid.StringMatching(environmentNamePattern).Draw(t, "env")
		svc := rapid.StringMatching(serviceKeyPattern).Draw(t, "svc")
		binding := rapid.String().Draw(t, "binding")

		got1 := ResourceName(env, svc, binding)
		got2 := ResourceName(env, svc, binding)
		if got1 != got2 {
			t.Fatalf("ResourceName(%q, %q, %q) not stable: %q vs %q", env, svc, binding, got1, got2)
		}

		if len(got1) > 63 {
			t.Fatalf("ResourceName(%q, %q, %q) = %q, len %d > 63", env, svc, binding, got1, len(got1))
		}

		untruncatedLen := len(env) + 1 + len(svc) + 1 + len(slugify(binding))
		if untruncatedLen > 63 && strings.HasSuffix(got1, "-") {
			t.Fatalf("ResourceName(%q, %q, %q) = %q, truncated but ends with a trailing hyphen", env, svc, binding, got1)
		}
	})
}

// TestServiceName covers the deployable unit's own name, which ResourceName
// cannot produce: a service's code is not bound to anything, it is the thing
// doing the binding.
func TestServiceName(t *testing.T) {
	if got := ServiceName("env-a", "api"); got != "env-a-api" {
		t.Fatalf("got %q, want the 0.5.0 worker shape", got)
	}
	// Slugged like every other derived name.
	if got := ServiceName("env-a", "My Service"); got != "env-a-my-service" {
		t.Fatalf("got %q", got)
	}
	// Truncated, and never left ending in a separator.
	long := ServiceName("env-a", strings.Repeat("x", 100))
	if len(long) > 63 {
		t.Fatalf("got %d chars, want at most 63", len(long))
	}
	if strings.HasSuffix(long, "-") {
		t.Fatalf("truncation left a trailing separator: %q", long)
	}
}

// TestNamer_EmptyPrefixMatchesResourceName_Golden is the
// byte-identical-with-0.5.0 guarantee for Namer.Resource, pinned against
// literal expected strings —
// deliberately not against a call to ResourceName, since ResourceName is
// itself defined as Namer{}.Resource (resource.go): comparing the two
// would only ever prove they agree with themselves, even if the shared
// underlying computation broke. These are TestResourceName_Golden's own
// fixtures (the JavaScript CLI's test suite), asserted a second time
// through the Namer path with an explicit empty prefix, so a future
// change to Namer.Resource that silently altered the no-prefix case would
// fail here even if it also broke ResourceName in lockstep.
func TestNamer_EmptyPrefixMatchesResourceName_Golden(t *testing.T) {
	cases := []struct {
		name, env, svc, binding, want string
	}{
		{
			name: "joins env, service key, and lowercased binding with hyphens",
			env:  "blue-honey-badger-12345", svc: "api", binding: "DB",
			want: "blue-honey-badger-12345-api-db",
		},
		{
			name: "replaces underscores and other non-alphanumerics with hyphens",
			env:  "n", svc: "api", binding: "MY_QUEUE",
			want: "n-api-my-queue",
		},
		{
			name: "collapses runs of separators rather than leaving them adjacent",
			env:  "n", svc: "api", binding: "MY__WEIRD--BINDING",
			want: "n-api-my-weird-binding",
		},
		{
			name: "strips a leading or trailing separator produced by the binding name itself",
			env:  "n", svc: "api", binding: "_LEADING",
			want: "n-api-leading",
		},
	}
	for _, c := range cases {
		if got := (Namer{}).Resource(c.env, c.svc, c.binding); got != c.want {
			t.Errorf("%s: Namer{}.Resource(%q, %q, %q) = %q, want %q", c.name, c.env, c.svc, c.binding, got, c.want)
		}
		if got := NewNamer("").Resource(c.env, c.svc, c.binding); got != c.want {
			t.Errorf("%s: NewNamer(\"\").Resource(%q, %q, %q) = %q, want %q", c.name, c.env, c.svc, c.binding, got, c.want)
		}
	}
}

// TestNamer_EmptyPrefixMatchesServiceName_Golden is
// TestNamer_EmptyPrefixMatchesResourceName_Golden's counterpart for
// Namer.Service, pinned against literal strings for the same
// non-circularity reason.
func TestNamer_EmptyPrefixMatchesServiceName_Golden(t *testing.T) {
	cases := []struct{ env, svc, want string }{
		{"env-a", "api", "env-a-api"},
		{"env-a", "My Service", "env-a-my-service"},
	}
	for _, c := range cases {
		if got := (Namer{}).Service(c.env, c.svc); got != c.want {
			t.Errorf("Namer{}.Service(%q, %q) = %q, want %q", c.env, c.svc, got, c.want)
		}
	}
}

// TestNamer_PrefixAppliedToResourceName pins the actual point of this
// feature: a non-empty prefix appears ahead of the untruncated name,
// exactly as written in the manifest, with no separator inserted between
// prefix and environmentName (the prefix's own trailing hyphen, enforced
// at manifest load by internal/manifest's validatePrefix, is what
// supplies that separator).
func TestNamer_PrefixAppliedToResourceName(t *testing.T) {
	namer := NewNamer("kraai-api-")
	got := namer.Resource("prod", "api", "DB")
	want := "kraai-api-prod-api-db"
	if got != want {
		t.Fatalf("Resource(...) = %q, want %q", got, want)
	}
}

// TestNamer_PrefixAppliedToServiceName is
// TestNamer_PrefixAppliedToResourceName's counterpart for Service.
func TestNamer_PrefixAppliedToServiceName(t *testing.T) {
	namer := NewNamer("kraai-api-")
	got := namer.Service("prod", "api")
	want := "kraai-api-prod-api"
	if got != want {
		t.Fatalf("Service(...) = %q, want %q", got, want)
	}
}

// TestNamer_TruncatesTo63WithPrefixWithoutTrailingHyphen is
// TestResourceName_TruncatesTo63WithoutTrailingHyphen's counterpart with a
// prefix in play: the prefix is prepended before the 63-byte cut, not
// after (Namer.Resource's own doc comment: "prefix then truncate, never
// the reverse"), so a long enough binding still truncates the *whole*
// name — prefix included — down to 63 bytes, with the same
// only-on-the-truncation-branch trailing-hyphen strip ResourceName has
// always had.
func TestNamer_TruncatesTo63WithPrefixWithoutTrailingHyphen(t *testing.T) {
	namer := NewNamer("kraai-api-")
	long := namer.Resource("n", "api", strings.Repeat("A", 80))
	if len(long) > 63 {
		t.Fatalf("len(%q) = %d, want <= 63", long, len(long))
	}
	if !strings.HasPrefix(long, "kraai-api-") {
		t.Fatalf("%q lost its prefix under truncation", long)
	}
	if strings.HasSuffix(long, "-") {
		t.Fatalf("%q ends with a trailing hyphen", long)
	}
}

// TestNamer_TruncationBoundaryStripsHyphenExactlyAtCut is a deterministic,
// engineered case of the inherited trailing-hyphen-strip quirk (see truncate's
// doc comment and TestResourceName_EmptySlugCanLeaveTrailingHyphenWhenUntruncated):
// prefix, environmentName and serviceKey are sized so the untruncated
// name's 63rd byte (index 62) is itself the hyphen separating serviceKey
// from the binding slug. A truncate that forgot the trailing-hyphen strip
// would return a name ending in "-" here; this pins the case a purely
// random rapid draw would need to get exactly right to catch.
func TestNamer_TruncationBoundaryStripsHyphenExactlyAtCut(t *testing.T) {
	prefix := "p-"                 // 2 bytes
	env := strings.Repeat("e", 30) // 30 bytes
	svc := strings.Repeat("s", 29) // 29 bytes
	binding := "ZZZZ"              // slugs to "zzzz", 4 bytes, pushes past 63

	// Untruncated: "p-" + 30 "e"s + "-" + 29 "s"s + "-" + "zzzz"
	//            = 2 + 30 + 1 + 29 + 1 + 4 = 67 bytes.
	// Byte 62 (the 63rd byte, 0-indexed) is exactly the hyphen between
	// serviceKey and the binding slug, so name[:63] ends in "-" before
	// the trailing-hyphen strip runs.
	untruncated := prefix + env + "-" + svc + "-" + binding
	if len(untruncated) != 67 {
		t.Fatalf("test fixture is miscounted: len(untruncated) = %d, want 67", len(untruncated))
	}
	if untruncated[62] != '-' {
		t.Fatalf("test fixture is miscounted: byte 62 = %q, want '-'", untruncated[62])
	}

	got := NewNamer(prefix).Resource(env, svc, binding)
	want := prefix + env + "-" + svc // the 63-byte cut minus its trailing hyphen
	if got != want {
		t.Fatalf("Resource(...) = %q, want %q (trailing hyphen from truncation not stripped)", got, want)
	}
}

// TestNamer_Entry pins Entry's shape: a hierarchical path, prefix glued
// directly to the environment as Resource and Service do, binding and entry
// slugged.
func TestNamer_Entry(t *testing.T) {
	cases := []struct {
		name, prefix, env, svc, binding, entry, want string
	}{
		{
			name: "no prefix", prefix: "", env: "dev", svc: "api", binding: "SECRETS", entry: "pepper_key",
			want: "/dev/api/secrets/pepper-key",
		},
		{
			name: "prefix glues to the environment", prefix: "acme-", env: "prod", svc: "api", binding: "SECRETS", entry: "github_client_secret",
			want: "/acme-prod/api/secrets/github-client-secret",
		},
		{
			name: "binding and entry are slugged, env and service are not", prefix: "", env: "Dev_1", svc: "API", binding: "My Secrets", entry: "MY_KEY",
			want: "/Dev_1/API/my-secrets/my-key",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NewNamer(c.prefix).Entry(c.env, c.svc, c.binding, c.entry)
			if got != c.want {
				t.Errorf("Entry(%q, %q, %q, %q) = %q, want %q", c.env, c.svc, c.binding, c.entry, got, c.want)
			}
		})
	}
}

// TestNamer_Entry_NotTruncated documents that Entry, unlike Resource and
// Service, is never cut to 63 bytes: SSM's own ceiling is far higher, and a
// hierarchical path truncated blind risks two different entries colliding
// at the cut.
func TestNamer_Entry_NotTruncated(t *testing.T) {
	long := NewNamer("").Entry("dev", "api", strings.Repeat("A", 80), strings.Repeat("B", 80))
	if len(long) <= 63 {
		t.Fatalf("len(%q) = %d, want > 63 to exercise the no-truncation path", long, len(long))
	}
}

// TestNamer_Entry_DistinctFromResource documents why Entry cannot reuse
// Resource plus a fourth segment: two different (binding, entry) pairs that
// would collide if Resource's already-truncated output were reused as a
// prefix must not collide here.
func TestNamer_Entry_DistinctFromResource(t *testing.T) {
	namer := NewNamer("")
	a := namer.Entry("dev", "api", "SECRETS", "one")
	b := namer.Entry("dev", "api", "SECRETS", "two")
	if a == b {
		t.Fatalf("Entry(%q) and Entry(%q) collided: %q", "one", "two", a)
	}
}

// TestRapid_Namer_BoundedAndStable is TestRapid_ResourceName_BoundedAndStable's
// counterpart with a valid, realistic prefix applied: Namer.Resource stays
// <=63 bytes and deterministic regardless of prefix, and — since a valid
// prefix (see internal/manifest's prefixPattern) never itself ends the
// final name early — the prefix always survives untruncated whenever the
// rest of the name does not overflow the budget on its own.
func TestRapid_Namer_BoundedAndStable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prefix := rapid.StringMatching(`[a-z][a-z0-9]{0,9}-`).Draw(t, "prefix")
		env := rapid.StringMatching(environmentNamePattern).Draw(t, "env")
		svc := rapid.StringMatching(serviceKeyPattern).Draw(t, "svc")
		binding := rapid.String().Draw(t, "binding")

		namer := NewNamer(prefix)
		got1 := namer.Resource(env, svc, binding)
		got2 := namer.Resource(env, svc, binding)
		if got1 != got2 {
			t.Fatalf("Resource(%q, %q, %q) not stable under prefix %q: %q vs %q", env, svc, binding, prefix, got1, got2)
		}
		if len(got1) > 63 {
			t.Fatalf("Resource(%q, %q, %q) under prefix %q = %q, len %d > 63", env, svc, binding, prefix, got1, len(got1))
		}
		if !strings.HasPrefix(got1, prefix) {
			t.Fatalf("Resource(%q, %q, %q) under prefix %q = %q, lost its prefix", env, svc, binding, prefix, got1)
		}
	})
}
