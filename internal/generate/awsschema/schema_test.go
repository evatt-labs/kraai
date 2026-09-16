package awsschema

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func mustParseFixture(t *testing.T, path string) ResourceType {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test helper, path is always a fixed testdata/*.json literal from this package's own callers
	if err != nil {
		t.Fatalf("reading fixture %s: %v", path, err)
	}
	rt, err := Parse(data)
	if err != nil {
		t.Fatalf("parsing fixture %s: %v", path, err)
	}
	return rt
}

func TestParse_LambdaFunction(t *testing.T) {
	rt := mustParseFixture(t, "testdata/lambda_function.json")

	if rt.TypeName != "AWS::Lambda::Function" {
		t.Fatalf("TypeName = %q, want AWS::Lambda::Function", rt.TypeName)
	}
	if got, want := rt.PrimaryIdentifier, []string{"/properties/FunctionName"}; !stringSlicesEqual(got, want) {
		t.Fatalf("PrimaryIdentifier = %v, want %v", got, want)
	}
	wantCreateOnly := []string{"/properties/FunctionName", "/properties/PackageType", "/properties/TenancyConfig"}
	if !stringSlicesEqual(rt.CreateOnlyProperties, wantCreateOnly) {
		t.Fatalf("CreateOnlyProperties = %v, want %v", rt.CreateOnlyProperties, wantCreateOnly)
	}
	if !rt.Mutable() {
		t.Fatal("AWS::Lambda::Function must be mutable (declares an update handler)")
	}
	if !rt.HasVerb("create") || !rt.HasVerb("read") || !rt.HasVerb("delete") || !rt.HasVerb("list") {
		t.Fatalf("expected create/read/update/delete/list handlers, got %v", handlerVerbs(rt))
	}
	create, ok := rt.Handlers["create"]
	if !ok {
		t.Fatal("missing create handler")
	}
	if !containsString(create.Permissions, "lambda:CreateFunction") {
		t.Fatalf("create handler permissions missing lambda:CreateFunction: %v", create.Permissions)
	}
	if !rt.Tagging.Taggable || rt.Tagging.TagProperty != "/properties/Tags" {
		t.Fatalf("unexpected tagging: %+v", rt.Tagging)
	}
	if rt.PropertiesSchema == "" {
		t.Fatal("expected a non-empty PropertiesSchema")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(rt.PropertiesSchema), &doc); err != nil {
		t.Fatalf("PropertiesSchema is not valid JSON: %v", err)
	}
	if _, ok := doc["properties"]; !ok {
		t.Fatal("PropertiesSchema missing top-level \"properties\" key")
	}
}

// TestParse_LambdaPermission_NoUpdateHandler pins down the claim the spike
// brief hands in as already established: AWS::Lambda::Permission's handlers
// are [create, read, delete, list] — no update — so its immutability is
// machine-readable straight from the schema, not something kraai's own
// register.go comment (lambdapermission.go's DiffersFromState doc comment)
// has to assert by hand.
func TestParse_LambdaPermission_NoUpdateHandler(t *testing.T) {
	rt := mustParseFixture(t, "testdata/lambda_permission.json")

	if rt.Mutable() {
		t.Fatal("AWS::Lambda::Permission must be immutable (no update handler)")
	}
	wantVerbs := map[string]bool{"create": true, "read": true, "delete": true, "list": true}
	if len(rt.Handlers) != len(wantVerbs) {
		t.Fatalf("handlers = %v, want exactly %v", handlerVerbs(rt), wantVerbs)
	}
	for v := range wantVerbs {
		if !rt.HasVerb(v) {
			t.Fatalf("missing expected verb %q", v)
		}
	}

	// The list handler's handlerSchema requires FunctionName — the
	// schema-derivable reason kraai's own lambdaPermissionListScope
	// (internal/provider/aws/lambdapermission.go) must supply one to scope
	// a ListResources call. See SPIKE.md for how far this narrows that
	// hand-written function.
	list, ok := rt.Handlers["list"]
	if !ok {
		t.Fatal("missing list handler")
	}
	if !containsString(list.HandlerSchemaRequired, "FunctionName") {
		t.Fatalf("list handler HandlerSchemaRequired = %v, want to include FunctionName", list.HandlerSchemaRequired)
	}

	// primaryIdentifier is compound: [FunctionName, Id], and Id is
	// server-assigned (readOnly) — this is the schema-visible reason a
	// byName lookup is unavailable for this type and kraai's own
	// register.go instead declares Lookup: resource.LookupByAttr.
	if len(rt.PrimaryIdentifier) != 2 {
		t.Fatalf("PrimaryIdentifier = %v, want a 2-element compound identifier", rt.PrimaryIdentifier)
	}
	if !containsString(rt.ReadOnlyProperties, "/properties/Id") {
		t.Fatalf("ReadOnlyProperties = %v, want to include /properties/Id", rt.ReadOnlyProperties)
	}
}

func TestParse_ControlCharacterSanitization(t *testing.T) {
	// A raw, unescaped control byte (a literal newline) inside a JSON
	// string is invalid per RFC 8259 and rejected by Go's own strict
	// decoder. Confirms Parse survives input shaped like the brief's own
	// warning, even though none of this workstream's live-fetched fixtures
	// happened to exhibit it for real (see SPIKE.md).
	inner := `{"typeName":"AWS::Test::Widget","description":"line one` + "\x00NL\x00" +
		`line two","primaryIdentifier":["/properties/Id"],"properties":{"Id":{"type":"string"}},` +
		`"required":["Id"],"handlers":{"create":{"permissions":["test:Create"]}}}`
	// Embed inner as the JSON string value of the outer document's "Schema"
	// field: escape inner's own structural quotes so the outer document
	// parses as one JSON string, then inject a raw (unescaped) newline byte
	// at the placeholder — reproducing a schema whose embedded description
	// carries a literal control byte rather than a properly escaped "\n".
	escapedInner := strings.ReplaceAll(inner, `"`, `\"`)
	escapedInner = strings.ReplaceAll(escapedInner, "\x00NL\x00", "\n")
	raw := []byte(`{"TypeName":"AWS::Test::Widget","Schema":"` + escapedInner + `"}`)

	// Sanity check: the naive path (decoding the outer JSON, which itself
	// contains the raw newline byte inside a string value) must fail
	// without sanitization.
	var outer describeTypeOutput
	if err := json.Unmarshal(raw, &outer); err == nil {
		t.Fatal("expected raw control byte to break a naive json.Unmarshal; it did not — fixture no longer exercises the failure mode")
	}

	rt, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse should sanitize the control byte and succeed, got: %v", err)
	}
	if rt.TypeName != "AWS::Test::Widget" {
		t.Fatalf("TypeName = %q, want AWS::Test::Widget", rt.TypeName)
	}
	if rt.Description == "" {
		t.Fatal("expected a non-empty sanitized description")
	}
}

func TestParse_MissingSchema(t *testing.T) {
	_, err := Parse([]byte(`{"TypeName":"AWS::Test::Widget"}`))
	if err == nil {
		t.Fatal("expected an error for a describe-type body with no Schema field")
	}
}

func handlerVerbs(rt ResourceType) []string {
	return sortedKeys(rt.Handlers)
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] { //nolint:gosec // i ranges over a, whose length equals b's (checked above)
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
