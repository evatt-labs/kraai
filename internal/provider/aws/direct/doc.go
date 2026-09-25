// Package direct reads AWS resources through their own service APIs rather
// than Cloud Control, from code generated out of two published sources: the
// CloudFormation resource schemas and the Smithy API models the AWS SDKs
// are built from.
//
// Every type is described by a reviewed override file naming the operation
// that reads it and how its properties map; nothing is inferred at build
// time. The models and schemas the overrides need are extracted once, at a
// pinned model commit, into a checked-in subset whose hashes lock.json
// records, so generating needs neither the network nor credentials.
//
// Outside this package only lists are used: a type with a direct list is
// listed through it on every lookup, never through Cloud Control. Reads and
// every mutation still go through Cloud Control.
package direct
