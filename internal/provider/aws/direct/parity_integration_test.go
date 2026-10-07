//go:build integration

package direct

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
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
	client := &Client{HTTP: &http.Client{Timeout: 30 * time.Second}, Credentials: cfg.Credentials, Region: region,
		Account: func(context.Context) (string, error) { return account, nil }}
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
		e := TypeEvidence{Type: r.Type, SmithyCommit: lock.SmithyCommit, Date: time.Now().UTC().Format("2006-01-02"), Region: region, Reader: ReaderHash(r)}
		observed := observing(client, r.UndeclaredReadErrors)
		t.Run(r.Type, func(t *testing.T) {
			e = readParity(ctx, t, cc, client, r, e, perType)
		})
		e.Observed = observed()
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

// harnessIDs are the identifiers of typeName's instances the environment
// names, for a type Cloud Control cannot list: KRAAI_PARITY_IDS_<TYPE>, the
// type uppercased with its colons as underscores (AWS_LAMBDA_URL), holds
// them comma-separated.
func harnessIDs(typeName string) []string {
	var ids []string
	for _, id := range strings.Split(os.Getenv("KRAAI_PARITY_IDS_"+strings.ToUpper(strings.ReplaceAll(typeName, "::", "_"))), ",") {
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// readParity compares up to perType instances of r's type and returns e
// with the outcome. A difference or a failed direct read fails t; what
// only Cloud Control could not do is recorded as inconclusive instead.
func readParity(ctx context.Context, t *testing.T, cc *cloudcontrol.Client, client *Client, r Reader, e TypeEvidence, perType int) TypeEvidence {
	var ids []string
	listed, err := cc.ListResources(ctx, &cloudcontrol.ListResourcesInput{TypeName: aws.String(r.Type), MaxResults: aws.Int32(int32(perType))})
	if err != nil {
		// A type Cloud Control lists only under a parent, such as a
		// function, is given its instances by the environment.
		if ids = harnessIDs(r.Type); len(ids) == 0 {
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
	} else {
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
		direct, err := client.ReadByID(ctx, r.Type, id)
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
	if len(r.AbsentIDs) > 0 {
		e = absenceParity(ctx, t, cc, client, r, e, perType)
	}
	return e
}

// absenceParity reads up to perType of r's absentIds, each of which Cloud
// Control must read as absent, and checks the direct read does not find
// one present.
func absenceParity(ctx context.Context, t *testing.T, cc *cloudcontrol.Client, client *Client, r Reader, e TypeEvidence, perType int) TypeEvidence {
	var ids []string
	for _, id := range r.AbsentIDs {
		// A variable the harness environment names, as a lifecycle vector
		// does, such as a route table the missing route is looked for in;
		// an identifier naming one that is unset is left out.
		unset := false
		id = vectorPlaceholder.ReplaceAllStringFunc(strings.NewReplacer("{account}", account, "{region}", client.Region).Replace(id), func(p string) string {
			value := os.Getenv(envName(p[1 : len(p)-1]))
			unset = unset || value == ""
			return value
		})
		if !unset {
			ids = append(ids, id)
		}
	}
	if len(ids) > perType {
		ids = ids[:perType]
	}
	present, failed := 0, 0
	for _, id := range ids {
		_, err := cc.GetResource(ctx, &cloudcontrol.GetResourceInput{TypeName: aws.String(r.Type), Identifier: aws.String(id)})
		var notFound *cctypes.ResourceNotFoundException
		if !errors.As(err, &notFound) {
			// A declared absent identifier Cloud Control does not read as
			// absent cannot prove anything, which the override must fix.
			t.Errorf("absentIds %s: Cloud Control answers %v, not NotFound", id, err)
			continue
		}
		e.Probed++
		// Printed to this run's log only, never to evidence.
		switch _, err := client.ReadByID(ctx, r.Type, id); {
		case err == nil:
			t.Errorf("Cloud Control reads %s as absent, the direct read finds it", id)
			present++
		case !errors.Is(err, ErrAbsent):
			t.Errorf("Cloud Control reads %s as absent, the direct read fails: %v", id, err)
			failed++
		}
	}
	switch {
	case present > 0:
		e.Absence = "differs"
	case failed > 0:
		e.Absence = "direct-unreadable"
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

// observing has c report each of want it observes, until the returned
// function stops it and returns them, sorted. Further calls run
// concurrently, so the codes are gathered under a lock.
func observing(c *Client, want []string) func() []string {
	var mu sync.Mutex
	seen := map[string]bool{}
	c.Observe = func(code string) {
		if slices.Contains(want, code) {
			mu.Lock()
			seen[code] = true
			mu.Unlock()
		}
	}
	return func() []string {
		c.Observe = nil
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			return nil
		}
		return sortedKeys(seen)
	}
}
