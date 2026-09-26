package manifest

import (
	"regexp"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// PolicySetPattern is what an environment's policy set may be called.
var PolicySetPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func validateEnvironment(path string, env *Environment) error {
	switch env.Kind {
	case EnvironmentKindEphemeral, EnvironmentKindPersistent:
	default:
		return kerrors.Validation("%s: kind: must be %q or %q, got %q",
			path, EnvironmentKindEphemeral, EnvironmentKindPersistent, env.Kind)
	}
	if env.TTL != "" {
		if env.Kind != EnvironmentKindEphemeral {
			return kerrors.Validation("%s: ttl: only an ephemeral environment expires; a %s one has no ttl", path, env.Kind)
		}
		ttl, err := time.ParseDuration(env.TTL)
		if err != nil || ttl <= 0 {
			return kerrors.Validation("%s: ttl: must be a positive duration such as 72h, got %q", path, env.TTL)
		}
	}

	seen := map[string]bool{}
	for _, name := range env.Policies {
		// A set name is joined onto --policy directories, which are read
		// outside the manifest root, so it must be one path segment.
		if !PolicySetPattern.MatchString(name) {
			return kerrors.Validation("%s: policies: %q must match %s", path, name, PolicySetPattern)
		}
		if seen[name] {
			return kerrors.Validation("%s: policies: %q is named twice", path, name)
		}
		seen[name] = true
	}

	// naming.prefix becomes a leading segment of every derived name, so it
	// is validated here like any other field; see validatePrefix.
	if env.Naming != nil {
		if err := validatePrefix(env.Naming.Prefix); err != nil {
			return kerrors.Wrap(err, kerrors.CodeValidation,
				"%s: naming.prefix: %q", path, env.Naming.Prefix)
		}
	}
	return nil
}
