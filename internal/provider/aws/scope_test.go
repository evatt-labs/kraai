package aws

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// childFacts is a tagged type listed under a parent, by ApiId or by
// DomainName.
var childFacts = cfschema.Facts{
	TypeName: "AWS::Test::Child", Identity: cfschema.IdentityByTag, TagProperty: "Tags",
	TagShape: cfschema.TagShapeArray, TagOnCreate: true,
	ListScope: [][]string{{"ApiId"}, {"DomainName"}},
}

func childSpec(properties map[string]any, attrs map[string]map[string]any) resource.Spec {
	return resource.Spec{
		Binding: "CHILD", Name: "kraai-e-s-child",
		Config:     map[string]any{nativePropertiesKey: properties},
		References: map[string]string{"API": "aws/AWS::Test::Api::Native"},
		Attributes: attrs,
	}
}

func TestNativeScope(t *testing.T) {
	child := newNativeResourceWith(&fakeClient{}, staticSchemas{"type": "object"}, childFacts, resource.LookupByTag)
	parent := map[string]map[string]any{"API.aws/AWS::Test::Api::Native": {"ApiId": "a1"}}

	for name, c := range map[string]struct {
		spec      resource.Spec
		scope     string
		known     bool
		wantError string
	}{
		"parent published":       {spec: childSpec(map[string]any{"ApiId": "${API.ApiId}"}, parent), scope: `{"ApiId":"a1"}`, known: true},
		"parent not created yet": {spec: childSpec(map[string]any{"ApiId": "${API.ApiId}"}, nil)},
		"a literal parent":       {spec: childSpec(map[string]any{"ApiId": "literal-id"}, nil), scope: `{"ApiId":"literal-id"}`, known: true},
		"the second alternative": {spec: childSpec(map[string]any{"DomainName": "api.example"}, nil), scope: `{"DomainName":"api.example"}`, known: true},
		"no parent named":        {spec: childSpec(map[string]any{"Other": "x"}, nil), wantError: "set ApiId or DomainName"},
	} {
		t.Run(name, func(t *testing.T) {
			scope, known, err := child.Scope(c.spec)
			if c.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantError) {
					t.Fatalf("Scope error = %v, want %q", err, c.wantError)
				}
				return
			}
			if err != nil || scope != c.scope || known != c.known {
				t.Fatalf("Scope = %q, %v, %v; want %q, %v", scope, known, err, c.scope, c.known)
			}
		})
	}

	unscoped := newNativeResourceWith(&fakeClient{}, staticSchemas{"type": "object"}, cfschema.Facts{TypeName: "AWS::X::Y", Identity: cfschema.IdentityByName}, resource.LookupByName)
	if scope, known, err := unscoped.Scope(nativeSpec("x", nil)); scope != "" || !known || err != nil {
		t.Fatalf("an unscoped type's Scope = %q, %v, %v", scope, known, err)
	}
}

// The engine lists a scoped type under the parent the Ref names, and the
// schema's list requirement is still checked against it.
func TestScopedGetListsUnderTheRefsParent(t *testing.T) {
	cc := &fakeClient{schema: childFacts}
	child := newNativeResourceWith(cc, staticSchemas{"type": "object"}, childFacts, resource.LookupByTag)
	ref := resource.Ref{Provider: Provider, Type: resource.RoleType(childFacts.TypeName, nativeRole), Name: "kraai-e-s-child", Scope: `{"ApiId":"a1"}`}
	if _, err := child.Get(context.Background(), ref); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(cc.listModels) != 1 || !reflect.DeepEqual(cc.listModels[0], map[string]any{"ApiId": "a1"}) {
		t.Fatalf("list models = %v, want the Ref's scope", cc.listModels)
	}

	// Without a scope, the schema's requirement refuses the unscoped list
	// Cloud Control would reject.
	ref.Scope = ""
	if _, err := child.Get(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "list handler requires") {
		t.Fatalf("Get without a scope: %v", err)
	}
}
