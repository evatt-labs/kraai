package assemble

import (
	"context"

	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws"
)

// LockStore returns the store that holds the manifest's environment locks
// and status records: an S3 bucket in the AWS account the manifest's
// provider settings name, when the manifest configures any capability with
// vendor aws. A manifest with no AWS provider gets lock.ErrNoStore, and its
// caller decides how loudly to proceed unguarded.
func LockStore(ctx context.Context, m *manifest.Manifest) (lock.Store, error) {
	vendors, err := vendorsUsed(m)
	if err != nil {
		return nil, err
	}
	provider, ok := vendors[vendorAWS]
	if !ok {
		return nil, lock.ErrNoStore
	}
	settings, err := aws.DecodeSettings(provider.Settings)
	if err != nil {
		return nil, err
	}
	client, err := aws.New(ctx, settings)
	if err != nil {
		return nil, err
	}
	return aws.NewLockStore(client), nil
}
