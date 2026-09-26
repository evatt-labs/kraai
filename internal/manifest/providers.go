package manifest

import (
	"sort"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// validateRootShape checks everything about kraai.yaml that does not depend
// on knowing which capabilities exist — its version, and its `plugins:`
// list. Split out so LoadRoot can run it during bootstrap, before there is a
// Vocabulary to check the rest against.
func (l *Loader) validateRootShape(root *Root) error {
	if root.Version != 1 {
		return kerrors.Validation("%s: version: must be 1, got %d", rootFile, root.Version)
	}
	return validatePlugins(root.Plugins)
}

// validateRoot checks kraai.yaml's version and every key under `providers:`.
//
// Iterates the map directly, in sorted key order, rather than through
// Providers.Capabilities: a key written with no value under it is
// unconfigured to every other caller, but it is still a key this file
// declared, and an unknown one has to be reported whether or not the author
// got as far as naming a vendor. Sorted so a manifest with more than one bad
// key reports the same one first on every run.
func (l *Loader) validateRoot(root *Root) error {
	if err := l.validateRootShape(root); err != nil {
		return err
	}

	declared := l.vocabulary.Names()
	known := make(map[string]bool, len(declared))
	for _, name := range declared {
		known[name] = true
	}

	written := make([]string, 0, len(root.Providers))
	for capability := range root.Providers {
		written = append(written, capability)
	}
	sort.Strings(written)

	for _, capability := range written {
		// Every failure here is caught at load, naming the file and the key,
		// rather than at plan time as a registry lookup with nothing pointing
		// back at the line that caused it.
		if !known[capability] {
			return kerrors.Validation(
				"%s: providers.%s: no registered provider declares capability %q — declared: %s",
				rootFile, capability, capability, strings.Join(declared, ", "))
		}
		provider, ok := root.Providers.For(capability)
		if !ok {
			continue
		}
		if provider.Vendor == "" {
			return kerrors.Validation(
				"%s: providers.%s: vendor is required", rootFile, capability)
		}
		// Both halves of the pairing: a real vendor that does not fulfil this
		// capability is still unresolvable.
		vendors := l.vocabulary.VendorsFor(capability)
		if !contains(vendors, provider.Vendor) {
			return kerrors.Validation(
				"%s: providers.%s.vendor: %q does not provide capability %q — %s",
				rootFile, capability, provider.Vendor, capability, vendorsSuffix(vendors))
		}
		if err := l.vocabulary.ValidateSettings(capability, provider.Vendor, provider.Settings); err != nil {
			return kerrors.Wrap(err, kerrors.CodeValidation, "%s: providers.%s.settings", rootFile, capability)
		}
	}
	return nil
}

// vendorsSuffix names who could have gone there, or says plainly that nobody
// could — an empty list means the capability is declared by some provider for
// something other than this, and "providers for it: " followed by nothing
// reads as a truncated message rather than an answer.
func vendorsSuffix(vendors []string) string {
	if len(vendors) == 0 {
		return "no provider declares it"
	}
	return "providers for it: " + strings.Join(vendors, ", ")
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
