package direct

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// ruleSet is a service's smithy.rules#endpointRuleSet, evaluated as the
// SDKs evaluate it rather than read from its URLs as written.
type ruleSet struct {
	Parameters map[string]ruleParameter `json:"parameters"`
	Rules      []endpointRule           `json:"rules"`
}

// ruleParameter declares one input to a rule set. BuiltIn names the client
// setting that fills it, such as AWS::Region; an operation fills the rest.
type ruleParameter struct {
	Type     string `json:"type"`
	BuiltIn  string `json:"builtIn"`
	Required bool   `json:"required"`
	Default  any    `json:"default"`
}

// endpointRule is an endpoint, error or tree rule: its conditions gate an endpoint,
// an error, or the rules nested under it.
type endpointRule struct {
	Type       string          `json:"type"`
	Conditions []ruleCondition `json:"conditions"`
	Endpoint   *ruleEndpoint   `json:"endpoint"`
	Error      any             `json:"error"`
	Rules      []endpointRule  `json:"rules"`
}

// ruleCondition is a function call whose result must be set and not false;
// Assign names that result for the conditions after it and the rules
// nested under the rule.
type ruleCondition struct {
	Fn     string `json:"fn"`
	Argv   []any  `json:"argv"`
	Assign string `json:"assign"`
}

type ruleEndpoint struct {
	URL        any            `json:"url"`
	Properties map[string]any `json:"properties"`
	Headers    map[string]any `json:"headers"`
}

// resolvedEndpoint is the endpoint a rule set gives one set of parameters.
type resolvedEndpoint struct {
	URL        string
	Properties map[string]any
	Headers    map[string][]string
}

// ruleError is an error rule's message: the rule set's own refusal of the
// parameters, as distinct from a rule set this evaluator cannot read.
type ruleError struct{ message string }

func (e *ruleError) Error() string { return e.message }

// parseRuleSet decodes a rule set, refusing one with no rules.
func parseRuleSet(raw json.RawMessage) (*ruleSet, error) {
	var rs ruleSet
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil, fmt.Errorf("the endpoint rule set does not decode: %w", err)
	}
	if len(rs.Rules) == 0 {
		return nil, fmt.Errorf("the model has no endpoint rule set")
	}
	return &rs, nil
}

// resolve evaluates the rule set for params, keyed by parameter name. A
// parameter params leaves out takes its default. The error is a
// *ruleError when the rule set itself refuses the parameters.
func (rs *ruleSet) resolve(params map[string]any) (*resolvedEndpoint, error) {
	scope := map[string]any{}
	for name, p := range rs.Parameters {
		v, ok := params[name]
		if !ok || v == nil {
			v = p.Default
		}
		if v == nil && p.Required {
			return nil, fmt.Errorf("the endpoint parameter %s is required and has no value", name)
		}
		if v != nil {
			scope[name] = v
		}
	}
	for name := range params {
		if _, ok := rs.Parameters[name]; !ok {
			return nil, fmt.Errorf("the rule set declares no endpoint parameter %s", name)
		}
	}
	e, matched, err := evalRules(rs.Rules, scope)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, fmt.Errorf("no endpoint rule matched")
	}
	return e, nil
}

// evalRules returns the endpoint of the first rule whose conditions hold,
// and whether any did. A tree rule whose conditions hold is terminal: none
// of its rules matching is an error, not a fall through to its siblings.
func evalRules(rules []endpointRule, scope map[string]any) (*resolvedEndpoint, bool, error) {
	for _, r := range rules {
		local, ok, err := evalConditions(r.Conditions, scope)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			continue
		}
		switch r.Type {
		case "endpoint":
			if r.Endpoint == nil {
				return nil, false, fmt.Errorf("an endpoint rule has no endpoint")
			}
			e, err := evalEndpoint(r.Endpoint, local)
			return e, err == nil, err
		case "error":
			msg, err := evalTemplateOrExpr(r.Error, local)
			if err != nil {
				return nil, false, err
			}
			s, ok := msg.(string)
			if !ok {
				return nil, false, fmt.Errorf("an error rule's message is not a string")
			}
			return nil, false, &ruleError{s}
		case "tree":
			e, matched, err := evalRules(r.Rules, local)
			if err != nil {
				return nil, false, err
			}
			if !matched {
				return nil, false, fmt.Errorf("a tree rule matched but none of its rules did")
			}
			return e, true, nil
		default:
			return nil, false, fmt.Errorf("unknown rule type %q", r.Type)
		}
	}
	return nil, false, nil
}

// evalConditions reports whether every condition holds, and the scope with
// the values they assign. The caller's scope is never written: an
// assignment is visible to the rule's later conditions and nested rules,
// not to its siblings.
func evalConditions(conds []ruleCondition, scope map[string]any) (map[string]any, bool, error) {
	local, cloned := scope, false
	for _, c := range conds {
		v, err := ruleFn(c.Fn, c.Argv, local)
		if err != nil {
			return nil, false, err
		}
		if v == nil || v == false {
			return nil, false, nil
		}
		if c.Assign != "" {
			if _, taken := local[c.Assign]; taken {
				return nil, false, fmt.Errorf("a condition assigns %s, which is already bound", c.Assign)
			}
			if !cloned {
				local, cloned = maps.Clone(scope), true
			}
			local[c.Assign] = v
		}
	}
	return local, true, nil
}

func evalEndpoint(e *ruleEndpoint, scope map[string]any) (*resolvedEndpoint, error) {
	u, err := evalTemplateOrExpr(e.URL, scope)
	if err != nil {
		return nil, err
	}
	url, ok := u.(string)
	if !ok {
		return nil, fmt.Errorf("an endpoint's URL is not a string")
	}
	out := &resolvedEndpoint{URL: url}
	if len(e.Properties) > 0 {
		props, err := evalValue(map[string]any(e.Properties), scope)
		if err != nil {
			return nil, err
		}
		out.Properties = props.(map[string]any)
	}
	for name, vs := range e.Headers {
		list, ok := vs.([]any)
		if !ok {
			return nil, fmt.Errorf("the header %s is not a list", name)
		}
		for _, v := range list {
			s, err := evalTemplateOrExpr(v, scope)
			if err != nil {
				return nil, err
			}
			str, ok := s.(string)
			if !ok {
				return nil, fmt.Errorf("a value of the header %s is not a string", name)
			}
			if out.Headers == nil {
				out.Headers = map[string][]string{}
			}
			out.Headers[name] = append(out.Headers[name], str)
		}
	}
	return out, nil
}

// evalValue evaluates an endpoint property: a string is a template, an
// object with ref or fn an expression, and objects and arrays are walked.
func evalValue(v any, scope map[string]any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["ref"]; ok {
			return evalExpr(t, scope)
		}
		if _, ok := t["fn"]; ok {
			return evalExpr(t, scope)
		}
		out := make(map[string]any, len(t))
		for k, child := range t {
			r, err := evalValue(child, scope)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			r, err := evalValue(child, scope)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case string:
		return template(t, scope)
	default:
		return v, nil
	}
}

// evalTemplateOrExpr evaluates a URL or error message: a string is a
// template, anything else an expression.
func evalTemplateOrExpr(v any, scope map[string]any) (any, error) {
	if s, ok := v.(string); ok {
		return template(s, scope)
	}
	return evalExpr(v, scope)
}

// evalExpr evaluates a function argument. A string argument is a template;
// a template with no braces is the literal it spells.
func evalExpr(v any, scope map[string]any) (any, error) {
	switch t := v.(type) {
	case string:
		return template(t, scope)
	case bool, float64, nil:
		return t, nil
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			r, err := evalExpr(child, scope)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		if name, ok := t["ref"].(string); ok {
			return scope[name], nil
		}
		if fn, ok := t["fn"].(string); ok {
			argv, _ := t["argv"].([]any)
			return ruleFn(fn, argv, scope)
		}
	}
	return nil, fmt.Errorf("an expression %v is neither a literal, a reference nor a function", v)
}

// template expands {name} and {name#path} in s; {{ and }} are literal
// braces. Every name it expands must be bound to a string.
func template(s string, scope map[string]any) (string, error) {
	if !strings.ContainsAny(s, "{}") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '{' && i+1 < len(s) && s[i+1] == '{':
			b.WriteByte('{')
			i++
		case c == '}' && i+1 < len(s) && s[i+1] == '}':
			b.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("the template %q has an unclosed {", s)
			}
			name, path, hasPath := strings.Cut(s[i+1:i+end], "#")
			v := scope[name]
			if hasPath {
				v = getAttr(v, path)
			}
			str, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("the template %q names %s, which is not a string", s, s[i+1:i+end])
			}
			b.WriteString(str)
			i += end
		case c == '}':
			return "", fmt.Errorf("the template %q has an unopened }", s)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// getAttr follows path, such as dnsSuffix or resourceId[1], into v; any
// step that is not there gives nil.
func getAttr(v any, path string) any {
	for part := range strings.SplitSeq(path, ".") {
		name, index, hasIndex := strings.Cut(part, "[")
		if name != "" {
			obj, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = obj[name]
		}
		if hasIndex {
			i, err := strconv.Atoi(strings.TrimSuffix(index, "]"))
			list, ok := v.([]any)
			if err != nil || !ok || i < 0 || i >= len(list) {
				return nil
			}
			v = list[i]
		}
	}
	return v
}
