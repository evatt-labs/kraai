package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"regexp"

	"github.com/evatt-labs/kraai/internal/redact"
	"github.com/evatt-labs/kraai/internal/resource"
)

// maxShownValue bounds how much of one property value a plan prints, so a
// policy document or a function's environment cannot swamp the output.
const maxShownValue = 120

// tokenShapes are the shapes of credentials a free-text field may carry
// although nothing marked them sensitive, each replaced where it appears:
// AWS access key IDs, GitHub tokens, a connection URI's password, and a
// PEM private key block.
var tokenShapes = []struct {
	pattern *regexp.Regexp
	with    string
}{
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "[aws access key]"},
	{regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b`), "[github token]"},
	{regexp.MustCompile(`(://[^/\s:@]+):[^@\s/]+@`), "${1}:[password]@"},
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), "[private key]"},
}

// shown is what a plan prints of a value it did not render itself, a note
// or an error: every value the command must not print replaced, and every
// credential shape scrubbed.
func shown(ctx context.Context, text string) string {
	text = redact.From(ctx).String(text)
	for _, shape := range tokenShapes {
		text = shape.pattern.ReplaceAllString(text, shape.with)
	}
	return text
}

// shownValue renders a property value for a plan: its string leaves made
// safe one by one, before rendering, since a value JSON escapes ("\"" for
// ") no longer reads as written; then compact JSON, cut to maxShownValue,
// and the rendering made safe again. nil renders as "".
func shownValue(ctx context.Context, value any) string {
	if value == nil {
		return ""
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(safeLeaves(ctx, value)); err != nil {
		return "(unprintable)"
	}
	text := shown(ctx, string(bytes.TrimRight(buf.Bytes(), "\n")))
	if len(text) > maxShownValue {
		text = text[:maxShownValue-3] + "..."
	}
	return text
}

// safeLeaves is value with each string in it made safe.
func safeLeaves(ctx context.Context, value any) any {
	switch v := value.(type) {
	case string:
		return shown(ctx, v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[shown(ctx, k)] = safeLeaves(ctx, e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = safeLeaves(ctx, e)
		}
		return out
	default:
		return value
	}
}

// planChangeJSON is one property change in the plan document. Before and
// After are rendered values (shownValue); the one a kind has no value for
// is absent.
type planChangeJSON struct {
	Property string `json:"property"`
	Kind     string `json:"kind"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"`
}

// shownChanges renders changes for a plan. A property one side of which
// holds a value the command must not print is sensitive on both: the
// value it had is the earlier one of the same secret, which nothing marked
// sensitive since, so neither side is shown.
func shownChanges(ctx context.Context, changes []resource.Change) []planChangeJSON {
	if len(changes) == 0 {
		return nil
	}
	out := make([]planChangeJSON, 0, len(changes))
	for _, c := range changes {
		change := planChangeJSON{
			Property: shown(ctx, c.Property),
			Kind:     string(c.Kind),
			Before:   shownValue(ctx, c.Before),
			After:    shownValue(ctx, c.After),
		}
		if altered(ctx, c.Before) || altered(ctx, c.After) {
			if change.Before != "" {
				change.Before = sensitiveValue
			}
			if change.After != "" {
				change.After = sensitiveValue
			}
		}
		out = append(out, change)
	}
	return out
}

// sensitiveValue is shown in place of a sensitive property's value.
const sensitiveValue = "(sensitive)"

// altered reports a value showing it would change: one holding something
// the command must not print, or a credential shape.
func altered(ctx context.Context, value any) bool {
	if value == nil {
		return false
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return true
	}
	return !reflect.DeepEqual(safeLeaves(ctx, value), value) || shown(ctx, string(raw)) != string(raw)
}

// changeSymbol marks a change in the text plan.
func changeSymbol(kind string) string {
	switch resource.ChangeKind(kind) {
	case resource.ChangeAdd:
		return "+"
	case resource.ChangeRemove:
		return "-"
	case resource.ChangeUpdate:
		return "~"
	}
	return "?"
}

// changeText is a change's values for the text plan: ": before -> after"
// for a change, ": after" for an addition, ": before" for a removal.
func changeText(c planChangeJSON) string {
	switch {
	case c.Before != "" && c.After != "":
		return ": " + c.Before + " -> " + c.After
	case c.After != "":
		return ": " + c.After
	case c.Before != "":
		return ": " + c.Before
	}
	return ""
}
