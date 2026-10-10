package aws

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	"github.com/evatt-labs/kraai/internal/resource"
)

// fakeRDS answers the snapshot calls: snapshots by create time, and, per
// identifier, the errors its deletes return in turn, the last for good (nil
// for success).
type fakeRDS struct {
	snapshots map[string]*time.Time
	deleteErr map[string][]error
	deleted   []string
}

func (f *fakeRDS) DescribeDBClusterSnapshots(_ context.Context, in *rds.DescribeDBClusterSnapshotsInput, _ ...func(*rds.Options)) (*rds.DescribeDBClusterSnapshotsOutput, error) {
	if aws.ToString(in.SnapshotType) != "manual" {
		return nil, errors.New("only manual snapshots are final snapshots")
	}
	out := &rds.DescribeDBClusterSnapshotsOutput{}
	for id, at := range f.snapshots {
		out.DBClusterSnapshots = append(out.DBClusterSnapshots, rdstypes.DBClusterSnapshot{
			DBClusterSnapshotIdentifier: aws.String(id), DBClusterIdentifier: in.DBClusterIdentifier, SnapshotCreateTime: at,
		})
	}
	return out, nil
}

func (f *fakeRDS) DeleteDBClusterSnapshot(_ context.Context, in *rds.DeleteDBClusterSnapshotInput, _ ...func(*rds.Options)) (*rds.DeleteDBClusterSnapshotOutput, error) {
	id := aws.ToString(in.DBClusterSnapshotIdentifier)
	if errs := f.deleteErr[id]; len(errs) > 0 {
		if len(errs) > 1 {
			f.deleteErr[id] = errs[1:]
		}
		if errs[0] != nil {
			return nil, errs[0]
		}
	}
	f.deleted = append(f.deleted, id)
	return &rds.DeleteDBClusterSnapshotOutput{}, nil
}

// testCluster is a cluster resource over fakes, found by name, whose clock
// stands at now.
func testCluster(fc *fakeClient, api *fakeRDS, now time.Time) *auroraClusterResource {
	engine := withIdentity(&resourceType{provider: Provider, typeName: TypeRDSDBCluster, lookup: resource.LookupByName, client: fc})
	return &auroraClusterResource{
		translatedResource: translated(engine, func(s resource.Spec) (resource.Spec, error) { return s, nil }, nil),
		client:             &Client{rds: api},
		clock:              func() time.Time { return now },
		snapshotPoll:       time.Millisecond,
		snapshotWaitFor:    time.Hour,
	}
}

// In an ephemeral environment every manual snapshot of a deleted cluster
// is deleted, waiting while RDS still takes one, an earlier destroy's
// leftover included. In any other the final snapshot this delete took is
// kept with a note naming it, and an older one, someone's on purpose, is
// not mentioned.
func TestAuroraDeleteHandlesTheFinalSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	after, before := now.Add(30*time.Second), now.Add(-time.Hour)
	for name, c := range map[string]struct {
		ephemeral   bool
		wantDeleted []string
		wantNote    string
	}{
		"ephemeral":  {ephemeral: true, wantDeleted: []string{"kept-on-purpose", "rds-snapshot-final"}},
		"persistent": {wantNote: "kept rds-snapshot-final, the final snapshot of cluster env-svc-sql"},
	} {
		t.Run(name, func(t *testing.T) {
			fc := &fakeClient{byIdentifier: map[string]map[string]any{
				"env-svc-sql": taggedProps("env-svc-sql", map[string]any{"DBClusterIdentifier": "env-svc-sql"}),
			}}
			api := &fakeRDS{
				snapshots: map[string]*time.Time{"rds-snapshot-final": &after, "kept-on-purpose": &before},
				deleteErr: map[string][]error{"rds-snapshot-final": {&rdstypes.InvalidDBClusterSnapshotStateFault{}, nil}},
			}
			ctx, notes := resource.WithNotes(context.Background())
			if c.ephemeral {
				ctx = resource.WithEphemeral(ctx)
			}
			if err := testCluster(fc, api, now).Delete(ctx, resource.Ref{Name: "env-svc-sql"}); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if !reflect.DeepEqual(fc.deleteCalls, []string{"env-svc-sql"}) {
				t.Fatalf("cluster deletes = %v", fc.deleteCalls)
			}
			slices.Sort(api.deleted)
			if !reflect.DeepEqual(api.deleted, c.wantDeleted) {
				t.Fatalf("snapshots deleted = %v, want %v", api.deleted, c.wantDeleted)
			}
			got := notes.All()
			if c.wantNote == "" && len(got) != 0 || c.wantNote != "" && (len(got) != 1 || !strings.HasPrefix(got[0], c.wantNote)) {
				t.Fatalf("notes = %q, want %q", got, c.wantNote)
			}
		})
	}
}

// A snapshot RDS keeps refusing to delete fails the delete once the wait
// runs out, naming it, rather than leaving it silently; one already gone
// is success.
func TestAuroraFinalSnapshotDeleteFailsLoudly(t *testing.T) {
	now := time.Now()
	fc := &fakeClient{byIdentifier: map[string]map[string]any{
		"env-svc-sql": taggedProps("env-svc-sql", map[string]any{"DBClusterIdentifier": "env-svc-sql"}),
	}}
	api := &fakeRDS{
		snapshots: map[string]*time.Time{"stuck": &now, "gone": &now},
		deleteErr: map[string][]error{
			"stuck": {&rdstypes.InvalidDBClusterSnapshotStateFault{}},
			"gone":  {&rdstypes.DBClusterSnapshotNotFoundFault{}},
		},
	}
	cluster := testCluster(fc, api, now)
	cluster.clock = nil
	cluster.snapshotWaitFor = 5 * time.Millisecond
	err := cluster.Delete(resource.WithEphemeral(context.Background()), resource.Ref{Name: "env-svc-sql"})
	if err == nil || !strings.Contains(err.Error(), "stuck") {
		t.Fatalf("Delete = %v, want a failure naming the stuck snapshot", err)
	}
}

// A cluster that is not there, or not kraai's, takes no snapshot and has
// none looked for beyond the list.
func TestAuroraDeleteOfAnAbsentClusterDeletesNoSnapshot(t *testing.T) {
	now := time.Now()
	api := &fakeRDS{snapshots: map[string]*time.Time{}}
	if err := testCluster(&fakeClient{}, api, now).Delete(resource.WithEphemeral(context.Background()), resource.Ref{Name: "env-svc-sql"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("snapshots deleted = %v, want none", api.deleted)
	}
}
