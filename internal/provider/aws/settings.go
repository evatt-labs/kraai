package aws

import "github.com/evatt-labs/kraai/internal/kerrors"

// Settings are the AWS-specific values a manifest's provider settings block
// carries, decoded here in one place.
type Settings struct {
	// Region is the AWS region every call targets. Optional: an empty value
	// defers to the SDK's own resolution chain (AWS_REGION, the shared
	// config file, IMDS), and an explicit one wins over all of them.
	Region string
}

// DecodeSettings reads Settings out of a manifest provider's settings map.
// Region absent is not an error; a present but wrong-typed value is, since
// `region: 12345` is a real mistake and folding it into "absent" would hide
// it behind whatever the SDK resolves.
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
