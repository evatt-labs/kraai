//go:build integration

package direct

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
)

var updateEvidence = flag.Bool("update-evidence", false, "rewrite evidence/parity.json from this run")

// Read parity, read-only: every reader's direct read of each listed instance
// against Cloud Control's read of the same instance. Needs AWS credentials;
// creates nothing.
//
//	go test -tags integration ./internal/provider/aws/direct -run TestReadParity [-args -update-evidence]
func TestReadParity(t *testing.T) {
	const region, perType = "us-east-1", 20
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	cc := cloudcontrol.NewFromConfig(cfg)
	client := &Client{HTTP: &http.Client{Timeout: 30 * time.Second}, Credentials: cfg.Credentials, Region: region}
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}

	var evidence Evidence
	for _, r := range Readers() {
		e := TypeEvidence{Type: r.Type, SmithyCommit: lock.SmithyCommit, Date: time.Now().UTC().Format("2006-01-02"), Region: region}
		// A type with a direct list is listed through it: Cloud Control's
		// list omits its instances. Cloud Control still reads each one.
		var ids []string
		if HasList(r.Type) {
			if ids, err = client.List(ctx, r.Type); err != nil {
				t.Fatalf("%s: listing directly: %v", r.Type, err)
			}
		} else {
			listed, err := cc.ListResources(ctx, &cloudcontrol.ListResourcesInput{TypeName: aws.String(r.Type), MaxResults: aws.Int32(perType)})
			if err != nil {
				t.Fatalf("%s: listing: %v", r.Type, err)
			}
			for _, d := range listed.ResourceDescriptions {
				ids = append(ids, aws.ToString(d.Identifier))
			}
		}
		if len(ids) > perType {
			ids = ids[:perType]
		}
		compared, differing, unreadable := map[string]bool{}, map[string]bool{}, 0
		for _, id := range ids {
			got, err := cc.GetResource(ctx, &cloudcontrol.GetResourceInput{TypeName: aws.String(r.Type), Identifier: aws.String(id)})
			if err != nil {
				unreadable++
				continue
			}
			var viaCC map[string]any
			if err := json.Unmarshal([]byte(aws.ToString(got.ResourceDescription.Properties)), &viaCC); err != nil {
				t.Fatal(err)
			}
			direct, err := client.Read(ctx, r.Type, map[string]string{r.Identifier[0].Property: id})
			if err != nil {
				t.Errorf("%s: direct read failed: %v", r.Type, err)
				continue
			}
			diffs, err := Compare(r.Type, viaCC, direct)
			if err != nil {
				t.Fatal(err)
			}
			skip := skipTreeFor(t, r.Type)
			for _, side := range []map[string]any{viaCC, direct} {
				for k := range side {
					if v, skipped := skip[k]; !skipped || v != nil {
						compared[k] = true
					}
				}
			}
			for _, diff := range diffs {
				differing[diff.Property] = true
				// Values print to this run's log only, never to evidence.
				t.Errorf("%s %s: cloudcontrol=%s direct=%s", r.Type, diff.Property, diff.CloudControl, diff.Direct)
			}
			e.Instances++
		}
		e.Compared, e.Differing = sortedKeys(compared), sortedKeys(differing)
		switch {
		case len(differing) > 0:
			e.Outcome = "differs"
		case e.Instances == 0 && unreadable > 0:
			e.Outcome = "oracle-unavailable"
			e.Note = "Cloud Control could not read any listed instance"
		case e.Instances == 0:
			e.Outcome = "no-instances"
		default:
			e.Outcome = "parity"
		}
		if unreadable > 0 && e.Instances > 0 {
			e.Note = "Cloud Control could not read some listed instances"
		}
		t.Logf("%s: %s over %d instances, %d unreadable by Cloud Control", r.Type, e.Outcome, e.Instances, unreadable)
		evidence.Types = append(evidence.Types, e)
	}
	sort.Slice(evidence.Types, func(i, j int) bool { return evidence.Types[i].Type < evidence.Types[j].Type })
	if *updateEvidence && !t.Failed() {
		raw, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("evidence/parity.json", append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func skipTreeFor(t *testing.T, typeName string) map[string]any {
	t.Helper()
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		if o.Type == typeName {
			return skipTree(o.Properties, o.Skip)
		}
	}
	t.Fatalf("%s has no override", typeName)
	return nil
}
