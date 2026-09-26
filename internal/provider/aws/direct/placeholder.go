package direct

import (
	"regexp"
	"strings"
)

// placeholderName matches a {Property} placeholder, or {Property:filter}
// for a part of its value; see placeholderFilters.
var placeholderName = regexp.MustCompile(`\{([A-Za-z0-9]+)(?::([A-Za-z]+))?\}`)

// placeholderFilters reads a part of an ARN placeholder's value: arnName is
// the last segment of the resource, and arnParent the one before it, empty
// when the resource has no parent, such as a rule on the default bus.
var placeholderFilters = map[string]func(string) string{
	"arnName": func(arn string) string {
		segments := arnSegments(arn)
		return segments[len(segments)-1]
	},
	"arnParent": func(arn string) string {
		if segments := arnSegments(arn); len(segments) >= 3 {
			return segments[len(segments)-2]
		}
		return ""
	},
}

// arnSegments splits an ARN's resource on slashes; a value that is not an
// ARN is one segment.
func arnSegments(arn string) []string {
	if parts := strings.SplitN(arn, ":", 6); len(parts) == 6 {
		return strings.Split(parts[5], "/")
	}
	return []string{arn}
}

// placeholders lists the {Property} names in every string of value.
func placeholders(value any) []string {
	var out []string
	switch v := value.(type) {
	case string:
		for _, m := range placeholderName.FindAllStringSubmatch(v, -1) {
			out = append(out, m[1])
		}
	case []any:
		for _, item := range v {
			out = append(out, placeholders(item)...)
		}
	case map[string]any:
		for _, item := range v {
			out = append(out, placeholders(item)...)
		}
	}
	return out
}
