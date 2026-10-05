package direct

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"

	"github.com/aws/smithy-go/endpoints/private/rulesfn"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// partitionsJSON is aws-sdk-go-v2 v1.47.0's
// internal/endpoints/awsrulesfn/partitions.json, which aws.partition reads.
// That package is internal to the SDK, so the data is copied, not imported.
//
//go:embed partitions.json
var partitionsJSON []byte

type partition struct {
	ID          string                    `json:"id"`
	RegionRegex string                    `json:"regionRegex"`
	Regions     map[string]map[string]any `json:"regions"`
	Outputs     map[string]any            `json:"outputs"`
	regex       *regexp.Regexp
}

var loadPartitions = sync.OnceValues(func() ([]partition, error) {
	var doc struct{ Partitions []partition }
	if err := json.Unmarshal(partitionsJSON, &doc); err != nil {
		return nil, fmt.Errorf("partitions.json: %w", err)
	}
	for i := range doc.Partitions {
		re, err := regexp.Compile(doc.Partitions[i].RegionRegex)
		if err != nil {
			return nil, fmt.Errorf("partitions.json: partition %s: %w", doc.Partitions[i].ID, err)
		}
		doc.Partitions[i].regex = re
	}
	return doc.Partitions, nil
})

// awsPartition is aws.partition: the partition a region is listed in, with
// that region's own overrides, else the first whose pattern it matches,
// else aws.
func awsPartition(region string) (map[string]any, error) {
	parts, err := loadPartitions()
	if err != nil {
		return nil, err
	}
	pick := func(p partition, overrides map[string]any) map[string]any {
		out := make(map[string]any, len(p.Outputs))
		for k, v := range p.Outputs {
			out[k] = v
		}
		for k, v := range overrides {
			if k != "description" {
				out[k] = v
			}
		}
		return out
	}
	for _, p := range parts {
		if o, ok := p.Regions[region]; ok {
			return pick(p, o), nil
		}
	}
	for _, p := range parts {
		if p.regex.MatchString(region) {
			return pick(p, nil), nil
		}
	}
	for _, p := range parts {
		if p.ID == "aws" {
			return pick(p, nil), nil
		}
	}
	return nil, nil
}

// ruleFn calls one of the rules language's functions on argv, evaluated in
// scope. A function this package does not know is an error, never a
// condition that silently fails.
func ruleFn(fn string, argv []any, scope map[string]any) (any, error) {
	args := make([]any, len(argv))
	for i, a := range argv {
		v, err := evalExpr(a, scope)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	arity := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("%s takes %d arguments, not %d", fn, n, len(args))
		}
		return nil
	}
	switch fn {
	case "isSet":
		if err := arity(1); err != nil {
			return nil, err
		}
		return args[0] != nil, nil
	case "not":
		if err := arity(1); err != nil {
			return nil, err
		}
		b, ok := args[0].(bool)
		if !ok {
			return nil, fmt.Errorf("not takes a boolean, not %T", args[0])
		}
		return !b, nil
	case "booleanEquals", "stringEquals":
		if err := arity(2); err != nil {
			return nil, err
		}
		return args[0] != nil && args[0] == args[1], nil
	case "getAttr":
		if err := arity(2); err != nil {
			return nil, err
		}
		path, ok := args[1].(string)
		if !ok {
			return nil, fmt.Errorf("getAttr takes a string path")
		}
		return getAttr(args[0], path), nil
	case "coalesce":
		for _, a := range args {
			if a != nil {
				return a, nil
			}
		}
		return nil, nil
	case "ite":
		if err := arity(3); err != nil {
			return nil, err
		}
		b, ok := args[0].(bool)
		if !ok {
			return nil, fmt.Errorf("ite takes a boolean condition, not %T", args[0])
		}
		if b {
			return args[1], nil
		}
		return args[2], nil
	}
	// The rest take a string first; an unset one gives an unset result.
	if len(args) == 0 {
		return nil, fmt.Errorf("%s takes arguments", fn)
	}
	s, isString := args[0].(string)
	if args[0] == nil {
		return nil, nil
	}
	if !isString {
		return nil, fmt.Errorf("%s takes a string, not %T", fn, args[0])
	}
	switch fn {
	case "substring":
		if err := arity(4); err != nil {
			return nil, err
		}
		start, ok1 := args[1].(float64)
		stop, ok2 := args[2].(float64)
		reverse, ok3 := args[3].(bool)
		if !ok1 || !ok2 || !ok3 {
			return nil, fmt.Errorf("substring takes a string, two integers and a boolean")
		}
		if v := rulesfn.SubString(s, int(start), int(stop), reverse); v != nil {
			return *v, nil
		}
		return nil, nil
	case "isValidHostLabel", "aws.isVirtualHostableS3Bucket":
		if err := arity(2); err != nil {
			return nil, err
		}
		sub, ok := args[1].(bool)
		if !ok {
			return nil, fmt.Errorf("%s takes a boolean second", fn)
		}
		if fn == "isValidHostLabel" {
			return rulesfn.IsValidHostLabel(s, sub), nil
		}
		return virtualHostableS3Bucket(s, sub), nil
	case "parseURL":
		if err := arity(1); err != nil {
			return nil, err
		}
		u := rulesfn.ParseURL(s)
		if u == nil {
			return nil, nil
		}
		return map[string]any{"scheme": u.Scheme, "authority": u.Authority, "path": u.Path, "normalizedPath": u.NormalizedPath, "isIp": u.IsIp}, nil
	case "uriEncode":
		if err := arity(1); err != nil {
			return nil, err
		}
		return rulesfn.URIEncode(s), nil
	case "split":
		if err := arity(3); err != nil {
			return nil, err
		}
		delim, ok1 := args[1].(string)
		limit, ok2 := args[2].(float64)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("split takes a string, a delimiter and a limit")
		}
		parts := rulesfn.Split(s, delim, int(limit))
		out := make([]any, len(parts))
		for i, p := range parts {
			out[i] = p
		}
		return out, nil
	case "aws.partition":
		if err := arity(1); err != nil {
			return nil, err
		}
		p, err := awsPartition(s)
		if p == nil || err != nil {
			return nil, err
		}
		return p, nil
	case "aws.parseArn":
		if err := arity(1); err != nil {
			return nil, err
		}
		return parseArn(s), nil
	}
	return nil, fmt.Errorf("the endpoint rule set calls %s, which this client does not evaluate", fn)
}

// parseArn is aws.parseArn: nil unless s is arn:partition:service:region:
// account:resource with partition, service and resource non-empty; the
// resource is split on / and :.
func parseArn(s string) any {
	rest, ok := strings.CutPrefix(s, "arn:")
	if !ok {
		return nil
	}
	sections := strings.SplitN(rest, ":", 5)
	if len(sections) != 5 || sections[0] == "" || sections[1] == "" || sections[4] == "" {
		return nil
	}
	var resource []any
	for v := sections[4]; ; {
		i := strings.IndexAny(v, "/:")
		if i < 0 {
			resource = append(resource, v)
			break
		}
		resource = append(resource, v[:i])
		v = v[i+1:]
	}
	return map[string]any{"partition": sections[0], "service": sections[1], "region": sections[2], "accountId": sections[3], "resourceId": resource}
}

// virtualHostableS3Bucket is aws.isVirtualHostableS3Bucket: every label
// 3 to 63 characters, lower case, a valid host label, and the whole not an
// IP address.
func virtualHostableS3Bucket(s string, allowSubDomains bool) bool {
	if net.ParseIP(s) != nil {
		return false
	}
	labels := []string{s}
	if allowSubDomains {
		labels = strings.Split(s, ".")
	}
	for _, label := range labels {
		if l := len(label); l < 3 || l > 63 || strings.ToLower(label) != label || !smithyhttp.ValidHostLabel(label) {
			return false
		}
	}
	return true
}
