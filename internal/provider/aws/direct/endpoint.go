package direct

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/smithy-go/endpoints/private/rulesfn"
)

// endpointRegion is the region an operation's endpoint is resolved for at
// compile time. It matches the aws partition's region pattern and names no
// real region, so wherever it appears in the result is where the client's
// region goes. TestEndpointsHoldInEveryRegion checks that what this gives
// is what every real region of the partition gives.
const endpointRegion = "us-kraai-1"

// endpointBound is the value a parameter bound from an input member takes
// at compile time: a general-purpose S3 bucket name, so a rule set that
// branches on the name, as S3's does on ARNs and directory buckets, takes
// the branch an ordinary name does.
const endpointBound = "kraai-endpoint-bound"

// endpointParams is the parameters an operation is resolved with: its
// static context parameters and the override's EndpointParams, a
// "{Member}" value bound to endpointBound. member is the input member a
// parameter is bound to, if one is.
func endpointParams(declared, static map[string]any) (params map[string]any, member string) {
	params = map[string]any{}
	for name, v := range static {
		params[name] = v
	}
	for name, v := range declared {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			member = s[1 : len(s)-1]
			v = endpointBound
		}
		params[name] = v
	}
	return params, member
}

// endpoint is an operation's resolved endpoint.
type endpoint struct {
	// Host is the host, {region} standing for the client's region.
	Host string
	// SigningRegion is the region a global endpoint is signed for.
	SigningRegion string
	// DisableDoubleEncoding is the auth scheme's disableDoubleEncoding.
	DisableDoubleEncoding bool
}

// endpointOf resolves the endpoint a service's rule set gives an operation
// in the aws partition: params set, every client setting such as FIPS or
// dual-stack at its default, and every other parameter the operation
// binds from its input left unset. bound is the value a parameter bound
// from an input member was given, which the URL must carry as its whole
// path, as S3's path-style URL carries the bucket; any other path is
// refused. It returns the endpoint, or why none this client can call is
// there.
func endpointOf(ruleSet json.RawMessage, params map[string]any, signingName, bound string) (endpoint, string) {
	if len(ruleSet) == 0 {
		return endpoint{}, "the model has no endpoint rule set"
	}
	rs, err := parseRuleSet(ruleSet)
	if err != nil {
		return endpoint{}, err.Error()
	}
	return endpointFor(rs, params, signingName, bound)
}

// endpointFor is endpointOf for a rule set already parsed.
func endpointFor(rs *ruleSet, params map[string]any, signingName, bound string) (endpoint, string) {
	all := map[string]any{}
	for name, p := range rs.Parameters {
		if p.BuiltIn == "AWS::Region" {
			all[name] = endpointRegion
		}
	}
	for name, v := range params {
		all[name] = v
	}
	e, err := rs.resolve(all)
	if err != nil {
		return endpoint{}, err.Error()
	}
	u, err := url.Parse(e.URL)
	path := ""
	if bound != "" {
		path = "/" + rulesfn.URIEncode(bound)
	}
	switch {
	case err != nil:
		return endpoint{}, fmt.Sprintf("the rule set gives %s, which is not a URL", e.URL)
	case u.Scheme != "https" || u.Port() != "" || u.User != nil || u.RawQuery != "":
		return endpoint{}, fmt.Sprintf("the rule set gives %s, not an https host", e.URL)
	case strings.TrimSuffix(u.EscapedPath(), "/") != path:
		if bound != "" {
			return endpoint{}, fmt.Sprintf("the rule set gives %s, whose path is not the bound parameter alone", e.URL)
		}
		return endpoint{}, fmt.Sprintf("the rule set gives %s, not an https host", e.URL)
	case len(e.Headers) > 0:
		return endpoint{}, fmt.Sprintf("the rule set gives %s with headers this client does not send", e.URL)
	}
	out := endpoint{Host: strings.ReplaceAll(u.Host, endpointRegion, "{region}")}
	scheme, reason := authScheme(e.Properties)
	if reason != "" {
		return endpoint{}, reason
	}
	if scheme.name != "" && scheme.name != signingName {
		return endpoint{}, fmt.Sprintf("the rule set signs %s for %s, not %s", e.URL, scheme.name, signingName)
	}
	out.DisableDoubleEncoding = scheme.disableDoubleEncoding
	switch scheme.region {
	case "", endpointRegion:
	case "us-east-1":
		out.SigningRegion = scheme.region
	default:
		return endpoint{}, fmt.Sprintf("the endpoint %s is signed for %q, not the client's region or us-east-1", out.Host, scheme.region)
	}
	if out.SigningRegion == "" && !strings.Contains(out.Host, "{region}") {
		return endpoint{}, fmt.Sprintf("the endpoint %s names no region and is signed for the client's", out.Host)
	}
	return out, ""
}

// sigv4Scheme is what an endpoint's first auth scheme says about signing:
// the region and name it signs for, either empty when it leaves it to the
// client, and whether the path is signed as sent.
type sigv4Scheme struct {
	region, name          string
	disableDoubleEncoding bool
}

// authScheme reads an endpoint's first auth scheme. A first scheme other
// than sigv4, such as sigv4a, is refused.
func authScheme(properties map[string]any) (sigv4Scheme, string) {
	schemes, _ := properties["authSchemes"].([]any)
	if len(schemes) == 0 {
		return sigv4Scheme{}, ""
	}
	scheme, _ := schemes[0].(map[string]any)
	if scheme["name"] != "sigv4" {
		return sigv4Scheme{}, fmt.Sprintf("the endpoint's first auth scheme is %v, not sigv4", scheme["name"])
	}
	var out sigv4Scheme
	out.region, _ = scheme["signingRegion"].(string)
	out.name, _ = scheme["signingName"].(string)
	out.disableDoubleEncoding, _ = scheme["disableDoubleEncoding"].(bool)
	return out, ""
}
