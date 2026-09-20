package resource

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// messagePrinter formats a jsonschema ErrorKind's LocalizedString. English
// only — kraai has no localization story anywhere else in the tree, and
// this exists only to satisfy the library's signature, not to add one.
var messagePrinter = message.NewPrinter(language.English)

// Schema is a structural JSON Schema (2020-12) that validates one shape of
// data a manifest can carry for a capability: CapabilityDef.ProviderSettings
// (a provider's `providers.<name>.settings` map) or CapabilityDef.Binding
// (one entry of a service's `<name>[]` bindings).
//
// JSON Schema rather than OpenAPI: OpenAPI 3.1 schemas are JSON Schema
// 2020-12, so nothing is lost expressively, and kraai already consumes JSON
// Schema elsewhere (the CloudFormation resource provider schemas fetched
// for primaryIdentifier and createOnlyProperties).
//
// A provider package builds a Schema as Go data (map[string]any), not an
// embedded .json file, so it lives next to the code whose settings it
// describes and is covered by gofmt/go vet like everything else.
//
// Compilation and structural-schema validation happen at most once per
// Schema value, memoized behind ensureCompiled's sync.Once — a provider
// package builds each Schema as a package-level var, so "once" means once
// per process, not once per manifest entry.
type Schema struct {
	// label names what this schema validates, for its error messages —
	// "aws compute settings", "neon database settings". Matches the prefix
	// the retired internal/provider/aws/settings_validate.go used
	// ("aws provider settings: ..."), generalized to any provider and
	// capability rather than hand-written per package.
	label string
	doc   map[string]any

	once       sync.Once
	compileErr error
	compiled   *jsonschema.Schema
	// properties are this schema's top-level property names, sorted —
	// computed once alongside compiled, and reused by Validate for both the
	// "recognized keys" list and the "did you mean" suggestion.
	properties []string
}

// NewSchema wraps doc, a JSON Schema document expressed as Go data, as a
// Schema labelled label for its error messages.
//
// doc is not compiled, and not checked for structural-schema validity,
// until the first call to Validate or until a caller building a Catalog
// (NewCatalog) forces it — see ensureCompiled's own doc comment for why
// compilation is deferred rather than eager here specifically, and
// Catalog.add for where "at registration" actually happens for a schema
// reached through a CapabilityDef.
//
// doc must not be mutated after it is passed to NewSchema: Schema reads it
// lazily, on first use, not at construction.
func NewSchema(label string, doc map[string]any) *Schema {
	return &Schema{label: label, doc: doc}
}

// ensureCompiled compiles and structurally validates s, memoizing the
// result behind sync.Once so repeated calls (from Validate, and from every
// Catalog.add that shares this Schema across capabilities) do the work
// exactly once.
//
// NewSchema returns no error, matching CapabilityDef's own no-error
// construction. A compile failure is a bug in the provider package that
// wrote the schema, not a runtime condition, so Catalog.add forces
// compilation at the same point it already catches a duplicate capability
// name or an empty Name — registration time, not first use — rather than
// introducing a second, panic-based failure mode for schemas alone.
func (s *Schema) ensureCompiled() error {
	s.once.Do(func() {
		s.compileErr = s.compileNow()
	})
	return s.compileErr
}

// schemaResourceID is the URL AddResource registers doc under before
// compiling it. Never dereferenced over the network — every Schema builds
// its own jsonschema.Compiler, so this only has to be unique within that
// one compiler instance, not globally.
const schemaResourceID = "mem://kraai/schema"

func (s *Schema) compileNow() error {
	if err := validateStructural(s.doc); err != nil {
		return kerrors.Wrap(err, kerrors.CodeValidation, "%s schema is not structural", s.label)
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaResourceID, s.doc); err != nil {
		return kerrors.Wrap(err, kerrors.CodeValidation, "%s schema is invalid JSON Schema", s.label)
	}
	compiled, err := c.Compile(schemaResourceID)
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeValidation, "%s schema is invalid JSON Schema", s.label)
	}

	s.compiled = compiled
	s.properties = topLevelProperties(s.doc)
	return nil
}

// Validate reports whether data satisfies s, returning nil when it does.
//
// A nil data is treated as an empty object rather than JSON null: a
// manifest entry that declares no settings at all (Spec.Config["settings"]
// absent) is "nothing was said," not "null was said," and a schema with no
// required properties must accept that the same way an empty YAML mapping
// would.
func (s *Schema) Validate(data map[string]any) error {
	if err := s.ensureCompiled(); err != nil {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}

	err := s.compiled.Validate(data)
	if err == nil {
		return nil
	}

	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		// Not expected from this library's Validate, but handled rather
		// than assumed away: a caller still gets a real error naming this
		// schema, not a silently swallowed one.
		return kerrors.Wrap(err, kerrors.CodeValidation, "%s", s.label)
	}

	// The unrecognized-key case gets the hand-crafted message this
	// mechanism exists to generalize — see unrecognizedKeyError's own doc
	// comment. Every other failure (wrong type, missing required, an enum
	// value not in the allowed set) falls through to the library's own
	// tree-formatted LocalizedError, still labelled, so it is never left
	// unexplained — Validate never returns an error that does not name
	// s.label.
	if unknown := rootUnrecognizedKeys(verr); len(unknown) > 0 {
		return s.unrecognizedKeyError(unknown)
	}
	return kerrors.Validation("%s: %s", s.label, formatValidationFailures(verr))
}

// formatValidationFailures flattens verr's Causes tree into one
// semicolon-joined line, one entry per leaf failure ("<path>: <message>",
// "root" for the document itself) — a smaller, purpose-built alternative
// to jsonschema.ValidationError's own Error(), which prefixes every
// message with a "jsonschema validation failed with '<schema URL>'"
// preamble that means nothing to a manifest author and duplicates the
// label this function's caller already attaches.
//
// kind.Group and kind.Schema are jsonschema/v6's own wrapper kinds — a
// Group has no message of its own, only Causes, and a Schema kind marks
// "this whole sub-schema failed" one level above the specific keyword
// that actually did; both are skipped so only the leaf, keyword-specific
// failures a manifest author can act on are reported.
func formatValidationFailures(verr *jsonschema.ValidationError) string {
	seen := map[string]bool{}
	var lines []string
	walkValidationErrors(verr, func(n *jsonschema.ValidationError) {
		switch n.ErrorKind.(type) {
		case *kind.Group, *kind.Schema:
			return
		}
		path := "root"
		if len(n.InstanceLocation) > 0 {
			path = strings.Join(n.InstanceLocation, ".")
		}
		line := path + ": " + n.ErrorKind.LocalizedString(messagePrinter)
		if seen[line] {
			return
		}
		seen[line] = true
		lines = append(lines, line)
	})
	sort.Strings(lines)
	if len(lines) == 0 {
		// Defensive: every ValidationError this package has seen carries at
		// least one non-Group, non-Schema cause. Falling back to the
		// library's own formatting rather than an empty string keeps this
		// unreachable-in-practice branch from ever hiding a real failure.
		return strings.TrimSpace(verr.Error())
	}
	return strings.Join(lines, "; ")
}

// walkValidationErrors calls visit for verr and, recursively, for every
// entry in its Causes tree — the shape jsonschema/v6 builds one failed
// Validate call into (see that package's ValidationError.Causes).
func walkValidationErrors(verr *jsonschema.ValidationError, visit func(*jsonschema.ValidationError)) {
	visit(verr)
	for _, cause := range verr.Causes {
		walkValidationErrors(cause, visit)
	}
}

// rootUnrecognizedKeys collects every property name an additionalProperties
// violation reported at the root of the validated document — data's own
// top-level keys, not a nested object's.
//
// Scoped to the root deliberately: every schema this workstream writes is a
// flat property bag (a provider's settings block, or one binding entry),
// so additionalProperties: false only ever appears at the top level, and
// this package's own structural-schema check does not require otherwise.
// A nested additionalProperties violation — were a future schema to add
// one — still fails Validate, just through the generic LocalizedError path
// below rather than this suggestion-bearing one, since a useful "did you
// mean" needs the violating node's own property list, not the root
// schema's.
func rootUnrecognizedKeys(verr *jsonschema.ValidationError) []string {
	var unknown []string
	walkValidationErrors(verr, func(n *jsonschema.ValidationError) {
		ap, ok := n.ErrorKind.(*kind.AdditionalProperties)
		if !ok || len(n.InstanceLocation) != 0 {
			return
		}
		unknown = append(unknown, ap.Properties...)
	})
	sort.Strings(unknown)
	return unknown
}

// unrecognizedKeyError builds the generic form of the message
// settings_validate.go used to hand-write per provider:
//
//	aws provider settings: unrecognized key(s): reservedConcurency
//	  (did you mean reservedConcurrency?) — recognized keys: architecture,
//	  env, envSecrets, functionUrlAuthType, httpFrontDoor, layerArn, ...
//
// Generic here because it reads s.properties — this schema's own top-level
// property names, computed once in compileNow from the same doc a provider
// wrote for any capability of any provider — rather than a hand-assembled
// union of key lists specific to one vendor's settings decoders.
func (s *Schema) unrecognizedKeyError(unknown []string) error {
	msgs := make([]string, len(unknown))
	for i, key := range unknown {
		if match := closestKey(key, s.properties); match != "" {
			msgs[i] = key + " (did you mean " + match + "?)"
		} else {
			msgs[i] = key
		}
	}
	recognized := "(none)"
	if len(s.properties) > 0 {
		recognized = strings.Join(s.properties, ", ")
	}
	return kerrors.Validation(
		"%s: unrecognized key(s): %s — recognized keys: %s",
		s.label, strings.Join(msgs, ", "), recognized)
}

// hasProperty reports whether name is one of s's top-level properties.
// Read from the document rather than the compiled schema so it can answer
// before compilation, which is when Catalog.add asks.
func (s *Schema) hasProperty(name string) bool {
	props, _ := s.doc["properties"].(map[string]any)
	_, ok := props[name]
	return ok
}

// topLevelProperties returns doc's top-level "properties" key names,
// sorted — deterministic across map iteration order, exactly as the
// retired validateKnownSettings sorted allKnown before formatting it.
func topLevelProperties(doc map[string]any) []string {
	props, _ := doc["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// closestKey returns the entry in candidates within Levenshtein distance 2
// of key, preferring the nearest; empty when nothing is close enough to be
// a plausible typo suggestion rather than noise for a key that is simply
// not supported at all.
//
// Moved here from internal/provider/aws/settings_validate.go verbatim
// (algorithm unchanged) — this is the generalization the proposal names:
// "the suggestion should now come for free everywhere," not a second,
// per-provider copy.
func closestKey(key string, candidates []string) string {
	const maxSuggestDistance = 2
	best := ""
	bestDist := maxSuggestDistance + 1
	for _, c := range candidates {
		d := levenshtein(key, c)
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	if bestDist > maxSuggestDistance {
		return ""
	}
	return best
}

// levenshtein returns the edit distance between a and b (single-character
// insert/delete/substitute), via the standard O(len(a)*len(b)) dynamic
// program. Hand-rolled rather than a dependency, same reasoning as the
// function this was copied from: it is the entire algorithm, used for one
// purpose, and not worth a third-party import for.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// validateStructural enforces kraai's structural-schema constraint — every
// schema node states its own "type", and the root schema declares neither
// "oneOf" nor "anyOf" — a narrower subset of Kubernetes' own
// structural-schema rule.
//
// Every node typed means a schema can never validate a value whose shape
// is ambiguous. No root oneOf/anyOf means the root document has exactly
// one way to be valid, which is what makes a single, deterministic
// "recognized keys" list — and therefore the unrecognized-key suggestion —
// possible at all. Nested oneOf/anyOf are not rejected: no schema here
// uses one, and rejecting a combinator nothing uses would be speculative
// scope.
//
// Enforced here rather than by the jsonschema/v6 compiler itself: the
// compiler accepts any valid JSON Schema, and structural is kraai's own,
// stricter contract on top of that.
func validateStructural(doc map[string]any) error {
	if err := validateStructuralNode(doc, ""); err != nil {
		return err
	}
	for _, combinator := range []string{"oneOf", "anyOf"} {
		if _, ok := doc[combinator]; ok {
			return kerrors.Validation(
				"schema declares a top-level %q, which a structural schema forbids — "+
					"give every branch its own named property instead", combinator)
		}
	}
	return nil
}

// validateStructuralNode requires node to declare "type", then recurses
// into every nested schema node reachable from it: each entry of
// "properties", "items" (an array's element schema), and
// "additionalProperties" when it is itself a schema rather than a bare
// true/false.
func validateStructuralNode(node map[string]any, path string) error {
	if _, ok := node["type"]; !ok {
		label := "the schema"
		if path != "" {
			label = "node " + path
		}
		return kerrors.Validation("%s declares no \"type\"", label)
	}

	if props, ok := node["properties"].(map[string]any); ok {
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child, ok := props[name].(map[string]any)
			if !ok {
				return kerrors.Validation("property %q at %s is not itself a schema object", name, nodeLabel(path))
			}
			if err := validateStructuralNode(child, joinSchemaPath(path, name)); err != nil {
				return err
			}
		}
	}

	if items, ok := node["items"].(map[string]any); ok {
		if err := validateStructuralNode(items, path+"[]"); err != nil {
			return err
		}
	}

	if ap, ok := node["additionalProperties"].(map[string]any); ok {
		if err := validateStructuralNode(ap, path+".*"); err != nil {
			return err
		}
	}

	return nil
}

func nodeLabel(path string) string {
	if path == "" {
		return "the root schema"
	}
	return path
}

func joinSchemaPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
