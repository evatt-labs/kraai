// Package manifest loads kraai's manifest: kraai.yaml at the root plus every
// services/*.yaml file, merged into one resolved, validated Manifest for an
// environment name. A directory rather than one file, so a large service
// set stays reviewable in a PR.
//
// Load order is fixed. environments/<name>.values.yaml is loaded as
// free-form data and merged with --set overrides, which win. That map is
// the template context for any kraai.yaml.j2 or services/*.yaml.j2 file;
// templating is opt-in by extension, and a plain .yaml is never templated.
// Every schema-validated file is then decoded strictly, rejecting unknown
// keys at every level with the failing key's path.
//
// The filesystem and the template engine are reached through the FS and
// TemplateEngine interfaces, so loader tests never touch disk.
package manifest
