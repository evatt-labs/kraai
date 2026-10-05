package direct

import (
	"encoding/json"
	"strings"
	"testing"
)

// regionalRules is a rule set giving url, a template over Region and the
// partition, for every region.
func regionalRules(url string) map[string]any {
	return rulesWith(map[string]any{"type": "endpoint", "endpoint": map[string]any{"url": url}})
}

// rulesWith is a rule set binding PartitionResult for Region, then trying
// rules in order.
func rulesWith(rules ...any) map[string]any {
	return map[string]any{
		"parameters": map[string]any{
			"Region":            map[string]any{"builtIn": "AWS::Region", "type": "string"},
			"UseFIPS":           map[string]any{"builtIn": "AWS::UseFIPS", "type": "boolean", "required": true, "default": false},
			"IsSearchOperation": map[string]any{"type": "boolean"},
		},
		"rules": []any{map[string]any{
			"type": "tree",
			"conditions": []any{map[string]any{
				"fn": "aws.partition", "argv": []any{map[string]any{"ref": "Region"}}, "assign": "PartitionResult",
			}},
			"rules": append(rules, map[string]any{"type": "error", "error": "no endpoint"}),
		}},
	}
}

func signedFor(url, name, region string) map[string]any {
	scheme := map[string]any{"name": name, "signingRegion": region}
	return map[string]any{"type": "endpoint", "endpoint": map[string]any{"url": url, "properties": map[string]any{"authSchemes": []any{scheme}}}}
}

func TestEndpointOf(t *testing.T) {
	fips := map[string]any{
		"type":       "endpoint",
		"conditions": []any{map[string]any{"fn": "booleanEquals", "argv": []any{map[string]any{"ref": "UseFIPS"}, true}}},
		"endpoint":   map[string]any{"url": "https://widgets-fips.{Region}.{PartitionResult#dnsSuffix}"},
	}
	search := map[string]any{
		"type":       "endpoint",
		"conditions": []any{map[string]any{"fn": "booleanEquals", "argv": []any{map[string]any{"ref": "IsSearchOperation"}, true}}},
		"endpoint":   map[string]any{"url": "https://search-widgets.{Region}.{PartitionResult#dnsSuffix}"},
	}
	regional := map[string]any{"type": "endpoint", "endpoint": map[string]any{"url": "https://widgets.{Region}.{PartitionResult#dnsSuffix}"}}
	cases := map[string]struct {
		rules                  map[string]any
		static                 map[string]any
		host, signing, refused string
	}{
		"regional":                                     {rules: rulesWith(regional), host: "widgets.{region}.amazonaws.com"},
		"fips is off by default":                       {rules: rulesWith(fips, regional), host: "widgets.{region}.amazonaws.com"},
		"dual-stack only":                              {rules: regionalRules("https://widgets.{Region}.{PartitionResult#dualStackDnsSuffix}"), host: "widgets.{region}.api.aws"},
		"global, signed for us-east-1":                 {rules: rulesWith(signedFor("https://widgets.{PartitionResult#dnsSuffix}", "sigv4", "{PartitionResult#implicitGlobalRegion}")), host: "widgets.amazonaws.com", signing: "us-east-1"},
		"one region's host for every region":           {rules: rulesWith(signedFor("https://widgets.us-east-1.amazonaws.com", "sigv4", "us-east-1")), host: "widgets.us-east-1.amazonaws.com", signing: "us-east-1"},
		"regional, signed for its region":              {rules: rulesWith(signedFor("https://widgets.{Region}.{PartitionResult#dnsSuffix}", "sigv4", "{Region}")), host: "widgets.{region}.amazonaws.com"},
		"another operation":                            {rules: rulesWith(search, regional), host: "widgets.{region}.amazonaws.com"},
		"the search operation":                         {rules: rulesWith(search, regional), static: map[string]any{"IsSearchOperation": true}, host: "search-widgets.{region}.amazonaws.com"},
		"a global host signed elsewhere":               {rules: rulesWith(signedFor("https://widgets.amazonaws.com", "sigv4", "us-west-2")), refused: `is signed for "us-west-2"`},
		"a global host signed for the client's region": {rules: regionalRules("https://widgets.{PartitionResult#dnsSuffix}"), refused: "names no region and is signed for the client's"},
		"sigv4a":                         {rules: rulesWith(signedFor("https://widgets.{Region}.{PartitionResult#dnsSuffix}", "sigv4a", "*")), refused: "first auth scheme is sigv4a"},
		"a signing name of its own":      {rules: rulesWith(map[string]any{"type": "endpoint", "endpoint": map[string]any{"url": "https://widgets.{Region}.{PartitionResult#dnsSuffix}", "properties": map[string]any{"authSchemes": []any{map[string]any{"name": "sigv4", "signingName": "gadgets"}}}}}), refused: "signs https://widgets.us-kraai-1.amazonaws.com for gadgets, not widgets"},
		"a path":                         {rules: regionalRules("https://widgets.{Region}.{PartitionResult#dnsSuffix}/v1"), refused: "not an https host"},
		"plain http":                     {rules: regionalRules("http://widgets.{Region}.{PartitionResult#dnsSuffix}"), refused: "not an https host"},
		"a port":                         {rules: regionalRules("https://widgets.{Region}.{PartitionResult#dnsSuffix}:8443"), refused: "not an https host"},
		"headers":                        {rules: rulesWith(map[string]any{"type": "endpoint", "endpoint": map[string]any{"url": "https://widgets.{Region}.{PartitionResult#dnsSuffix}", "headers": map[string]any{"x-widget": []any{"1"}}}}), refused: "headers this client does not send"},
		"the rule set's own refusal":     {rules: rulesWith(), refused: "no endpoint"},
		"a parameter the rule set lacks": {rules: rulesWith(regional), static: map[string]any{"Bucket": "b"}, refused: "declares no endpoint parameter Bucket"},
		"no rule set":                    {refused: "the model has no endpoint rule set"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var raw json.RawMessage
			if c.rules != nil {
				raw, _ = json.Marshal(c.rules)
			}
			host, signing, reason := endpointOf(raw, c.static, "widgets")
			if c.refused != "" {
				if !strings.Contains(reason, c.refused) || host != "" {
					t.Fatalf("endpointOf = %q, %q, %q; want refused with %q", host, signing, reason, c.refused)
				}
				return
			}
			if host != c.host || signing != c.signing || reason != "" {
				t.Fatalf("endpointOf = %q, %q, %q; want %q, %q", host, signing, reason, c.host, c.signing)
			}
		})
	}
}

// TestEndpointsHoldInEveryRegion resolves every operation each override
// calls in every region of the aws partition, and checks it gets the
// compiled host with that region in it: the compiler resolves for one
// made-up region, which is only sound while no rule set gives a region an
// endpoint of its own.
func TestEndpointsHoldInEveryRegion(t *testing.T) {
	parts, err := loadPartitions()
	if err != nil {
		t.Fatal(err)
	}
	var regions []string
	for _, p := range parts {
		if p.ID == "aws" {
			for r := range p.Regions {
				if r != "aws-global" {
					regions = append(regions, r)
				}
			}
		}
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := loadLock(files)
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]Reader{}
	for _, r := range Readers() {
		byType[r.Type] = r
	}
	var checked int
	for _, o := range all {
		r, ok := byType[o.Type]
		if !ok {
			t.Errorf("%s compiled to no reader", o.Type)
			continue
		}
		m, err := loadModel(files, lock.Models[o.Read.Model].File)
		if err != nil {
			t.Fatal(err)
		}
		var service, namespace string
		for id, s := range m.Shapes {
			if s.Type == "service" {
				service, namespace = id, id[:strings.Index(id, "#")+1]
			}
		}
		rs, err := parseRuleSet(m.Shapes[service].Traits["smithy.rules#endpointRuleSet"])
		if err != nil {
			t.Fatal(err)
		}
		calls := map[string]Reader{o.Read.Operation: r}
		if len(r.Also) != len(o.Also) {
			t.Fatalf("%s compiled %d of its %d further calls", o.Type, len(r.Also), len(o.Also))
		}
		for i, a := range o.Also {
			calls[a.Operation] = r.Also[i]
		}
		var mutations func(*MutationCall)
		mutations = func(c *MutationCall) {
			if c == nil {
				return
			}
			calls[c.Operation] = r
			mutations(c.Change)
		}
		mutations(r.Create)
		for i := range r.Update {
			mutations(&r.Update[i])
		}
		mutations(r.Delete)
		for op, call := range calls {
			var static map[string]struct{ Value any }
			_ = json.Unmarshal(m.Shapes[namespace+op].Traits["smithy.rules#staticContextParams"], &static)
			for _, region := range regions {
				params := map[string]any{"Region": region}
				for name, p := range static {
					params[name] = p.Value
				}
				e, err := rs.resolve(params)
				if err != nil {
					t.Errorf("%s %s in %s: %v", o.Type, op, region, err)
					break
				}
				// Signed for the client's region, either way it is spelled.
				signing, _, _ := authScheme(e.Properties)
				if signing == "" {
					signing = region
				}
				compiled := call.SigningRegion
				if compiled == "" {
					compiled = region
				}
				wantURL := "https://" + strings.ReplaceAll(call.Host, "{region}", region)
				if e.URL != wantURL || signing != compiled {
					t.Errorf("%s %s in %s: the rule set gives %s signed for %s; compiled %s signed for %s",
						o.Type, op, region, e.URL, signing, wantURL, compiled)
					break
				}
				checked++
			}
		}
	}
	t.Logf("%d operation and region pairs across %d overrides and %d regions", checked, len(all), len(regions))
}
