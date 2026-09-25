//go:build integration

package direct

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// account is the account the harness runs in, for AbsentIDs' {account}.
var account string

var (
	updateEvidence = flag.Bool("update-evidence", false, "merge this run into evidence/parity.json")
	onlyTypes      = flag.String("types", "", "comma-separated types to run; every reader when empty")
)

// Read parity, read-only: every reader's direct read of each listed instance
// against Cloud Control's read of the same instance. Needs AWS credentials;
// creates nothing.
//
//	go test -tags integration ./internal/provider/aws/direct -run TestReadParity [-args -update-evidence -types AWS::X::Y,...]
func TestReadParity(t *testing.T) {
	const region, perType = "us-east-1", 20
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	cc := cloudcontrol.NewFromConfig(cfg)
	who, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}
	account = aws.ToString(who.Account)
	client := &Client{HTTP: &http.Client{Timeout: 30 * time.Second}, Credentials: cfg.Credentials, Region: region}
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	only := map[string]bool{}
	for _, typeName := range strings.Split(*onlyTypes, ",") {
		if typeName != "" {
			only[typeName] = true
		}
	}

	var run Evidence
	readers := map[string]bool{}
	for _, r := range Readers() {
		readers[r.Type] = true
		if len(only) > 0 && !only[r.Type] {
			continue
		}
		e := TypeEvidence{Type: r.Type, SmithyCommit: lock.SmithyCommit, Date: time.Now().UTC().Format("2006-01-02"), Region: region}
		t.Run(r.Type, func(t *testing.T) {
			e = readParity(ctx, t, cc, client, r, e, perType)
		})
		t.Logf("%s: %s over %d instances", r.Type, e.Outcome, e.Instances)
		run.Types = append(run.Types, e)
	}
	if !*updateEvidence {
		return
	}
	var prior Evidence
	if raw, err := os.ReadFile("evidence/parity.json"); err == nil {
		if err := json.Unmarshal(raw, &prior); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.MarshalIndent(MergeEvidence(prior, run, readers), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("evidence/parity.json", append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readParity compares up to perType instances of r's type and returns e
// with the outcome. A difference or a failed direct read fails t; what
// only Cloud Control could not do is recorded as inconclusive instead.
func readParity(ctx context.Context, t *testing.T, cc *cloudcontrol.Client, client *Client, r Reader, e TypeEvidence, perType int) TypeEvidence {
	// A type with a direct list is listed through it: Cloud Control's list
	// omits its instances. Cloud Control still reads each one.
	var ids []string
	if HasList(r.Type) {
		var err error
		if ids, err = client.List(ctx, r.Type); err != nil {
			t.Errorf("listing directly: %v", err)
			e.Outcome, e.Note = "direct-unreadable", "the direct list failed"
			return e
		}
	} else {
		listed, err := cc.ListResources(ctx, &cloudcontrol.ListResourcesInput{TypeName: aws.String(r.Type), MaxResults: aws.Int32(int32(perType))})
		if err != nil {
			// The error code alone: a throttle and a refusal read
			// differently, and neither names an instance.
			code := "unknown"
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) {
				code = apiErr.ErrorCode()
			}
			e.Outcome, e.Note = "unlisted", "Cloud Control could not list the type: "+code
			return e
		}
		for _, d := range listed.ResourceDescriptions {
			ids = append(ids, aws.ToString(d.Identifier))
		}
	}
	if len(ids) > perType {
		ids = ids[:perType]
	}
	compared, differing, unreadable, directFailed := map[string]bool{}, map[string]bool{}, 0, 0
	skip := skipTreeFor(t, r.Type)
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
			// Printed to this run's log only, never to evidence.
			t.Errorf("direct read failed: %v", err)
			directFailed++
			continue
		}
		diffs, err := Compare(r.Type, viaCC, direct)
		if err != nil {
			t.Fatal(err)
		}
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
			t.Errorf("%s: cloudcontrol=%s direct=%s", diff.Property, diff.CloudControl, diff.Direct)
		}
		e.Instances++
	}
	e.Compared, e.Differing = sortedKeys(compared), sortedKeys(differing)
	switch {
	case directFailed > 0:
		e.Outcome = "direct-unreadable"
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
	if r.Probe != nil || len(r.AbsentIDs) > 0 {
		e = absenceParity(ctx, t, cc, client, r, e, perType)
	}
	return e
}

// absenceParity reads up to perType identifiers r's probe lists, each of
// which Cloud Control must read as absent, and checks the direct read does
// not find one present. An identifier Cloud Control finds proves nothing
// and is not counted.
func absenceParity(ctx context.Context, t *testing.T, cc *cloudcontrol.Client, client *Client, r Reader, e TypeEvidence, perType int) TypeEvidence {
	var ids []string
	if r.Probe != nil {
		probed, err := client.Probe(ctx, r.Type)
		if err != nil {
			t.Errorf("probing: %v", err)
			return e
		}
		ids = probed
	}
	for _, id := range r.AbsentIDs {
		ids = append(ids, strings.NewReplacer("{account}", account, "{region}", client.Region).Replace(id))
	}
	if len(ids) > perType {
		ids = ids[:perType]
	}
	present := 0
	for _, id := range ids {
		_, err := cc.GetResource(ctx, &cloudcontrol.GetResourceInput{TypeName: aws.String(r.Type), Identifier: aws.String(id)})
		var notFound *cctypes.ResourceNotFoundException
		if !errors.As(err, &notFound) {
			continue
		}
		e.Probed++
		if _, err := client.Read(ctx, r.Type, map[string]string{r.Identifier[0].Property: id}); err == nil {
			// Printed to this run's log only, never to evidence.
			t.Errorf("Cloud Control reads %s as absent, the direct read finds it", id)
			present++
		}
	}
	switch {
	case present > 0:
		e.Absence = "differs"
	case e.Probed > 0:
		e.Absence = "parity"
	}
	return e
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
