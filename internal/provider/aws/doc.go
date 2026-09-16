// Package aws adapts the AWS Cloud Control API to the resource contract.
//
// # One engine, not one client per service
//
// Cloud Control exposes uniform GetResource/CreateResource/UpdateResource/
// DeleteResource/ListResources across 1,598 FULLY_MUTABLE public resource
// types, which maps 1:1 onto internal/resource's per-verb Resource interface
// (verified against a live account, 2026-09-13). So this package is one
// generic Resource implementation, parameterized per registration by its
// CloudFormation TypeName and its identity lookup strategy; adding a
// resource type is a registry entry, never a new client.
//
// # The write path
//
// Create, Update and Delete are real. Create submits spec.Config as Cloud
// Control's desired state (stamping the identity tag into it first for a
// byTag type, per D26) and polls the resulting ProgressEvent to a terminal
// state. Update fetches the current schema; a type with no update handler
// (IMMUTABLE provisioning: create/read/delete only) refuses with
// resource.ErrImmutable rather than attempting a call Cloud Control would
// reject, and everything else diffs current properties against spec.Config
// into an RFC 6902 JSON Patch document, submits it, and polls to terminal.
// Delete treats an already-absent resource as success, both when resolve
// finds no identifier and when Cloud Control's own delete reports the
// resource gone — the same absence-is-success contract Get already holds.
//
// Async polling, JSON Patch emission and createOnlyProperties-driven
// replacement detection are shared, generic mechanisms (client.go's
// pollToTerminal, patch.go's buildPatch, resource.go's DiffersFromState) —
// no per-type write logic exists, matching the read path's one-engine
// design.
package aws
