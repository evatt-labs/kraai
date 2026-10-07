package aws

import "context"

// CreateResource creates an instance of desiredState through the backend
// routing.go picks, returning the identifier assigned and the resulting
// properties.
func (c *Client) CreateResource(ctx context.Context, typeName string, desiredState map[string]any) (string, map[string]any, error) {
	// Marked before the call: a create that fails part way may still have
	// made the instance.
	c.created.Store(typeName, true)
	// Before and after: a read of this type in flight during the call must
	// not repopulate the cache with the world as it was.
	c.reads.forget(typeName)
	defer c.reads.forget(typeName)
	return c.createBackend(ctx, typeName, desiredState).create(ctx, typeName, desiredState)
}

// UpdateResource applies patch, an RFC 6902 JSON Patch document, to
// identifier through the backend routing.go picks, returning the resulting
// properties.
func (c *Client) UpdateResource(ctx context.Context, typeName, identifier string, patch []byte) (map[string]any, error) {
	c.reads.forget(typeName)
	defer c.reads.forget(typeName)
	b, current, changes, err := c.updateBackend(ctx, typeName, identifier, patch)
	if err != nil {
		return nil, err
	}
	return b.update(ctx, typeName, identifier, patch, current, changes)
}

// DeleteResource deletes identifier through the backend routing.go picks.
// Deleting something already absent is success.
func (c *Client) DeleteResource(ctx context.Context, typeName, identifier string) error {
	c.reads.forget(typeName)
	defer c.reads.forget(typeName)
	return c.deleteBackend(ctx, typeName, identifier).delete(ctx, typeName, identifier)
}
