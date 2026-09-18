package aws

import "github.com/evatt-labs/kraai/internal/kerrors"

// Settings are the AWS-specific values a manifest's providers.compute (or
// providers.objects) settings block carries for this provider.
//
// Free-form in the manifest — internal/manifest's Provider.Settings is
// uninterpreted map[string]any — and decoded here, in one place, rather
// than at each call site that needs a field out of it.
type Settings struct {
	// Region is the AWS region every Cloud Control and CloudFormation call
	// in this package targets. Optional: New passes it straight to the AWS
	// SDK's own region resolution (awsconfig.WithRegion), which treats an
	// empty value as "not configured" and falls through to its own default
	// chain (AWS_REGION, AWS_DEFAULT_REGION, the shared config file, IMDS).
	// An explicit value here still wins over all of those, because it is
	// the first source that chain checks — this field does not need to
	// re-implement that priority itself.
	Region string
}

// DecodeSettings reads Settings out of a manifest provider's Settings map.
// Exported so a caller assembling the registry can validate settings before
// wiring anything up, rather than at first use.
//
// Region absent is not an error — see Settings.Region's doc comment; a
// manifest with an opinion sets it, one without defers to the SDK. An
// earlier version of this function required it, which defeated that
// fallback outright: any manifest that omitted region failed to load at
// all rather than deferring to the SDK the way the fallback is supposed to.
// A present-but-wrong-typed value is still rejected, deliberately not
// folded into "absent": a manifest author who wrote `region: 12345` made a
// real mistake, and silently treating that the same as leaving the key out
// would hide it behind whatever region the SDK's chain happens to resolve
// to instead.
func DecodeSettings(config map[string]any) (Settings, error) {
	raw, present := config["region"]
	if !present {
		return Settings{}, nil
	}
	region, ok := raw.(string)
	if !ok {
		return Settings{}, kerrors.Validation("aws provider settings: region must be a string, got %T", raw)
	}
	return Settings{Region: region}, nil
}
