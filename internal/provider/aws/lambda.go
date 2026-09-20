package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// artifactObjectKey derives the deterministic S3 key a service's packaged
// artifact is stored under: {serviceName}/{sha256}.zip. Content-addressed
// rather than a fixed "latest.zip"-style key so two applies of identical
// source produce the identical key — the same object, no upload, no Code
// diff — while any real code change produces a new key and, downstream, a
// visible Lambda code update.
func artifactObjectKey(serviceName, sha256Hex string) string {
	return serviceName + "/" + sha256Hex + ".zip"
}

// lambdaFunctionResource provisions a service's Lambda function: packaging
// its deployment artifact, uploading it to the per-service artifact bucket,
// and wiring its execution role, environment and Web Adapter layer before
// delegating to the generic Cloud Control engine.
//
// # Why packaging lives in a wrapper, not the generic engine
//
// resourceType (resource.go) submits Spec.Config as Cloud Control's desired
// state directly — correct for a binding capability's resources, where
// internal/plan's expandBinding already builds a type-appropriate Config
// per registration. expandCompute (out of scope, unchanged by this
// workstream) builds one generic Config per service — {dir, settings,
// trigger, handler, schedule} — shared across every Tier 1/2 compute
// registration for that service, not a Lambda::Function property map. This
// type (and every other Tier 2 type in this package) translates that
// generic shape into its own real Cloud Control properties before
// delegating; "no new engine work" (the brief's own scope line) means
// resource.go's polling/patch/schema-diff mechanics stay untouched, not
// that every registration can skip translation.
//
// # Why plan-time and apply-time packaging are split
//
// Building the deployment zip is pure local file I/O — safe to do during
// `kraai plan`, which only ever calls Get and, where implemented,
// Diff (internal/plan narrows every resource.Resource to a
// getter, see that package's own doc). Uploading the built artifact to S3
// is not safe there: `kraai plan` must never mutate anything, and
// Diff's signature carries no context to run a network call
// under cleanly regardless. So Diff here (see its own doc
// comment) never builds or uploads an artifact at all — it only ever
// checks FunctionName, this type's sole createOnlyProperty — and the real
// packaging-plus-upload sequence runs exclusively inside Create/Update,
// which only `kraai apply` ever calls.
type lambdaFunctionResource struct {
	*resourceType
	client *Client
}

func newLambdaFunctionResource(client *Client) *lambdaFunctionResource {
	l := &lambdaFunctionResource{
		resourceType: &resourceType{provider: Provider, typeName: TypeLambdaFunction, lookup: resource.LookupByName, client: client},
		client:       client,
	}
	l.resourceType.translate = l.translate
	return l
}

// translate packages the service's artifact, uploads it, resolves its
// execution role's ARN and its environment (literal and secret-sourced),
// and builds the real AWS::Lambda::Function desired state. Every step here
// either performs local I/O (packaging) or a network call this method's
// ctx already carries — nothing here can run during `kraai plan`, only
// `kraai apply`'s Create/Update.
func (l *lambdaFunctionResource) translate(ctx context.Context, spec resource.Spec) (resource.Spec, error) {
	dir, _ := spec.Config["dir"].(string)
	if dir == "" {
		return resource.Spec{}, kerrors.Validation("binding %q declares no dir to package", spec.Binding)
	}
	handler, _ := spec.Config["handler"].(string)
	if handler == "" {
		return resource.Spec{}, kerrors.Validation("binding %q declares no compute.handler", spec.Binding)
	}

	settingsMap, _ := spec.Config["settings"].(map[string]any)
	lambdaSettings, err := decodeLambdaSettings(settingsMap)
	if err != nil {
		return resource.Spec{}, err
	}

	// include is set only when the service's manifest entry declares one
	// (internal/plan's expandCompute omits an empty slice from Config
	// entirely — see its own doc comment) so a plain type assertion, not a
	// defensive multi-type read like settingStr's: this comes straight from
	// manifest.Compute.Include, a typed []string field, never through a
	// free-form settings map that could carry some other shape.
	include, _ := spec.Config["include"].([]string)

	data, sha256Hex, err := buildArtifact(dir, include)
	if err != nil {
		return resource.Spec{}, err
	}
	bucket := artifactBucketName(spec.Name)
	key := artifactObjectKey(spec.Name, sha256Hex)
	if err := l.client.PutObject(ctx, bucket, key, data); err != nil {
		return resource.Spec{}, err
	}

	account, err := l.client.AccountID(ctx)
	if err != nil {
		return resource.Spec{}, err
	}
	// The execution role's own real name is spec.Name — the same derived
	// name every Tier 2 registration for this service shares (iamrole.go
	// sets RoleName to exactly this) — so its ARN is constructed the same
	// way eventsrule.go constructs the function's own: no live lookup. The
	// role is now a real DependsOn ahead of this function (register.go), so
	// a live lookup would be safe too; constructing it locally still avoids
	// a needless extra API call for a value this package can already
	// derive.
	execRoleARN := roleARN(account, spec.Name)

	env, err := resolveEnv(ctx, spec, lambdaSettings)
	if err != nil {
		return resource.Spec{}, err
	}

	translated := spec
	translated.Config = map[string]any{
		"FunctionName": spec.Name,
		"PackageType":  "Zip",
		"Code": map[string]any{
			"S3Bucket": bucket,
			"S3Key":    key,
		},
		"Handler":       handler,
		"Runtime":       lambdaSettings.Runtime,
		"Architectures": []any{lambdaSettings.Architecture},
		"MemorySize":    lambdaSettings.MemorySize,
		"Timeout":       lambdaSettings.Timeout,
		"Role":          execRoleARN,
		"Environment": map[string]any{
			"Variables": env,
		},
	}
	// Layers is emitted only when one was configured. A directly-invoked
	// function needs no layer, and sending Layers: [""] for it would be an
	// invalid ARN that Cloud Control rejects outright.
	if lambdaSettings.LayerArn != "" {
		translated.Config["Layers"] = []any{lambdaSettings.LayerArn}
	}

	// ReservedConcurrentExecutions is set only when the manifest actually
	// declared one — nil means "no opinion," not zero, and the two must
	// never collapse into the same desired-state shape. See
	// LambdaSettings.ReservedConcurrentExecutions' own doc comment
	// (compute_settings.go) for why, and for the live schema evidence that
	// this is the correct Cloud Control property name.
	if lambdaSettings.ReservedConcurrentExecutions != nil {
		translated.Config["ReservedConcurrentExecutions"] = *lambdaSettings.ReservedConcurrentExecutions
	}
	return translated, nil
}

// resolveEnv builds the function's environment variables: settings.Env
// passed through verbatim, plus settings.EnvSecrets resolved through
// spec.Secret at the point of use — the credential contract this package
// is written against (see the compute_settings.go LambdaSettings.EnvSecrets
// doc comment for the namespacing rule and why no binding name is ever
// hardcoded here).
func resolveEnv(ctx context.Context, spec resource.Spec, settings LambdaSettings) (map[string]any, error) {
	env := make(map[string]any, len(settings.Env)+len(settings.EnvSecrets))
	for k, v := range settings.Env {
		env[k] = v
	}
	for envVar, secretKey := range settings.EnvSecrets {
		value, err := spec.Secret(ctx, secretKey)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation,
				"resolving %q for environment variable %q", secretKey, envVar)
		}
		env[envVar] = value
	}
	return env, nil
}

// Diff checks only FunctionName, this type's sole
// createOnlyProperty (this package's own register.go: "FunctionName is
// settable at create; CloudFormation marks it 'Update requires:
// Replacement'"). See this type's own doc comment for
// why the real translate — packaging, upload, secret resolution — never
// runs here.
//
// Settings validation used to live here too, on the reasoning that this
// was "the one thing every compute service reaches unconditionally." That
// reasoning was wrong: Diff only runs once internal/plan's
// decide has already found an existing resource via Get, so it is never
// reached on a fresh environment's first plan, where every resource is
// ActionCreate — a typo'd or invalid setting reached nothing at all.
// ValidateSpec (below) is the actual unconditional reach now, via
// plan.SpecValidator; see its own doc comment for the fix and
// plan.SpecValidator's for the full failure mode this replaced.
func (l *lambdaFunctionResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	nameOnly := spec
	nameOnly.Config = map[string]any{"FunctionName": spec.Name}
	return l.compare(nameOnly, state)
}

// ValidateSpec implements plan.SpecValidator: decodeLambdaSettings is pure
// (no I/O) validation of the merged settings map — required
// runtime/architecture/layerArn, reservedConcurrency's type and sign,
// package's one accepted value, httpFrontDoor's two accepted values, and
// the unknown-key check (validateKnownSettings, settings_validate.go) —
// and this is where it runs unconditionally, before internal/plan's decide
// ever calls Get. See plan.SpecValidator's own doc comment for why this
// replaced hanging the same check off Diff, and for the real
// `kraai plan` evidence (a typo'd reservedConcurrency, an invalid
// httpFrontDoor) that Diff alone missed on a fresh environment.
func (l *lambdaFunctionResource) ValidateSpec(spec resource.Spec) error {
	settingsMap, _ := spec.Config["settings"].(map[string]any)
	_, err := decodeLambdaSettings(settingsMap)
	return err
}
