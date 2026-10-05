package direct

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// endpointRegion is the region an operation's endpoint is resolved for at
// compile time. It matches the aws partition's region pattern and names no
// real region, so wherever it appears in the result is where the client's
// region goes. TestEndpointsHoldInEveryRegion checks that what this gives
// is what every real region of the partition gives.
const endpointRegion = "us-kraai-1"

// endpointOf resolves the endpoint a service's rule set gives an operation
// in the aws partition: the operation's static context parameters set,
// every client setting such as FIPS or dual-stack at its default, and every
// parameter the operation binds from its input left unset. It returns the
// host, {region} standing for the client's region, and the region a global
// endpoint is signed for; or why no endpoint this client can call is there.
func endpointOf(ruleSet json.RawMessage, static map[string]any, signingName string) (host, signingRegion, reason string) {
	if len(ruleSet) == 0 {
		return "", "", "the model has no endpoint rule set"
	}
	rs, err := parseRuleSet(ruleSet)
	if err != nil {
		return "", "", err.Error()
	}
	params := map[string]any{}
	for name, p := range rs.Parameters {
		if p.BuiltIn == "AWS::Region" {
			params[name] = endpointRegion
		}
	}
	for name, v := range static {
		params[name] = v
	}
	e, err := rs.resolve(params)
	if err != nil {
		return "", "", err.Error()
	}
	u, err := url.Parse(e.URL)
	switch {
	case err != nil:
		return "", "", fmt.Sprintf("the rule set gives %s, which is not a URL", e.URL)
	case u.Scheme != "https" || u.Port() != "" || u.User != nil || u.RawQuery != "" || strings.Trim(u.Path, "/") != "":
		return "", "", fmt.Sprintf("the rule set gives %s, not an https host", e.URL)
	case len(e.Headers) > 0:
		return "", "", fmt.Sprintf("the rule set gives %s with headers this client does not send", e.URL)
	}
	host = strings.ReplaceAll(u.Host, endpointRegion, "{region}")
	signing, name, reason := authScheme(e.Properties)
	if reason != "" {
		return "", "", reason
	}
	if name != "" && name != signingName {
		return "", "", fmt.Sprintf("the rule set signs %s for %s, not %s", e.URL, name, signingName)
	}
	switch signing {
	case "", endpointRegion:
	case "us-east-1":
		signingRegion = signing
	default:
		return "", "", fmt.Sprintf("the endpoint %s is signed for %q, not the client's region or us-east-1", host, signing)
	}
	if signingRegion == "" && !strings.Contains(host, "{region}") {
		return "", "", fmt.Sprintf("the endpoint %s names no region and is signed for the client's", host)
	}
	return host, signingRegion, ""
}

// authScheme reads the region and name an endpoint's first auth scheme
// signs for; either is empty when the scheme leaves it to the client. A
// first scheme other than sigv4, such as sigv4a, is refused.
func authScheme(properties map[string]any) (region, name, reason string) {
	schemes, _ := properties["authSchemes"].([]any)
	if len(schemes) == 0 {
		return "", "", ""
	}
	scheme, _ := schemes[0].(map[string]any)
	if scheme["name"] != "sigv4" {
		return "", "", fmt.Sprintf("the endpoint's first auth scheme is %v, not sigv4", scheme["name"])
	}
	region, _ = scheme["signingRegion"].(string)
	name, _ = scheme["signingName"].(string)
	return region, name, ""
}
