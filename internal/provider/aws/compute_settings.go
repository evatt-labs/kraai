package aws

import "github.com/evatt-labs/kraai/internal/kerrors"

// LambdaSettings are the Lambda-specific values a compute Spec's merged
// settings block carries. Decoded per call from Spec.Config["settings"],
// not once at registration: the planner merges a service's own override
// over the provider's settings before this package sees them, and decoding
// once would throw every override away.
type LambdaSettings struct {
	// Runtime is the Lambda runtime identifier, e.g. "python3.13".
	Runtime string
	// Architecture is "arm64" or "x86_64". A single string here; Lambda
	// accepts exactly one entry in the Architectures array.
	Architecture string
	// LayerArn is the ARN of the Lambda Web Adapter layer the deployment
	// package runs under, published by the author or by AWS; kraai does not
	// build one.
	LayerArn string
	// MemorySize and Timeout (seconds) default when unset.
	MemorySize int
	Timeout    int
	// Env is literal environment variable values, passed through unchanged.
	Env map[string]string
	// EnvSecrets maps an environment variable name to a namespaced secret
	// key, e.g. {"DATABASE_URL": "DB.connection_uri"}. The binding name is
	// whatever the author wrote; this package passes it to Spec.Secret.
	EnvSecrets map[string]string
	// ManagedPolicyArns are additional IAM managed policies to attach to
	// the execution role, beside the AWSLambdaBasicExecutionRole every role
	// gets.
	ManagedPolicyArns []string
	// HTTPFrontDoor selects which HTTP invoke path an HTTP-triggered
	// service gets: httpFrontDoorAPIGateway (the default) or
	// httpFrontDoorURL. Always one of the two after decode; the two
	// registrations are gated on this value and a service gets exactly one.
	HTTPFrontDoor string
	// ReservedConcurrentExecutions is Lambda's per-function concurrency
	// ceiling, the manifest's reservedConcurrency. A pointer because 0 is
	// Lambda's way to disable a function and nil means the property is not
	// set, leaving the account pool in force; the two must not collapse.
	ReservedConcurrentExecutions *int
}

const (
	defaultMemorySize = 512
	defaultTimeout    = 30

	// The two valid HTTPFrontDoor values. API Gateway is the default.
	httpFrontDoorAPIGateway = "apigateway"
	httpFrontDoorURL        = "url"

	// packageZip is the only value accepted for the manifest's "package"
	// key: this package builds a zip artifact and has no image path, so
	// "image" is rejected rather than silently built as a zip.
	packageZip = "zip"
)

// normalizeHTTPFrontDoor maps an unset value to the default and leaves
// anything else for the caller to validate. Shared by decodeLambdaSettings,
// which validates, and httpFrontDoorIs, which only compares.
func normalizeHTTPFrontDoor(raw string) string {
	if raw == "" {
		return httpFrontDoorAPIGateway
	}
	return raw
}

// decodeLambdaSettings reads LambdaSettings out of a compute Spec's merged
// settings map. The map already holds every key an author could write for
// this vendor, "region" and "functionUrlAuthType" included, which is why the
// unknown-key check runs here. Reached from translate and from ValidateSpec,
// so a fresh environment's first plan validates too.
//
// Runtime and Architecture are required. LayerArn is not: a directly
// invoked function exposes an ordinary handler and has no adapter to run
// under, and requiring one made such a function unplannable.
func decodeLambdaSettings(settings map[string]any) (LambdaSettings, error) {
	if err := computeSettingsSchema.Validate(settings); err != nil {
		return LambdaSettings{}, err
	}

	reservedConcurrency, err := settingIntPtr(settings, "reservedConcurrency")
	if err != nil {
		return LambdaSettings{}, err
	}
	if reservedConcurrency != nil && *reservedConcurrency < 0 {
		return LambdaSettings{}, kerrors.Validation(
			"aws lambda compute settings: reservedConcurrency must be >= 0, got %d", *reservedConcurrency)
	}

	if pkg := settingStr(settings, "package"); pkg != "" && pkg != packageZip {
		return LambdaSettings{}, kerrors.Validation(
			"aws lambda compute settings: package must be %q (or unset) — kraai only builds a .zip deployment artifact, got %q",
			packageZip, pkg)
	}

	s := LambdaSettings{
		Runtime:                      settingStr(settings, "runtime"),
		Architecture:                 settingStr(settings, "architecture"),
		LayerArn:                     settingStr(settings, "layerArn"),
		MemorySize:                   settingInt(settings, "memorySize", defaultMemorySize),
		Timeout:                      settingInt(settings, "timeout", defaultTimeout),
		Env:                          settingStrMap(settings, "env"),
		EnvSecrets:                   settingStrMap(settings, "envSecrets"),
		ReservedConcurrentExecutions: reservedConcurrency,
	}
	// Checked here so a typo'd scheme fails ValidateSpec on a fresh
	// environment's first plan, the same guarantee runtime and architecture
	// already get, rather than surfacing only once translate tries to
	// resolve it.
	for envVar, raw := range s.EnvSecrets {
		if err := validateSecretRef(raw); err != nil {
			return LambdaSettings{}, kerrors.Wrap(err, kerrors.CodeValidation, "envSecrets.%s", envVar)
		}
	}
	// A manifest author's own variable must never collide with the marker
	// resolveEnv writes beside a secret-backed one (secretVersionMarkerName):
	// either would silently overwrite the other in the function's
	// environment, and whichever lost would never be checked here again,
	// the same "runs sometimes" failure this package's ValidateSpec
	// checks exist to catch at a fresh environment's first plan.
	for envVar := range s.EnvSecrets {
		marker := secretVersionMarkerName(envVar)
		if _, clash := s.Env[marker]; clash {
			return LambdaSettings{}, kerrors.Validation(
				"aws lambda compute settings: env.%s collides with the version marker kraai writes for envSecrets.%s; rename one",
				marker, envVar)
		}
		if _, clash := s.EnvSecrets[marker]; clash {
			return LambdaSettings{}, kerrors.Validation(
				"aws lambda compute settings: envSecrets.%s collides with the version marker kraai writes for envSecrets.%s; rename one",
				marker, envVar)
		}
	}
	if arns, ok := settings["managedPolicyArns"].([]any); ok {
		for _, a := range arns {
			if str, ok := a.(string); ok && str != "" {
				s.ManagedPolicyArns = append(s.ManagedPolicyArns, str)
			}
		}
	}

	var missing []string
	if s.Runtime == "" {
		missing = append(missing, "runtime")
	}
	if s.Architecture == "" {
		missing = append(missing, "architecture")
	}
	if len(missing) > 0 {
		return LambdaSettings{}, kerrors.Validation(
			"aws lambda compute is missing required settings: %v", missing)
	}

	// Validated here because an applicability condition has no error
	// channel: an unrecognized value would make both front-door conditions
	// false and the service would silently plan no front door at all.
	s.HTTPFrontDoor = normalizeHTTPFrontDoor(settingStr(settings, "httpFrontDoor"))
	if s.HTTPFrontDoor != httpFrontDoorAPIGateway && s.HTTPFrontDoor != httpFrontDoorURL {
		return LambdaSettings{}, kerrors.Validation(
			"aws lambda compute settings: httpFrontDoor must be %q or %q (or unset, defaulting to %q), got %q",
			httpFrontDoorAPIGateway, httpFrontDoorURL, httpFrontDoorAPIGateway, s.HTTPFrontDoor)
	}
	return s, nil
}

// httpFrontDoorIs builds the closure register.go hands to
// resource.RequiresSettings, matching when a service's merged compute
// settings select want. It reads the raw setting rather than calling
// decodeLambdaSettings: it runs while resolving every compute registration,
// including ones that see nil settings and have no opinion on runtime. An
// invalid value still cannot select nothing silently, because the
// function's ValidateSpec rejects it on every plan.
func httpFrontDoorIs(want string) func(settings map[string]any) bool {
	return func(settings map[string]any) bool {
		return normalizeHTTPFrontDoor(settingStr(settings, "httpFrontDoor")) == want
	}
}

func settingStr(settings map[string]any, key string) string {
	v, _ := settings[key].(string)
	return v
}

// settingInt reads an integer-valued setting, accepting the int a YAML
// decoder produces and the float64 a JSON round trip (--set) does.
func settingInt(settings map[string]any, key string, fallback int) int {
	switch v := settings[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return fallback
	}
}

// settingIntPtr reads an optional integer-valued setting, distinguishing
// absent (nil) from any value including zero. A present but wrong-typed
// value is an error, since coercing it would pick one of two opposite
// meanings for the author.
func settingIntPtr(settings map[string]any, key string) (*int, error) {
	raw, present := settings[key]
	if !present {
		return nil, nil
	}
	switch v := raw.(type) {
	case int:
		return &v, nil
	case float64:
		i := int(v)
		return &i, nil
	default:
		return nil, kerrors.Validation("aws lambda compute settings: %s must be an integer, got %T", key, raw)
	}
}

// settingStrMap reads a string-to-string map setting from the
// map[string]any a YAML decoder produces. Wrongly typed entries are dropped
// rather than failing the decode.
func settingStrMap(settings map[string]any, key string) map[string]string {
	raw, ok := settings[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
