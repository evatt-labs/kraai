package direct

import "testing"

// join_main bound a type's primary identifier, an ARN, straight to an input
// member that takes only the name or id, a draft every direct read would
// reject. A type is now read by substituting the name or id out of the ARN
// through an {ArnProperty:arnName} input placeholder; the ARN itself
// remains the reader's identifier, bound through the placeholder rather
// than an input member of its own.
func TestReadersSubstituteARNDrafts(t *testing.T) {
	for name, c := range map[string]struct {
		typeName, arnProperty, member, arn, want string
	}{
		"a rule on the default bus": {
			"AWS::Events::Rule", "Arn", "Name",
			"arn:aws:events:us-east-1:123456789012:rule/my-rule",
			"my-rule",
		},
		"a rule on a custom bus": {
			"AWS::Events::Rule", "Arn", "Name",
			"arn:aws:events:us-east-1:123456789012:rule/my-bus/my-rule",
			"my-rule",
		},
		"a rule's custom bus": {
			"AWS::Events::Rule", "Arn", "EventBusName",
			"arn:aws:events:us-east-1:123456789012:rule/my-bus/my-rule",
			"my-bus",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, ok := readers[c.typeName]
			if !ok {
				t.Fatalf("no reader for %s", c.typeName)
			}

			want := "{" + c.arnProperty + ":" + map[string]string{"Name": "arnName", "EventBusName": "arnParent"}[c.member] + "}"
			var got string
			var found bool
			for _, b := range r.Input {
				if b.Member == c.member {
					got, found = b.Value, true
				}
			}
			if !found {
				t.Fatalf("Input has no binding for %s; Input = %#v", c.member, r.Input)
			}
			if got != want {
				t.Fatalf("Input[%s].Value = %q, want %q", c.member, got, want)
			}

			if s := substitute(got, map[string]string{c.arnProperty: c.arn}); s != c.want {
				t.Fatalf("substitute(%q, %s=%q) = %q, want %q", got, c.arnProperty, c.arn, s, c.want)
			}

			var placeholder bool
			for _, b := range r.Identifier {
				if b.Property == c.arnProperty && b.Location == "placeholder" {
					placeholder = true
				}
			}
			if !placeholder {
				t.Fatalf("Identifier has no placeholder binding for %s; Identifier = %#v", c.arnProperty, r.Identifier)
			}
		})
	}
}
