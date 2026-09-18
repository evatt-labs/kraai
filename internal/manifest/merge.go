package manifest

import "github.com/evatt-labs/kraai/internal/kerrors"

// mergeServiceFiles merges files (one per services/*.yaml or
// services/*.yaml.j2, already decoded), in order, into one service set.
// sources names each file for error messages and must be the same length
// as files.
//
// A service key defined in two different files is a validation error, not
// last-file-wins: a directory of small files instead of one monolith
// exists for PR reviewability, and a silent overwrite across files is
// exactly the kind of thing a reviewer could miss.
func mergeServiceFiles(files []ServicesFile, sources []string) (map[string]Service, error) {
	merged := make(map[string]Service)
	definedIn := make(map[string]string, len(merged))

	for i, file := range files {
		for name, svc := range file.Services {
			if prevSource, ok := definedIn[name]; ok {
				return nil, kerrors.Validation(
					"service %q is defined in both %s and %s", name, prevSource, sources[i])
			}
			merged[name] = svc
			definedIn[name] = sources[i]
		}
	}
	return merged, nil
}
