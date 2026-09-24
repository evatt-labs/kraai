package policy

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
)

// Gate is which command a policy judges: the plan an apply would carry
// out, or the teardown a destroy would.
type Gate string

const (
	// GatePlan judges a plan, in package kraai.plan.
	GatePlan Gate = "plan"
	// GateDestroy judges a teardown, in package kraai.destroy.
	GateDestroy Gate = "destroy"
)

// ManifestDir is where a manifest keeps its policies.
const ManifestDir = "policies"

// evalTimeout bounds one evaluation. A policy runs after apply has taken the
// environment lock, and the lock renews for as long as the run lasts, so an
// evaluation that never finished would hold it for good.
var evalTimeout = 30 * time.Second

// forbiddenBuiltins reach outside the evaluation: the network, DNS, and
// opa.runtime, which exposes the process environment and with it every
// credential kraai runs with. A policy may arrive in the pull request it is
// judging.
var forbiddenBuiltins = []string{"http.send", "net.lookup_ip_addr", "opa.runtime"}

// Set is the loaded, compiled policies, or none.
type Set struct {
	// units are compiled apart so that none can define rules in another's
	// namespace: the manifest's policies arrive with the change they judge,
	// and must not be able to extend a helper or a set the --policy ones
	// rely on.
	units []unit
}

type unit struct {
	compiler *ast.Compiler
	// gates are those with a deny rule defined.
	gates map[Gate]bool
}

// Empty reports whether no policy was loaded.
func (s *Set) Empty() bool { return s == nil || len(s.units) == 0 }

// capabilities is this OPA version's, less the forbidden builtins, with no
// network host allowed.
func capabilities() *ast.Capabilities {
	caps := ast.CapabilitiesForThisVersion()
	kept := caps.Builtins[:0]
	for _, b := range caps.Builtins {
		if !slices.Contains(forbiddenBuiltins, b.Name) {
			kept = append(kept, b)
		}
	}
	caps.Builtins = kept
	caps.AllowNet = []string{}
	return caps
}

// Load reads every policy in the manifest's policies directory and at each
// of extra, a file or a directory of .rego files, and compiles the two
// groups separately; a gate denies what either denies. None at all is an
// empty Set. A policy that does not parse or compile, calls a forbidden
// builtin, declares a package kraai does not evaluate, or belongs to a
// group with no deny rule fails the load: a policy that silently never ran
// would read as one that passed.
func Load(fsys manifest.FS, extra []string) (*Set, error) {
	sources := map[string]string{}
	names, err := fsys.Glob(ManifestDir + "/*.rego")
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "listing %s", ManifestDir)
	}
	for _, name := range names {
		// A parse error quotes the offending source, so a policy symlinked
		// to another file in the manifest directory, such as its .env,
		// would print that file into a pull request's CI log.
		info, err := fsys.Lstat(name)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading %s", name)
		}
		if !info.Mode().IsRegular() {
			return nil, kerrors.Validation("policy %s is not a regular file; a symlink is refused", name)
		}
		raw, err := fsys.ReadFile(name)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading %s", name)
		}
		sources[name] = string(raw)
	}
	extraSources := map[string]string{}
	for _, path := range extra {
		if err := readExtra(path, extraSources); err != nil {
			return nil, err
		}
	}

	set := &Set{}
	for _, group := range []map[string]string{sources, extraSources} {
		if len(group) == 0 {
			continue
		}
		u, err := compile(group)
		if err != nil {
			return nil, err
		}
		set.units = append(set.units, u)
	}
	return set, nil
}

// compile parses and compiles one group of policies on its own.
func compile(sources map[string]string) (unit, error) {
	caps := capabilities()
	modules := make(map[string]*ast.Module, len(sources))
	for name, source := range sources {
		module, err := ast.ParseModuleWithOpts(name, source, ast.ParserOptions{RegoVersion: ast.RegoV1, Capabilities: caps})
		if err != nil {
			return unit{}, kerrors.Wrap(err, kerrors.CodeValidation, "parsing policy %s", name)
		}
		if err := checkPackage(name, module); err != nil {
			return unit{}, err
		}
		modules[name] = module
	}

	compiler := ast.NewCompiler().WithCapabilities(caps)
	if compiler.Compile(modules); compiler.Failed() {
		return unit{}, kerrors.Wrap(compiler.Errors, kerrors.CodeValidation, "compiling policies")
	}

	gates := map[Gate]bool{}
	for _, module := range modules {
		gate, ok := gateOf(module)
		if !ok {
			continue
		}
		for _, rule := range module.Rules {
			if rule.Head.Name.String() != "deny" && rule.Head.Ref().String() != "deny" {
				continue
			}
			if rule.Head.RuleKind() != ast.MultiValue {
				return unit{}, kerrors.Validation(
					"policy %s: deny must be a set of messages (deny contains msg if { ... })", module.Package.Location.File)
			}
			gates[gate] = true
		}
	}
	if len(gates) == 0 {
		return unit{}, kerrors.Validation(
			"policies %s define no deny rule in package kraai.plan or kraai.destroy, so they would never deny anything",
			strings.Join(slices.Sorted(maps.Keys(sources)), ", "))
	}
	return unit{compiler: compiler, gates: gates}, nil
}

// readExtra adds the policy at path, or every .rego file directly in it.
func readExtra(path string, sources map[string]string) error {
	info, err := os.Stat(path)
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeValidation, "reading policy %s", path)
	}
	files := []string{path}
	if info.IsDir() {
		if files, err = filepath.Glob(filepath.Join(path, "*.rego")); err != nil {
			return kerrors.Wrap(err, kerrors.CodeValidation, "listing policies in %s", path)
		}
	}
	for _, file := range files {
		raw, err := os.ReadFile(file) //nolint:gosec // G304: a path the operator named on the command line
		if err != nil {
			return kerrors.Wrap(err, kerrors.CodeValidation, "reading policy %s", file)
		}
		sources[file] = string(raw)
	}
	return nil
}

// checkPackage admits the two gates' packages and helpers under kraai.lib.
// A misspelled package would otherwise define rules nothing queries.
func checkPackage(name string, module *ast.Module) error {
	pkg := strings.TrimPrefix(module.Package.Path.String(), "data.")
	if pkg == "kraai.plan" || pkg == "kraai.destroy" || strings.HasPrefix(pkg, "kraai.lib.") {
		return nil
	}
	return kerrors.Validation(
		"policy %s declares package %s; kraai evaluates kraai.plan and kraai.destroy, with helpers under kraai.lib",
		name, pkg)
}

func gateOf(module *ast.Module) (Gate, bool) {
	switch strings.TrimPrefix(module.Package.Path.String(), "data.") {
	case "kraai.plan":
		return GatePlan, true
	case "kraai.destroy":
		return GateDestroy, true
	}
	return "", false
}

// Evaluate returns every distinct message the gate's deny rules produce for
// input, sorted; none when no policy defines that gate. A message that is
// not a string is reported as its JSON.
func (s *Set) Evaluate(ctx context.Context, gate Gate, input map[string]any) ([]string, error) {
	if s.Empty() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()

	var denials []string
	for _, u := range s.units {
		if !u.gates[gate] {
			continue
		}
		found, err := u.evaluate(ctx, gate, input)
		if err != nil {
			return nil, err
		}
		denials = append(denials, found...)
	}
	if denials == nil {
		return nil, nil
	}
	sort.Strings(denials)
	return slices.Compact(denials), nil
}

func (u unit) evaluate(ctx context.Context, gate Gate, input map[string]any) ([]string, error) {
	results, err := rego.New(
		rego.Query("data.kraai."+string(gate)+".deny"),
		rego.Compiler(u.compiler),
		rego.Input(input),
		rego.Capabilities(capabilities()),
		rego.StrictBuiltinErrors(true),
	).Eval(ctx)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, kerrors.Validation("evaluating the %s policies took longer than %s", gate, evalTimeout)
		}
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "evaluating the %s policies", gate)
	}
	var denials []string
	for _, result := range results {
		for _, expression := range result.Expressions {
			values, _ := expression.Value.([]any)
			for _, value := range values {
				if text, ok := value.(string); ok {
					denials = append(denials, text)
					continue
				}
				encoded, _ := json.Marshal(value)
				denials = append(denials, string(encoded))
			}
		}
	}
	return denials, nil
}

// Denied is the error a command refuses with when a gate denies it.
func Denied(gate Gate, denials []string) error {
	return kerrors.Validation("policy denied the %s:\n  - %s", gate, strings.Join(denials, "\n  - "))
}
