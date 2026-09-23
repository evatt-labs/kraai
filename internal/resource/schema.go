package resource

import (
	"errors"
	"regexp"
	"slices"
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
// only; it exists to satisfy the library's signature, not to localize.
var messagePrinter = message.NewPrinter(language.English)

// Schema is a structural JSON Schema (2020-12) that validates one shape of
// data a manifest can carry for a capability: a provider's settings map
// (CapabilityDef.ProviderSettings) or one binding entry
// (CapabilityDef.Binding).
//
// A provider package builds a Schema as Go data next to the code it
// describes. It is compiled and structurally checked once per Schema value,
// on first use or when a Catalog is built, whichever comes first.
type Schema struct {
	// label names what this schema validates in its error messages: "aws
	// compute settings", "neon database settings".
	label string
	doc   map[string]any
	// vendor marks a schema the vendor published rather than one kraai
	// wrote; see NewVendorSchema.
	vendor bool

	once       sync.Once
	compileErr error
	compiled   *jsonschema.Schema
	// properties are this schema's top-level property names, sorted, for
	// the "recognized keys" list and the "did you mean" suggestion.
	properties []string
}

// NewSchema wraps doc, a JSON Schema document expressed as Go data, as a
// Schema labelled label for its error messages.
//
// doc is read lazily, on first use, so it must not be mutated after this
// call. It is not compiled or checked here; see ensureCompiled.
func NewSchema(label string, doc map[string]any) *Schema {
	return &Schema{label: label, doc: doc}
}

// NewVendorSchema wraps a JSON Schema a vendor publishes, such as the
// properties of a CloudFormation resource type, for validating a native
// resource's properties with the same error messages as NewSchema.
//
// It is compiled as draft-07, the dialect CloudFormation schemas are
// written in, and is not held to the structural rule, which no vendor
// schema follows. A pattern Go's regexp cannot compile (a lookahead, a
// backreference) is not checked here; the vendor still enforces it.
func NewVendorSchema(label string, doc map[string]any) *Schema {
	return &Schema{label: label, doc: doc, vendor: true}
}

// Compile compiles s now rather than on first use, so a caller building
// schemas at run time can report a bad one where it was built.
func (s *Schema) Compile() error { return s.ensureCompiled() }

// ensureCompiled compiles and structurally validates s exactly once.
//
// NewSchema returns no error, matching CapabilityDef's own no-error
// construction, so a compile failure surfaces here instead. Catalog.add
// forces it at catalog construction, which makes a bad schema a registration
// error rather than a failure on the first manifest that exercises it.
func (s *Schema) ensureCompiled() error {
	s.once.Do(func() {
		s.compileErr = s.compileNow()
	})
	return s.compileErr
}

// schemaResourceID is the URL AddResource registers doc under. Never
// dereferenced; every Schema builds its own compiler, so it only has to be
// unique within one.
const schemaResourceID = "mem://kraai/schema"

func (s *Schema) compileNow() error {
	c := jsonschema.NewCompiler()
	if s.vendor {
		c.DefaultDraft(jsonschema.Draft7)
		c.UseRegexpEngine(lenientRegexp)
	} else if err := validateStructural(s.doc); err != nil {
		return kerrors.Wrap(err, kerrors.CodeValidation, "%s schema is not structural", s.label)
	}

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
// Every error it returns names s.label.
//
// nil data is an empty object, not JSON null: a manifest entry that declares
// no settings said nothing, and a schema with no required properties must
// accept that as it would an empty YAML mapping.
func (s *Schema) Validate(data map[string]any) error {
	return s.ValidateIgnoring(data, nil)
}

// ValidateIgnoring is Validate with every failure at or below one of ignore's
// instance paths dropped: for a value not known yet, such as a reference to
// a resource that does not exist, whose placeholder must not be judged
// against the schema.
func (s *Schema) ValidateIgnoring(data map[string]any, ignore [][]string) error {
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
		// Not expected from this library's Validate, but the caller still
		// gets an error naming this schema rather than a swallowed one.
		return kerrors.Wrap(err, kerrors.CodeValidation, "%s", s.label)
	}

	// Unrecognized keys get the suggestion-bearing message; every other
	// failure gets the library's own leaf messages, flattened.
	if unknown := rootUnrecognizedKeys(verr); len(unknown) > 0 {
		return s.unrecognizedKeyError(unknown)
	}
	failures := formatValidationFailures(verr, ignore)
	if failures == "" {
		return nil
	}
	return kerrors.Validation("%s: %s", s.label, failures)
}

// ignored reports whether location is at or below one of ignore's paths.
func ignored(location []string, ignore [][]string) bool {
	for _, path := range ignore {
		if len(location) >= len(path) && slices.Equal(location[:len(path)], path) {
			return true
		}
	}
	return false
}

// formatValidationFailures flattens verr's Causes tree into one
// semicolon-joined line of "<path>: <message>" leaf failures, "root" for the
// document itself. The library's own Error() prefixes every message with a
// schema URL preamble that means nothing to a manifest author.
//
// kind.Group and kind.Schema are wrapper kinds with no message of their own,
// so only the keyword-specific failures an author can act on are reported.
func formatValidationFailures(verr *jsonschema.ValidationError, ignore [][]string) string {
	seen := map[string]bool{}
	var lines []string
	walkUnignored(verr, ignore, func(n *jsonschema.ValidationError) {
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
	if len(lines) == 0 && len(ignore) == 0 {
		// Every ValidationError seen so far carries at least one leaf
		// cause; fall back to the library's formatting rather than hide a
		// failure behind an empty string.
		return strings.TrimSpace(verr.Error())
	}
	return strings.Join(lines, "; ")
}

// walkUnignored is walkValidationErrors without what ignore covers: a
// failure at or below an ignored path, and the whole of a oneOf or anyOf
// failure above one, since which branch matches depends on the value that
// is not known yet.
func walkUnignored(verr *jsonschema.ValidationError, ignore [][]string, visit func(*jsonschema.ValidationError)) {
	if ignored(verr.InstanceLocation, ignore) {
		return
	}
	switch verr.ErrorKind.(type) {
	case *kind.OneOf, *kind.AnyOf:
		if aboveIgnored(verr.InstanceLocation, ignore) {
			return
		}
	}
	visit(verr)
	for _, cause := range verr.Causes {
		walkUnignored(cause, ignore, visit)
	}
}

// aboveIgnored reports whether one of ignore's paths lies below location.
func aboveIgnored(location []string, ignore [][]string) bool {
	for _, path := range ignore {
		if len(path) > len(location) && slices.Equal(path[:len(location)], location) {
			return true
		}
	}
	return false
}

// walkValidationErrors calls visit for verr and, recursively, for every
// entry in its Causes tree.
func walkValidationErrors(verr *jsonschema.ValidationError, visit func(*jsonschema.ValidationError)) {
	visit(verr)
	for _, cause := range verr.Causes {
		walkValidationErrors(cause, visit)
	}
}

// rootUnrecognizedKeys collects every property name an additionalProperties
// violation reported at the root of the validated document.
//
// Root only: every schema here is a flat property bag, so that is the only
// place additionalProperties: false appears, and a useful "did you mean"
// needs the violating node's own property list. A nested violation still
// fails Validate, through the generic path.
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

// unrecognizedKeyError names each unknown key, with the closest recognized
// key as a suggestion where one is near enough, and lists every recognized
// key:
//
//	aws provider settings: unrecognized key(s): reservedConcurency
//	  (did you mean reservedConcurrency?) — recognized keys: architecture,
//	  env, envSecrets, functionUrlAuthType, httpFrontDoor, layerArn, ...
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

// HasProperty reports whether name is one of s's top-level properties. Read
// from the document rather than the compiled schema so it can answer before
// compilation, which is when Catalog.add asks.
func (s *Schema) HasProperty(name string) bool {
	props, _ := s.doc["properties"].(map[string]any)
	_, ok := props[name]
	return ok
}

// topLevelProperties returns doc's top-level "properties" key names, sorted.
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
// of key, preferring the nearest; empty when nothing is close enough to be a
// plausible typo rather than noise for a key that is simply unsupported.
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

// levenshtein returns the edit distance between a and b. Hand-rolled: it is
// the entire algorithm, used for one purpose.
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

// validateStructural enforces kraai's structural-schema rule, a narrower
// subset of Kubernetes': every node states its own "type", and the root
// declares neither "oneOf" nor "anyOf". A typed node can never validate a
// value of ambiguous shape, and a root with one way to be valid is what makes
// a single, deterministic "recognized keys" list possible. Nested
// combinators are not rejected; nothing uses one.
//
// Enforced here because the jsonschema compiler accepts any valid JSON
// Schema, and structural is kraai's stricter contract on top.
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

// validateStructuralNode requires node to declare "type", then recurses into
// every nested schema: each "properties" entry, "items", and
// "additionalProperties" when it is a schema rather than a bare boolean.
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

// lenientRegexp compiles a vendor schema's pattern with Go's regexp, and
// matches everything when that fails: the vendor's patterns are written for
// an ECMA or Java engine, and one Go cannot compile must not make the whole
// type unusable.
func lenientRegexp(pattern string) (jsonschema.Regexp, error) {
	if re, err := regexp.Compile(pattern); err == nil {
		return re, nil
	}
	return unchecked(pattern), nil
}

// unchecked is a pattern Go's regexp cannot compile, accepted unmatched.
type unchecked string

func (u unchecked) String() string            { return string(u) }
func (u unchecked) MatchString(_ string) bool { return true }
