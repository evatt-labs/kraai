// Package manifest loads kraai's manifest, a directory rather than a single
// file so a large service set stays reviewable and diffable in a PR:
// kraai.yaml at the manifest root plus every services/*.yaml file, merged
// into one resolved, validated, strongly-typed Manifest for a given
// environment name.
//
// Load order matters and is fixed:
//
//  1. environments/<name>.values.yaml is loaded as free-form data (never
//     schema-validated) and merged with CLI --set overrides, Helm's exact
//     precedence: --set wins over the values file.
//  2. That merged values map becomes the template context for any
//     kraai.yaml.j2 / services/*.yaml.j2 file. Templating is opt-in by file
//     extension only — a plain .yaml file is never templated. Rendering
//     happens before YAML parsing; a template error fails loudly at render
//     time rather than silently producing broken YAML.
//  3. Every schema-validated file (kraai.yaml, services/*.yaml,
//     environments/<name>.yaml, rendered or not) is decoded strictly:
//     unknown keys are rejected at every nesting level, with the failing
//     key's dotted/bracketed path in the error message.
//
// Every external system this package touches — the filesystem and the
// template engine — is reached through a small interface (FS,
// TemplateEngine) so loader tests never touch disk or a real template
// engine unless they explicitly choose to.
package manifest
