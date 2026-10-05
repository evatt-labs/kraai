package direct

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// accountPlaceholder names the account id in a property no read returns,
// such as an ARN built from the identifier.
const accountPlaceholder = "account"

// templateVars is identifier, with the region and, when one of r's fields
// is built from it, the account id.
func (c *Client) templateVars(ctx context.Context, r Reader, identifier map[string]string) (map[string]string, error) {
	needs := slices.ContainsFunc(r.Fields, func(f Field) bool {
		return f.Kind == "template" && strings.Contains(f.Member, "{"+accountPlaceholder+"}")
	})
	vars := maps.Clone(identifier)
	if vars == nil {
		vars = map[string]string{}
	}
	vars[regionPlaceholder] = c.Region
	if needs && c.Account != nil {
		account, err := c.Account(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading the account id the %s read builds a property from: %w", r.Type, err)
		}
		vars[accountPlaceholder] = account
	}
	return vars, nil
}

// selections matches a member path's list selections, whose placeholders
// filter elements rather than build a value.
var selections = regexp.MustCompile(`\[[^\]]*\]`)
