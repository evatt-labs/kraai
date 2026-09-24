package policy

import (
	"context"
	"encoding/json"
	"errors"
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
	compiler *ast.Compiler
	// gates are those with a deny rule defined.
	gates map[Gate]bool
}

// Empty reports whether no policy was loaded.
func (s *Set) Empty() bool { return s == nil || s.compiler == nil }

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
// of extra, a file or a directory of .rego files, and compiles them. None
// at all is an empty Set. A policy that does not parse or compile, calls a
// forbidden builtin, declares a package kraai does not evaluate, or leaves
// both gates without a deny rule fails the load: a policy that silently
// never ran would read as one that passed.
func Load(fsys manifest.FS, extra []string) (*Set, error) {
	sources := map[string]string{}
	names, err := fsys.Glob(ManifestDir + "/*.rego")
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "listing %s", ManifestDir)
	}
	for _, name := range names {
		raw, err := fsys.ReadFile(name)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading %s", name)
		}
		sources[name] = string(raw)
	}
	for _, path := range extra {
		if err := readExtra(path, sources); err != nil {
			return nil, err
		}
	}
	if len(sources) == 0 {
		return &Set{}, nil
	}

	caps := capabilities()
	modules := make(map[string]*ast.Module, len(sources))
	for name, source := range sources {
		module, err := ast.ParseModuleWithOpts(name, source, ast.ParserOptions{RegoVersion: ast.RegoV1, Capabilities: caps})
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation, "parsing policy %s", name)
		}
		if err := checkPackage(name, module); err != nil {
			return nil, err
		}
		modules[name] = module
	}

	compiler := ast.NewCompiler().WithCapabilities(caps)
	if compiler.Compile(modules); compiler.Failed() {
		return nil, kerrors.Wrap(compiler.Errors, kerrors.CodeValidation, "compiling policies")
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
				return nil, kerrors.Validation(
					"policy %s: deny must be a set of messages (deny contains msg if { ... })", module.Package.Location.File)
			}
			gates[gate] = true
		}
	}
	if len(gates) == 0 {
		return nil, kerrors.Validation(
			"policies define no deny rule in package kraai.plan or kraai.destroy, so they would never deny anything")
	}
	return &Set{compiler: compiler, gates: gates}, nil
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

// Evaluate returns every message the gate's deny rule produces for input,
// sorted; none when no policy defines that gate. A message that is not a
// string is reported as its JSON.
func (s *Set) Evaluate(ctx context.Context, gate Gate, input map[string]any) ([]string, error) {
	if s.Empty() || !s.gates[gate] {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()

	results, err := rego.New(
		rego.Query("data.kraai."+string(gate)+".deny"),
		rego.Compiler(s.compiler),
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
	sort.Strings(denials)
	return denials, nil
}

// Denied is the error a command refuses with when a gate denies it.
func Denied(gate Gate, denials []string) error {
	return kerrors.Validation("policy denied the %s:\n  - %s", gate, strings.Join(denials, "\n  - "))
}
