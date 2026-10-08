package aws

import "testing"

// Forms a service returns a value in other than the one it was given
// compare as the same value; anything else still differs.
func TestCoversNormalizedForms(t *testing.T) {
	none := listRules{}
	lambdaURL := listRules{equivalent: equivalentForms["AWS::Lambda::Url"]}
	permission := listRules{equivalent: equivalentForms[realTypeLambdaPermission]}
	policy := map[string]any{"Statement": []any{map[string]any{"Effect": "Allow", "Action": []any{"logs:PutLogEvents"}}}}
	for name, c := range map[string]struct {
		desired, current any
		pointer          string
		rules            listRules
		want             bool
	}{
		"an object read back as JSON text":      {policy, `{"Statement":[{"Effect":"Allow","Action":"logs:PutLogEvents"}]}`, "/properties/PolicyDocument", none, true},
		"JSON text read back as an object":      {`{"a": 1}`, map[string]any{"a": float64(1)}, "/properties/P", none, true},
		"JSON text that differs":                {policy, `{"Statement":[{"Effect":"Deny","Action":"logs:PutLogEvents"}]}`, "/properties/PolicyDocument", none, false},
		"a one-element list kept as its item":   {[]any{"x"}, "x", "/properties/Action", none, true},
		"a scalar read back as a list of one":   {"x", []any{"x"}, "/properties/Action", none, true},
		"a one-element list of another item":    {[]any{"x"}, "y", "/properties/Action", none, false},
		"two elements against one":              {[]any{"x", "y"}, "x", "/properties/Action", none, false},
		"text that is not JSON":                 {map[string]any{"a": 1.0}, "{not json", "/properties/P", none, false},
		"a function ARN read back as its name":  {"arn:aws:lambda:us-east-1:123456789012:function:web", "web", "/properties/TargetFunctionArn", lambdaURL, true},
		"a qualified ARN read back as its name": {"arn:aws:lambda:us-east-1:123456789012:function:web:live", "web", "/properties/TargetFunctionArn", lambdaURL, true},
		"an ARN of another function":            {"arn:aws:lambda:us-east-1:123456789012:function:web", "api", "/properties/TargetFunctionArn", lambdaURL, false},
		"an account read back as its root":      {"123456789012", "arn:aws:iam::123456789012:root", "/properties/Principal", permission, true},
		"another account's root":                {"123456789012", "arn:aws:iam::210987654321:root", "/properties/Principal", permission, false},
		"a rule only for its own property":      {"123456789012", "arn:aws:iam::123456789012:root", "/properties/SourceAccount", permission, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := covers(c.desired, c.current, c.pointer, c.rules); got != c.want {
				t.Fatalf("covers(%v, %v) = %v, want %v", c.desired, c.current, got, c.want)
			}
		})
	}
}
