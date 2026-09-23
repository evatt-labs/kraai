package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// routeFacts is an untaggable type listed under its API, like
// AWS::ApiGatewayV2::Route.
var routeFacts = cfschema.Facts{
	TypeName: "AWS::Test::Route", Identity: cfschema.IdentityByAttr,
	ListScope: [][]string{{"ApiId"}}, CreateOnly: []string{"/properties/ApiId"},
	ReadOnly: []string{"/properties/RouteId"}, WriteOnly: []string{"/properties/Secret"}, HasUpdate: true,
}

const apiProducer = "aws/AWS::ApiGatewayV2::Api::Native"

func matchedRouteSpec(properties map[string]any, match []any, attrs map[string]map[string]any) resource.Spec {
	config := map[string]any{nativePropertiesKey: properties}
	if match != nil {
		config[nativeMatchKey] = match
	}
	return resource.Spec{Binding: "ROUTE", Name: "kraai-e-s-route", Config: config,
		References: map[string]string{"API": apiProducer}, Attributes: attrs}
}

func newRoute(cc *fakeClient) *nativeResource {
	cc.schema = routeFacts
	return newNativeResourceWith(cc, staticSchemas{"type": "object"}, routeFacts, resource.LookupByAttr)
}

func TestNativeValidateMatch(t *testing.T) {
	route := newRoute(&fakeClient{})
	props := map[string]any{"ApiId": "${API.ApiId}", "RouteKey": "GET /x", "Secret": "s", "RouteId": "r"}
	for name, c := range map[string]struct {
		spec    resource.Spec
		wantErr string
	}{
		"declared and set":                 {spec: matchedRouteSpec(map[string]any{"ApiId": "${API.ApiId}", "RouteKey": "GET /x"}, []any{"RouteKey"}, nil)},
		"a reference to an identifier":     {spec: matchedRouteSpec(map[string]any{"ApiId": "${API.ApiId}", "RouteKey": "GET /x"}, []any{"ApiId", "RouteKey"}, nil)},
		"none declared":                    {spec: matchedRouteSpec(map[string]any{"ApiId": "a"}, nil, nil), wantErr: "declare match"},
		"not set":                          {spec: matchedRouteSpec(map[string]any{"ApiId": "a"}, []any{"RouteKey"}, nil), wantErr: "properties does not set"},
		"assigned by the vendor":           {spec: matchedRouteSpec(props, []any{"RouteId"}, nil), wantErr: "assigns itself"},
		"never returned by a read":         {spec: matchedRouteSpec(props, []any{"Secret"}, nil), wantErr: "never returns"},
		"a reference an update can change": {spec: matchedRouteSpec(map[string]any{"ApiId": "a", "RouteKey": "${API.Name}"}, []any{"RouteKey"}, nil), wantErr: "an update of API can change"},
	} {
		t.Run(name, func(t *testing.T) {
			err := route.validateMatch(c.spec, c.spec.Config[nativePropertiesKey].(map[string]any))
			if c.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want %q", err, c.wantErr)
			}
		})
	}

	queue := newFixtureNative(t, TypeSQSQueue, &fakeClient{})
	spec := nativeSpec("q", map[string]any{"DelaySeconds": 5})
	spec.Config[nativeMatchKey] = []any{"DelaySeconds"}
	if err := queue.ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "found by its tag") {
		t.Fatalf("match on a tagged type: %v", err)
	}
}

func TestNativeLocateWithMatch(t *testing.T) {
	route := newRoute(&fakeClient{})
	published := map[string]map[string]any{"API." + apiProducer: {"ApiId": "a1"}}
	props := map[string]any{"ApiId": "${API.ApiId}", "RouteKey": "GET /x"}

	scope, match, known, err := route.Locate(matchedRouteSpec(props, []any{"RouteKey"}, published))
	if err != nil || !known || scope != `{"ApiId":"a1"}` || match != `{"RouteKey":"GET /x"}` {
		t.Fatalf("Locate = %q, %q, %v, %v", scope, match, known, err)
	}
	if _, _, known, err := route.Locate(matchedRouteSpec(props, []any{"RouteKey"}, nil)); err != nil || known {
		t.Fatalf("Locate with the API not created = known %v, %v; want not known", known, err)
	}
	// The parent known, a match value naming something not created yet.
	pendingMatch := map[string]any{"ApiId": "a1", "RouteKey": "${API.ApiId}"}
	if scope, _, known, err := route.Locate(matchedRouteSpec(pendingMatch, []any{"RouteKey"}, nil)); err != nil || known || scope != "" {
		t.Fatalf("Locate with a pending match value = %q, known %v, %v; want not known", scope, known, err)
	}
}

// Every entry found by its match values carries the note, create-only keys
// included: an edited value finds nothing whatever the key.
func TestNativeNotesOnMatchedEntriesOnly(t *testing.T) {
	route := newRoute(&fakeClient{})
	notes := route.Notes(matchedRouteSpec(map[string]any{"ApiId": "a"}, []any{"ApiId"}, nil))
	if len(notes) != 1 || !strings.Contains(notes[0], "found by ApiId") || !strings.Contains(notes[0], "leaves the old one unmanaged") {
		t.Fatalf("notes = %v", notes)
	}
	if notes := newFixtureNative(t, TypeSQSQueue, &fakeClient{}).Notes(nativeSpec("q", nil)); notes != nil {
		t.Fatalf("a tagged type has notes %v", notes)
	}
}

// Found by its declared values under its parent: one match is found, none
// is absent, and two is an error, never the first of them.
func TestResolveByDeclaredMatch(t *testing.T) {
	ref := resource.Ref{Provider: Provider, Type: resource.RoleType(routeFacts.TypeName, nativeRole), Name: "kraai-e-s-route",
		Scope: `{"ApiId":"a1"}`, Match: `{"RouteKey":"GET /x","Weight":5}`}
	mine := map[string]any{"RouteKey": "GET /x", "Weight": 5.0}
	other := map[string]any{"RouteKey": "GET /y", "Weight": 5.0}

	cc := &fakeClient{list: []string{"r1", "r2"}, byIdentifier: map[string]map[string]any{"r1": other, "r2": mine}}
	state, err := newRoute(cc).Get(context.Background(), ref)
	if err != nil || state == nil || state.ID != "r2" {
		t.Fatalf("Get = %+v, %v; want r2", state, err)
	}
	if got := cc.listModels[0]["ApiId"]; got != "a1" {
		t.Fatalf("listed under %v, want a1", got)
	}

	cc = &fakeClient{list: []string{"r1"}, byIdentifier: map[string]map[string]any{"r1": other}}
	if state, err := newRoute(cc).Get(context.Background(), ref); err != nil || state != nil {
		t.Fatalf("Get with no match = %+v, %v; want absent", state, err)
	}

	cc = &fakeClient{list: []string{"r1", "r2"}, byIdentifier: map[string]map[string]any{"r1": mine, "r2": mine}}
	if _, err := newRoute(cc).Get(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "2 instances carry") {
		t.Fatalf("Get with two matches = %v, want an error", err)
	}

	unmatched := ref
	unmatched.Match = ""
	if _, err := newRoute(&fakeClient{}).Get(context.Background(), unmatched); err == nil || !strings.Contains(err.Error(), "none were given") {
		t.Fatalf("Get without match values = %v", err)
	}
}

// A created instance that reads back a match property differently could
// never be found again: Create fails loudly instead of letting every later
// apply make another copy.
func TestNativeCreateChecksTheMatchReadsBack(t *testing.T) {
	published := map[string]map[string]any{"API." + apiProducer: {"ApiId": "a1"}}
	props := map[string]any{"ApiId": "${API.ApiId}", "RouteKey": "GET /x"}

	cc := &fakeClient{createID: "r1", createProps: map[string]any{"ApiId": "a1", "RouteKey": "GET /x"}}
	if _, err := newRoute(cc).Create(context.Background(), matchedRouteSpec(props, []any{"RouteKey"}, published)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A manifest integer and the float a read decodes are one JSON value.
	weighted := map[string]any{"ApiId": "${API.ApiId}", "Weight": 5}
	cc = &fakeClient{createID: "r1", createProps: map[string]any{"ApiId": "a1", "Weight": 5.0}}
	if _, err := newRoute(cc).Create(context.Background(), matchedRouteSpec(weighted, []any{"Weight"}, published)); err != nil {
		t.Fatalf("Create matched on an integer: %v", err)
	}

	cc = &fakeClient{createID: "r1", createProps: map[string]any{"ApiId": "a1", "RouteKey": "get /x"}}
	_, err := newRoute(cc).Create(context.Background(), matchedRouteSpec(props, []any{"RouteKey"}, published))
	if err == nil || !strings.Contains(err.Error(), "could never be found again") {
		t.Fatalf("Create with a match that reads back differently = %v", err)
	}
}
