// Package aws adapts the AWS Cloud Control API to the resource contract.
//
// Cloud Control exposes uniform Get, Create, Update, Delete and List across
// its resource types, which maps one to one onto internal/resource's
// per-verb interface. This package is therefore one generic Resource
// implementation (resourceType), parameterized per registration by its
// CloudFormation TypeName and its identity lookup strategy, and adding a
// type is a registry entry rather than a client. Per-type code exists only
// where the vendor's property vocabulary has to be built from the
// manifest's (a translate), or where a verb needs something Cloud Control
// cannot express (an S3 upload, a bucket policy).
//
// Create submits the desired state, stamping the identity tag into it first
// for a byTag type, and polls to a terminal state. Update refuses with
// resource.ErrImmutable for a type whose schema has no update handler, and
// otherwise submits an RFC 6902 patch. Delete treats an already-absent
// resource as success. Diff derives replace-versus-update from the type's
// own schema (createOnlyProperties, writeOnlyProperties, handlers), fetched
// once per type per process.
package aws
