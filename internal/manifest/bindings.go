package manifest

import (
	"sort"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// bindingKeyAliases maps a manifest key to the capability it names, for the
// keys where the two differ.
//
// Only `databases:` does. Every other binding key — keyvalue, objects,
// queues, network — is spelled exactly like its capability, so the map has
// one entry rather than a full translation table, and a capability a
// provider adds needs no entry here at all.
//
// A compatibility shim; `databases:` can be retired the next time binding
// keys change incompatibly.
var bindingKeyAliases = map[string]string{"databases": CapabilityDatabase}

// normalizeBindingKeys rewrites each service's binding keys to the
// capability each names, so everything downstream — validation, planning —
// sees capability names only and no second spelling.
//
// Rejects a service declaring both spellings of one capability rather than
// silently merging or dropping one: two keys meaning the same thing is
// always a mistake, and which one won would depend on nothing the author
// could see.
func normalizeBindingKeys(services map[string]Service) error {
	for _, name := range sortedServiceNames(services) {
		svc := services[name]
		for written, capability := range bindingKeyAliases {
			entries, ok := svc.Bindings[written]
			if !ok {
				continue
			}
			if _, clash := svc.Bindings[capability]; clash {
				return kerrors.Validation(
					"services.%s: %q and %q both name the %s capability; use one",
					name, written, capability, capability)
			}
			delete(svc.Bindings, written)
			svc.Bindings[capability] = entries
		}
	}
	return nil
}

// validateServices checks every field DecodeStrict cannot: not an unknown
// key (that is strict-decoding's job, except for the binding keys it is
// handed an inline map for), but a known field whose value falls outside its
// declared vocabulary — Compute.Trigger, the binding keys and their entries,
// and DependsOn's own structural sanity (every named service exists, and a
// service does not name itself).
//
// Iterates services, and each service's binding keys, in sorted order so a
// manifest with more than one problem reports the same one first on every
// run, rather than whichever Go's map iteration happened to visit first.
func (l *Loader) validateServices(root *Root, services map[string]Service) error {
	known := make(map[string]bool, len(l.vocabulary.Names()))
	for _, name := range l.vocabulary.Names() {
		known[name] = true
	}

	for _, name := range sortedServiceNames(services) {
		svc := services[name]
		if svc.Compute != nil {
			switch svc.Compute.Trigger {
			case TriggerHTTP, TriggerSchedule:
			default:
				return kerrors.Validation("services.%s.compute.trigger: must be %q or %q, got %q",
					name, TriggerHTTP, TriggerSchedule, svc.Compute.Trigger)
			}
		}
		references, err := l.validateBindings(root, name, svc, known)
		if err != nil {
			return err
		}
		svc.References = references
		services[name] = svc
		for _, dep := range svc.DependsOn {
			if dep == name {
				return kerrors.Validation("services.%s.depends_on: a service cannot depend on itself", name)
			}
			if _, ok := services[dep]; !ok {
				return kerrors.Validation(
					"services.%s.depends_on: %q is not a declared service", name, dep)
			}
		}
	}
	return nil
}

// validateBindings checks every binding entry of svc against the schema of
// the vendor configured for its capability, and returns the references each
// entry makes to its siblings, keyed by binding name and then by entry key:
// what Service.References carries, resolved here because this is the one
// place with both the entries and the vocabulary saying which keys are
// references. A capability with no vendor configured is left alone; the
// planner reports that once, with the binding it failed to expand.
func (l *Loader) validateBindings(root *Root, name string, svc Service, known map[string]bool) (map[string]map[string]string, error) {
	bindings := svc.Bindings
	capabilities := make([]string, 0, len(bindings))
	for capability := range bindings {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)

	var references map[string]map[string]string
	for _, capability := range capabilities {
		if !known[capability] {
			return nil, kerrors.Validation(
				"services.%s.%s: no registered provider declares capability %q — declared: %s",
				name, capability, capability, strings.Join(l.vocabulary.Names(), ", "))
		}

		configured, hasVendor := root.Providers.For(capability)
		for i, entry := range bindings[capability] {
			// The one key this package requires of every entry, checked
			// here rather than left to the vendor's schema: internal/plan
			// derives a resource's name from it, so an entry without one
			// cannot be planned no matter what vendor fulfils it, and a
			// capability whose vendor declares no Binding schema would
			// otherwise reach the planner unnamed.
			if entry.Name() == "" {
				return nil, kerrors.Validation(
					"services.%s.%s[%d]: %s is required and must be a non-empty string",
					name, capability, i, BindingKey)
			}
			if !hasVendor {
				continue
			}
			if err := l.vocabulary.ValidateBinding(capability, configured.Vendor, entry); err != nil {
				return nil, kerrors.Wrap(err, kerrors.CodeValidation,
					"services.%s.%s[%d]", name, capability, i)
			}
			for _, key := range l.vocabulary.References(capability, configured.Vendor) {
				raw, present := entry[key]
				if !present {
					continue
				}
				target, ok := raw.(string)
				if !ok || target == "" {
					return nil, kerrors.Validation(
						"services.%s.%s[%d].%s: must name a binding on service %q, got %v",
						name, capability, i, key, name, raw)
				}
				if target == entry.Name() {
					return nil, kerrors.Validation(
						"services.%s.%s[%d].%s: %q names this binding itself",
						name, capability, i, key, target)
				}
				if !hasAnyBinding(svc, target) {
					return nil, kerrors.Validation(
						"services.%s.%s[%d].%s: %q is not a binding declared on service %q",
						name, capability, i, key, target, name)
				}
				if references == nil {
					references = map[string]map[string]string{}
				}
				if references[entry.Name()] == nil {
					references[entry.Name()] = map[string]string{}
				}
				references[entry.Name()][key] = target
			}
		}
	}
	return references, nil
}

// hasAnyBinding reports whether svc declares a binding named name under
// any capability.
func hasAnyBinding(svc Service, name string) bool {
	for capability := range svc.Bindings {
		if hasBinding(svc, capability, name) {
			return true
		}
	}
	return false
}

// hasBinding reports whether svc declares a binding named name under
// capability.
func hasBinding(svc Service, capability, name string) bool {
	for _, entry := range svc.Bindings[capability] {
		if entry.Name() == name {
			return true
		}
	}
	return false
}

// sortedServiceNames returns services' keys in ascending order.
func sortedServiceNames(services map[string]Service) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
