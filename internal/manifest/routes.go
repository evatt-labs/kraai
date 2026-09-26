package manifest

import (
	"fmt"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// validateRoutes checks every `routes:` entry against the services it names.
//
// Here rather than in validateEnvironment because it needs the service map:
// a route belongs to a service, and a custom domain's certificate is one of
// that service's own `tls:` bindings. Sorted iteration, as everywhere in the
// file, so a manifest with several problems reports the same one first.
func validateRoutes(envName string, services map[string]Service, env *Environment) error {
	path := environmentsDir + "/" + envName + ".yaml"
	for _, svc := range sortedKeysOf(env.Routes) {
		service, ok := services[svc]
		if !ok {
			return kerrors.Validation(
				"%s: routes.%s: %q is not a declared service", path, svc, svc)
		}
		for i, route := range env.Routes[svc] {
			where := fmt.Sprintf("%s: routes.%s[%d]", path, svc, i)
			if route.Pattern == "" {
				return kerrors.Validation("%s: pattern is required", where)
			}
			switch {
			case route.CustomDomain && route.Certificate == "":
				// The case a manifest could previously write and be silently
				// wrong about: a custom domain is a resource kraai builds,
				// and it cannot be built without the certificate it presents.
				return kerrors.Validation(
					"%s: custom_domain requires certificate, naming a tls binding on service %q "+
						"that carries the certificate for %q", where, svc, route.Pattern)
			case !route.CustomDomain && route.Certificate != "":
				return kerrors.Validation(
					"%s: certificate %q is set but custom_domain is not — a certificate is only "+
						"presented on a custom domain", where, route.Certificate)
			case route.CustomDomain:
				if !hasBinding(service, CapabilityTLS, route.Certificate) {
					return kerrors.Validation(
						"%s: certificate %q is not a tls binding declared on service %q",
						where, route.Certificate, svc)
				}
			}
		}
	}
	return nil
}
