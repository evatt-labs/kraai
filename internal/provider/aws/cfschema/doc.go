// Package cfschema decodes CloudFormation resource provider schemas and
// derives from each what the aws provider needs to address a type
// generically: how instances are identified, where a tag goes and in what
// shape, which properties force replacement, what a list call must be
// scoped by, and the IAM actions the handlers declare. A generated index
// carries those facts for every AWS-published provisionable type, so the
// questions are answered without credentials; the full schema is still
// fetched when property validation needs it.
package cfschema
