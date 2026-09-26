package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
)

// fakeClient is ccAPI, hand-rolled — no AWS account, no network needed.
type fakeClient struct {
	// byIdentifier answers GetResource. A missing key means "not found",
	// distinct from an entry mapping to an error.
	byIdentifier map[string]map[string]any
	getErr       map[string]error
	list         []string
	// listByType, when set for a type, answers ListResources for it instead
	// of list: Cloud Control lists per type, and a test resolving two types
	// through one fake needs each to see only its own identifiers.
	listByType map[string][]string
	listErr    error
	getCalls   []string
	listCalls  int
	// created is what Created reports.
	created map[string]bool
	// listModels records the resourceModel passed to every ListResources
	// call, in order, so a test can assert a parent-scoped type's request
	// actually carried the right scope (or that a non-parent-scoped type's
	// request carried none at all).
	listModels []map[string]any

	createID    string
	createProps map[string]any
	createErr   error
	createCalls []map[string]any
	// createOrder records the typeName of every CreateResource call, in
	// order, so a test can assert two types were created in the right
	// sequence (cloudfront_test.go's OAC-before-distribution).
	createOrder []string
	// createResults, keyed by typeName, overrides createID/createProps/
	// createErr for a call against that specific type — needed once a
	// single fakeClient creates more than one type in the same test (the
	// CloudFront composite resource creates both an OriginAccessControl
	// and a Distribution through the same client). A type with no entry
	// here falls back to createID/createProps/createErr above, which is
	// what every existing, single-type test in this file already relies
	// on.
	createResults map[string]struct {
		id    string
		props map[string]any
		err   error
	}

	updateProps   map[string]any
	updateErr     error
	updateCalls   []string
	updatePatches [][]byte

	deleteErr   error
	deleteCalls []string

	schema      cfschema.Facts
	schemaErr   error
	schemaCalls int

	// tagged answers TaggedResources by tag value; taggedErr fails it.
	tagged      map[string][]string
	taggedErr   error
	taggedCalls int
	taggedTypes []string
}

func (f *fakeClient) Created(typeName string) bool { return f.created[typeName] }

func (f *fakeClient) TaggedResources(_ context.Context, name, tagType string) ([]string, error) {
	f.taggedCalls++
	f.taggedTypes = append(f.taggedTypes, tagType)
	if f.taggedErr != nil {
		return nil, f.taggedErr
	}
	return f.tagged[name], nil
}

func (f *fakeClient) GetResource(_ context.Context, _ string, identifier string) (map[string]any, bool, error) {
	f.getCalls = append(f.getCalls, identifier)
	if err, ok := f.getErr[identifier]; ok {
		return nil, false, err
	}
	props, ok := f.byIdentifier[identifier]
	if !ok {
		return nil, false, nil
	}
	return props, true, nil
}

func (f *fakeClient) ListResources(_ context.Context, typeName string, resourceModel map[string]any) ([]string, error) {
	f.listCalls++
	f.listModels = append(f.listModels, resourceModel)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if ids, ok := f.listByType[typeName]; ok {
		return ids, nil
	}
	return f.list, nil
}

func (f *fakeClient) CreateResource(_ context.Context, typeName string, desiredState map[string]any) (string, map[string]any, error) {
	f.createCalls = append(f.createCalls, desiredState)
	f.createOrder = append(f.createOrder, typeName)
	if result, ok := f.createResults[typeName]; ok {
		if result.err != nil {
			return "", nil, result.err
		}
		return result.id, result.props, nil
	}
	if f.createErr != nil {
		return "", nil, f.createErr
	}
	return f.createID, f.createProps, nil
}

func (f *fakeClient) UpdateResource(_ context.Context, _ string, identifier string, patch []byte) (map[string]any, error) {
	f.updateCalls = append(f.updateCalls, identifier)
	f.updatePatches = append(f.updatePatches, patch)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updateProps, nil
}

func (f *fakeClient) DeleteResource(_ context.Context, _ string, identifier string) error {
	f.deleteCalls = append(f.deleteCalls, identifier)
	return f.deleteErr
}

func (f *fakeClient) DescribeType(context.Context, string) (cfschema.Facts, error) {
	f.schemaCalls++
	if f.schemaErr != nil {
		return cfschema.Facts{}, f.schemaErr
	}
	return f.schema, nil
}

func matchNameField(properties map[string]any, name string) bool {
	v, _ := properties["Name"].(string)
	return v == name
}
