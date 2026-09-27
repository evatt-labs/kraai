//go:build integration

package direct

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

var updateLifecycle = flag.Bool("update-lifecycle", false, "merge this run into evidence/lifecycle.json")

// Lifecycle parity, which creates and deletes real instances: each type
// with a lifecycle vector is created through its direct mutations, updated
// one property at a time and deleted, and after each step read back through
// Cloud Control. Runs only with KRAAI_ALLOW_MUTATE=1; every instance is
// named kraai-lifecycle-* and deleted when the test ends, however it ends.
//
// A vector property naming {kmsKeyArn} runs only when
// KRAAI_LIFECYCLE_KMS_KEY_ARN names a key the service may use, as a key
// bills while it exists.
//
//	KRAAI_ALLOW_MUTATE=1 go test -tags integration ./internal/provider/aws/direct -run TestLifecycleParity [-args -update-lifecycle]
func TestLifecycleParity(t *testing.T) {
	if os.Getenv("KRAAI_ALLOW_MUTATE") != "1" {
		t.Skip("creates and deletes real resources; set KRAAI_ALLOW_MUTATE=1")
	}
	const region = "us-east-1"
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
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	var run LifecycleEvidence
	for _, o := range all {
		if o.Lifecycle == nil {
			continue
		}
		hash, err := OverrideHash(o.Type)
		if err != nil {
			t.Fatal(err)
		}
		e := TypeLifecycle{Type: o.Type, Date: time.Now().UTC().Format("2006-01-02"), Override: hash, Outcome: "parity"}
		ran := false
		t.Run(o.Type, func(t *testing.T) {
			ran = true
			e = lifecycle(ctx, t, cc, client, o, e)
		})
		// A type -run leaves out is not evidence either way.
		if !ran {
			continue
		}
		t.Logf("%s: lifecycle %s, updated %v", o.Type, e.Outcome, e.Updated)
		run.Types = append(run.Types, e)
	}
	if !*updateLifecycle {
		return
	}
	var prior LifecycleEvidence
	if raw, err := os.ReadFile("evidence/lifecycle.json"); err == nil {
		if err := json.Unmarshal(raw, &prior); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.MarshalIndent(MergeLifecycle(prior, run), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("evidence/lifecycle.json", append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func lifecycle(ctx context.Context, t *testing.T, cc *cloudcontrol.Client, client *Client, o Override, e TypeLifecycle) TypeLifecycle {
	name := fmt.Sprintf("kraai-lifecycle-%s-%d", strings.ToLower(o.Type[strings.LastIndex(o.Type, ":")+1:]), time.Now().Unix())
	nameTag := map[string]any{"Key": "kraai:resource-name", "Value": name}
	// A vector may name the instance, {name}, where, {account} and
	// {region}, and a key made for the harness, {kmsKeyArn}.
	kmsKey := os.Getenv("KRAAI_LIFECYCLE_KMS_KEY_ARN")
	vars := strings.NewReplacer("{name}", name, "{account}", account, "{region}", client.Region, "{kmsKeyArn}", kmsKey)
	desired := fill(o.Lifecycle.Create, vars).(map[string]any)
	desired["Tags"] = append([]any{nameTag}, asList(desired["Tags"])...)

	id, err := client.Create(ctx, o.Type, desired)
	deleted := false
	// A create that fails after the instance exists still returns its
	// identifier, and the instance is deleted.
	if id != "" {
		t.Cleanup(func() {
			if !deleted {
				if err := client.Delete(context.Background(), o.Type, id); err != nil {
					t.Errorf("cleanup delete of %s: %v", id, err)
				}
			}
		})
	}
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fail := func(step string, err error) {
		t.Errorf("%s: %v", step, err)
		e.Outcome = "differs"
	}
	if err := ccShows(ctx, cc, o.Type, id, desired); err != nil {
		fail("create read through Cloud Control", err)
	}
	for _, property := range sortedKeys(o.Lifecycle.Update) {
		if kmsKey == "" && strings.Contains(fmt.Sprint(o.Lifecycle.Update[property]), "{kmsKeyArn}") {
			t.Logf("%s: skipped, KRAAI_LIFECYCLE_KMS_KEY_ARN is unset", property)
			continue
		}
		value := fill(o.Lifecycle.Update[property], vars)
		if property == "Tags" {
			value = append([]any{nameTag}, asList(value)...)
		}
		current, err := client.ReadByID(ctx, o.Type, id)
		if err != nil {
			fail("read before updating "+property, err)
			continue
		}
		changes := map[string]any{property: value}
		if err := client.Update(ctx, o.Type, id, current, changes); err != nil {
			fail("update "+property, err)
			continue
		}
		if err := ccShows(ctx, cc, o.Type, id, changes); err != nil {
			fail("update "+property+" read through Cloud Control", err)
			continue
		}
		e.Updated = append(e.Updated, property)
	}
	if err := client.Delete(ctx, o.Type, id); err != nil {
		fail("delete", err)
		return e
	}
	deleted = true
	if err := ccAbsent(ctx, cc, o.Type, id); err != nil {
		fail("delete read through Cloud Control", err)
	}
	sort.Strings(e.Updated)
	return e
}

// ccShows polls Cloud Control's read of id until it covers want.
func ccShows(ctx context.Context, cc *cloudcontrol.Client, typeName, id string, want map[string]any) error {
	var last map[string]any
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		out, err := cc.GetResource(ctx, &cloudcontrol.GetResourceInput{TypeName: aws.String(typeName), Identifier: aws.String(id)})
		if err != nil {
			continue
		}
		last = nil
		if json.Unmarshal([]byte(aws.ToString(out.ResourceDescription.Properties)), &last) == nil && covers(want, last) {
			return nil
		}
	}
	return fmt.Errorf("Cloud Control's read never showed %v; last %v", want, last)
}

// ccAbsent polls Cloud Control until it reads id as absent.
func ccAbsent(ctx context.Context, cc *cloudcontrol.Client, typeName, id string) error {
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		_, err := cc.GetResource(ctx, &cloudcontrol.GetResourceInput{TypeName: aws.String(typeName), Identifier: aws.String(id)})
		var notFound *cctypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return nil
		}
	}
	return errors.New("Cloud Control still reads it")
}

// fill replaces the variables in every string of v, in a copy.
func fill(v any, vars *strings.Replacer) any {
	switch t := v.(type) {
	case string:
		return vars.Replace(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = fill(item, vars)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = fill(item, vars)
		}
		return out
	}
	return v
}
