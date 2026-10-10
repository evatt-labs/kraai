package aws

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// snapshotClockSkew widens the window a final snapshot is looked for in,
// since its create time is RDS's clock and the delete's start is ours.
const snapshotClockSkew = time.Minute

// Delete implements resource.Resource. Cloud Control's delete of a cluster
// always takes a final snapshot, named at random and untagged, which no
// property turns off. In an ephemeral environment kraai deletes every
// manual snapshot of the cluster, as it deletes every other store's data,
// a final snapshot an earlier destroy left included, so a destroy that
// failed partway is finished by the next; in any other the one this delete
// took is kept, and a note names it.
func (a *auroraClusterResource) Delete(ctx context.Context, ref resource.Ref) error {
	started := a.now()
	if err := a.translatedResource.Delete(ctx, ref); err != nil {
		return err
	}
	if a.client == nil || a.client.rds == nil {
		return nil
	}
	ephemeral := resource.Ephemeral(ctx)
	since := started.Add(-snapshotClockSkew)
	if ephemeral {
		since = time.Time{}
	}
	snapshots, err := a.finalSnapshots(ctx, ref.Name, since)
	if err != nil {
		return err
	}
	for _, id := range snapshots {
		if !ephemeral {
			resource.Note(ctx, fmt.Sprintf("kept %s, the final snapshot of cluster %s; "+
				"it is billed until deleted (aws rds delete-db-cluster-snapshot --db-cluster-snapshot-identifier %s)", id, ref.Name, id))
			continue
		}
		if err := a.deleteSnapshot(ctx, id); err != nil {
			return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting %s, the final snapshot of cluster %s", id, ref.Name)
		}
	}
	return nil
}

func (a *auroraClusterResource) now() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}

// finalSnapshots is the identifier of every manual snapshot of cluster
// made since since, or every one for a zero since.
func (a *auroraClusterResource) finalSnapshots(ctx context.Context, cluster string, since time.Time) ([]string, error) {
	var ids []string
	pages := rds.NewDescribeDBClusterSnapshotsPaginator(a.client.rds, &rds.DescribeDBClusterSnapshotsInput{
		DBClusterIdentifier: aws.String(cluster),
		SnapshotType:        aws.String("manual"),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "listing the snapshots of cluster %s", cluster)
		}
		for _, s := range page.DBClusterSnapshots {
			// A snapshot still being taken may have no create time yet.
			if s.SnapshotCreateTime == nil || !s.SnapshotCreateTime.Before(since) {
				ids = append(ids, aws.ToString(s.DBClusterSnapshotIdentifier))
			}
		}
	}
	return ids, nil
}

// deleteSnapshot deletes snapshot id, waiting while RDS is still taking
// it; one already gone is success.
func (a *auroraClusterResource) deleteSnapshot(ctx context.Context, id string) error {
	deadline := a.now().Add(a.snapshotTimeout())
	for {
		_, err := a.client.rds.DeleteDBClusterSnapshot(ctx, &rds.DeleteDBClusterSnapshotInput{DBClusterSnapshotIdentifier: aws.String(id)})
		var gone *rdstypes.DBClusterSnapshotNotFoundFault
		var busy *rdstypes.InvalidDBClusterSnapshotStateFault
		switch {
		case err == nil, errors.As(err, &gone):
			return nil
		case !errors.As(err, &busy):
			return err
		case !a.now().Before(deadline):
			return fmt.Errorf("still not deletable after %s: %w", a.snapshotTimeout(), err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.snapshotWait()):
		}
	}
}

func (a *auroraClusterResource) snapshotTimeout() time.Duration {
	if a.snapshotWaitFor > 0 {
		return a.snapshotWaitFor
	}
	return 15 * time.Minute
}

func (a *auroraClusterResource) snapshotWait() time.Duration {
	if a.snapshotPoll > 0 {
		return a.snapshotPoll
	}
	return 15 * time.Second
}
