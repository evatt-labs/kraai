package aws

import "github.com/evatt-labs/kraai/internal/kerrors"

// LambdaSettings are the AWS Lambda-specific values a compute Spec's merged
// settings block carries, decoded per call from Spec.Config["settings"]
// rather than once at registration.
//
// # Why per call, not once at registration (contrast with neonresource.BranchSettings)
//
// A Neon branch has no per-binding settings override in the manifest schema
// today, so neonresource decodes BranchSettings once from
// providers.database.settings and closes over it. Compute settings are
// different: internal/plan's expandCompute already layers a service's own
// Compute.Settings over providers.compute.settings per top-level key
// (manifest.MergeSettings) before it ever reaches this package, precisely
// so one service can override, say, memorySize without every other
// service's Lambda inheriting it. Decoding once at registration would
// throw that override away — every service would get the provider's
// settings only, silently. Decoding from Spec.Config on every call is what
// lets the merge planner already performs actually reach the function.
type LambdaSettings struct {
	// Runtime is the Lambda runtime identifier, e.g. "python3.13".
	Runtime string
	// Architecture is the instruction set Lambda runs the function on, e.g.
	// "arm64" or "x86_64". CloudFormation's Architectures property is an
	// array, but Lambda accepts exactly one entry per function — a second
	// entry is rejected by the API itself — so this is a single string here
	// and wrapped into a one-element array when building the desired state.
	Architecture string
	// LayerArn is the ARN of the Lambda Web Adapter layer this function's
	// deployment package runs under. kraai does not build or publish this
	// layer itself (aws-provider-compute's brief): a manifest author
	// supplies the ARN of a layer version they published or one of AWS's
	// own published adapter layer ARNs.
	LayerArn string
	// MemorySize and Timeout configure the function's resource allocation
	// and maximum execution duration in seconds. Optional: defaultMemorySize
	// and defaultTimeout apply when unset, rather than forcing every
	// manifest to state them.
	MemorySize int
	Timeout    int
	// Env is literal (non-secret) environment variable values, passed
	// through to the function's Environment.Variables unchanged.
	Env map[string]string
	// EnvSecrets maps an environment variable name to the credential
	// contract's namespaced secret key — e.g. {"DATABASE_URL":
	// "DB.connection_uri"} when the service declares a database binding
	// named "DB". The binding name is whatever string the manifest author
	// wrote here; this package never hardcodes one, satisfying the
	// credential contract's namespacing (own binding's secrets bare,
	// other readable bindings' secrets as "<binding>.<name>") without this
	// package needing to know the manifest's binding vocabulary at all —
	// it only ever passes the string through to Spec.Secret.
	EnvSecrets map[string]string
	// ManagedPolicyArns are additional IAM managed policy ARNs to attach to
	// the function's execution role, alongside the
	// AWSLambdaBasicExecutionRole policy every execution role gets
	// unconditionally (CloudWatch Logs access; a function that cannot
	// write its own logs is not usefully deployable).
	ManagedPolicyArns []string
	// HTTPFrontDoor selects which of this package's two HTTP invoke paths
	// an HTTP-triggered service gets: httpFrontDoorAPIGateway (the
	// default) or httpFrontDoorURL. Always normalized to one of those two
	// values by decodeLambdaSettings — never empty, and never anything
	// else, because register.go's registrations for both paths are gated
	// on this exact value via resource.RequiresSettings and a service must
	// get exactly one. See httpFrontDoorIs's own doc comment
	// for why the planner-facing selector reads the raw setting directly
	// rather than going through this validated decode.
	HTTPFrontDoor string
	// ReservedConcurrentExecutions is Lambda's per-function concurrency
	// ceiling — the manifest's `reservedConcurrency` key, renamed to match
	// what AWS actually calls the property. Verified against Cloud
	// Control's live AWS::Lambda::Function resource schema
	// (`aws cloudformation describe-type --type RESOURCE --type-name
	// AWS::Lambda::Function`, 2026-09-15): the schema names it
	// `ReservedConcurrentExecutions`, type integer, minimum 0, and it sits
	// directly on the function resource — there is no separate
	// AWS::Lambda::EventInvokeConfig-style companion resource for it, so
	// lambda.go's translate can set it straight into the same desired
	// state as every other function property.
	//
	// A pointer, not a bare int: kraai-api's BLUEPRINT.md 5.6 calls this
	// "the real cost/DB-pressure guardrail," and 0 is Lambda's documented
	// way to throttle a function to zero concurrent executions — disabling
	// it — which is nothing like "no opinion, don't set the property."
	// Collapsing absent and 0 into the same bare int would have
	// reintroduced this exact bug's own failure mode one field over:
	// either a manifest author cannot express "disable this function" at
	// all, or every unset service silently gets throttled to zero. nil
	// means the manifest set nothing and lambda.go omits the property
	// entirely, leaving Lambda's own default (unreserved, account-pool
	// concurrency) in force; a non-nil zero means the manifest asked for
	// zero and lambda.go sends exactly that.
	ReservedConcurrentExecutions *int
}

const (
	defaultMemorySize = 512
	defaultTimeout    = 30

	// httpFrontDoorAPIGateway and httpFrontDoorURL are LambdaSettings.
	// HTTPFrontDoor's only two valid values, and the two front doors
	// register.go's ApiGatewayV2::Api and Lambda::Url registrations
	// select between via their settings conditions.
	//
	// API Gateway is the default: kraai-api's own documented topology is
	// "FastAPI app behind API Gateway HTTP API, deployed via AWS Lambda Web
	// Adapter" — an unconfigured service
	// should get the shape kraai's own first real consumer actually uses,
	// not the newer/simpler alternative this package happens to register
	// second.
	httpFrontDoorAPIGateway = "apigateway"
	httpFrontDoorURL        = "url"

	// packageZip is the only value decodeLambdaSettings accepts for the
	// manifest's "package" key. kraai's Tier 2
	// compute registration (this package's own doc comment, lambda.go)
	// only ever builds a .zip deployment artifact — there is no container
	// build/push path here to honor `package: image` even though Cloud
	// Control's PackageType enum allows it (verified against the same
	// live AWS::Lambda::Function schema fetch as
	// ReservedConcurrentExecutions: PackageType is createOnly, enum
	// ["Image", "Zip"]). See decodeLambdaSettings's own handling of the
	// "package" key for why an unsupported value is rejected rather than
	// silently built as a zip anyway.
	packageZip = "zip"
)

// normalizeHTTPFrontDoor maps an unset value to the documented default,
// leaving anything else (valid or not) unchanged for the caller to
// validate. Shared by decodeLambdaSettings (which validates and errors on
// anything else) and httpFrontDoorIs (the closures register.go hands to
// resource.RequiresSettings, which only need to compare — see that
// function's own doc comment for why it does not itself validate).
func normalizeHTTPFrontDoor(raw string) string {
	if raw == "" {
		return httpFrontDoorAPIGateway
	}
	return raw
}

// decodeLambdaSettings reads LambdaSettings out of a compute Spec's merged
// settings map (Spec.Config["settings"]).
//
// The settings map it receives is already the full merge of
// providers.compute.settings and any per-service override
// (manifest.MergeSettings, internal/plan's expandCompute) — so it is also
// the one decoder that actually holds every key a manifest author could
// have written for this vendor, DecodeSettings' "region" and
// lambdaurl.go's "functionUrlAuthType" included. That is why the
// unknown-key check (computeSettingsSchema.Validate, settings_schema.go)
// runs from here rather than from DecodeSettings or at registry-assembly
// time.
//
// This function alone is called from more than one place —
// lambdaFunctionResource.translate (Create/Update) and
// lambdaFunctionResource.ValidateSpec (lambda.go), the latter being what
// internal/plan's decide reaches unconditionally through plan.SpecValidator
// for every planned action, including a brand-new environment's very first
// ActionCreate. See ValidateSpec's own doc comment for why that unconditional
// reach matters and what it replaced (decodeLambdaSettings used to be
// reachable only via DiffersFromState, which only ever runs once a resource
// already exists — the exact gap that let a typo'd reservedConcurrency and
// an invalid httpFrontDoor both plan clean against a fresh environment).
//
// Runtime and Architecture are required: without them there is no deployable
// function at all, since nothing says which interpreter or instruction set
// to build for.
//
// LayerArn is NOT required, deliberately. An earlier version demanded it on
// the reasoning that there would be "no adapter to run an ASGI app under" —
// which silently assumed every function is a web application behind the
// Lambda Web Adapter. A directly-invoked function is not: it exposes an
// ordinary handler, is called by a scheduler or another service rather than
// over HTTP, and attaching a web adapter layer to it would be meaningless.
// Requiring one made such a function unplannable, which the real consumer
// manifest hit immediately on its schedule-triggered service.
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

	// httpFrontDoor is validated here, not just left to select nothing:
	// register.go's ApiGatewayV2::Api and Lambda::Url registrations are both
	// conditioned on this value, and an applicability condition has no error
	// channel of its own — an unrecognized value would make both conditions
	// return false and the service would silently plan no HTTP front door at
	// all, rather than the loud failure an invalid manifest value deserves
	// (Rule 20). Checked in decodeLambdaSettings, not inside httpFrontDoorIs
	// itself, so it is caught once, centrally, rather than by every caller of
	// that selector remembering to.
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
// settings select want.
//
// Reads the raw setting directly via settingStr/normalizeHTTPFrontDoor
// rather than calling the validating decodeLambdaSettings: this runs while
// resolving every compute registration, including AWS::IAM::Role and
// AWS::SSM::Parameter, which carry no opinion on httpFrontDoor at all and
// whose own registrations see nil settings maps in some call paths —
// decodeLambdaSettings would refuse those on missing
// runtime/architecture/layerArn, coupling front-door selection to
// requirements that have nothing to do with it.
//
// An actually-invalid value still cannot select nothing silently:
// decodeLambdaSettings validates it too, and AWS::Lambda::Function's
// ValidateSpec (lambda.go, implementing plan.SpecValidator) is what
// internal/plan's decide reaches unconditionally for every planned
// action — Create, NoChange and Replace alike — so `kraai plan` always
// surfaces the same error this selector would otherwise swallow, on a
// fresh environment included.
//
// This was previously claimed of DiffersFromState/Create/Update instead,
// which was false on exactly the path that matters most: DiffersFromState
// only runs once decide has already found an existing resource via Get,
// and Create/Update only run under `kraai apply`, never `kraai plan`. A
// service planned into a brand-new environment with an invalid
// httpFrontDoor got ActionCreate for AWS::Lambda::Function (DiffersFromState
// never reached) while the settings condition silently matched neither
// AWS::ApiGatewayV2::Api nor AWS::Lambda::Url for the exact same reason
// this comment describes below — the plan came out two resources short
// with nothing in the output saying why. ValidateSpec closes that gap:
// see plan.SpecValidator's own doc comment for the general fix and the
// real `kraai plan` evidence this bug produced.
//
// An applicability condition still has no error channel and still cannot
// report "you asked for a front door that does not exist" on its own — an
// invalid value still makes both AWS::ApiGatewayV2::Api's and
// AWS::Lambda::Url's condition return false, so neither is ever planned as
// its own Action, with or without this fix.
// What changed is that this is no longer silent: AWS::Lambda::Function's
// own ActionFailed, reachable on every path now, is the loud signal this
// selector was always designed to depend on instead of reporting the
// problem itself.
func httpFrontDoorIs(want string) func(settings map[string]any) bool {
	return func(settings map[string]any) bool {
		return normalizeHTTPFrontDoor(settingStr(settings, "httpFrontDoor")) == want
	}
}

func settingStr(settings map[string]any, key string) string {
	v, _ := settings[key].(string)
	return v
}

// settingInt reads an integer-valued setting. A manifest is YAML/JSON
// decoded, so a whole-number value normally arrives as int (YAML) or
// float64 (JSON/encoding-json round trips, e.g. via --set); both are
// accepted rather than forcing a manifest author to know which decoder
// produced the value they wrote.
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
// absent (nil) from any concrete value it was set to — including zero.
// Unlike settingInt, which folds a missing or wrong-typed value into the
// same fallback, a present-but-wrong-typed value here is a real error: the
// same reasoning DecodeSettings' own doc comment (settings.go) gives for
// region, and load-bearing for reservedConcurrency specifically, where nil
// and 0 mean opposite things (see LambdaSettings.
// ReservedConcurrentExecutions' own doc comment) and silently coercing a
// bad value to either would pick one of those meanings for the manifest
// author.
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

// settingStrMap reads a string-to-string map setting (env/envSecrets),
// tolerating the map[string]any shape a YAML decoder produces for a nested
// mapping. Absent or wrongly-typed entries are dropped rather than failing
// the whole decode — an env var block is additive convenience, not
// something a manifest author cannot function without.
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
