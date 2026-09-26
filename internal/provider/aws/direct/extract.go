//go:build ignore

// extract fetches, at a pinned commit, the Smithy model each override reads
// through and the CloudFormation schema of each override's type, keeps from
// each model only the shapes its chosen operations reach, and writes the
// checked-in subset and lock.json. Needs the network and, for the schemas,
// AWS credentials; everything downstream of it needs neither.
//
// A schema already recorded in lock.json for a type an override still names
// is kept as locked rather than re-fetched from the live CloudFormation
// registry; -refresh re-fetches every schema regardless. Every write lands in
// a temporary directory first and is swapped into place only once every
// fetch has succeeded, so a failure partway through leaves schemas/, models/
// and lock.json exactly as they were.
//
//	go run extract.go [-commit SHA] [-region us-east-1] [-models DIR] [-schemas DIR] [-refresh]
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"go.yaml.in/yaml/v3"
)

type override struct {
	Type string `yaml:"type"`
	Read struct {
		Model     string `yaml:"model"`
		Operation string `yaml:"operation"`
	} `yaml:"read"`
	List *struct {
		Operation string `yaml:"operation"`
	} `yaml:"list"`
	Probe *struct {
		Operation string `yaml:"operation"`
	} `yaml:"probe"`
	Also []struct {
		Operation string `yaml:"operation"`
	} `yaml:"also"`
}

type lockedFile struct {
	File   string `json:"file"`
	Source string `json:"sourceSha256"`
	Subset string `json:"subsetSha256"`
}

type lock struct {
	SmithyCommit string                `json:"smithyCommit"`
	Models       map[string]lockedFile `json:"models"`
	Schemas      map[string]lockedFile `json:"schemas"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "extract:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		commit  = flag.String("commit", "", "api-models-aws commit to read; defaults to the one lock.json records")
		region  = flag.String("region", "us-east-1", "region whose CloudFormation registry to read")
		local   = flag.String("models", "", "the models/ directory of an api-models-aws checkout at the commit, read instead of fetching")
		cached  = flag.String("schemas", "", "a directory of raw CloudFormation schemas, read instead of calling DescribeType; the one join_main read")
		refresh = flag.Bool("refresh", false, "re-fetch every schema from the live CloudFormation registry, ignoring what lock.json already has for it")
	)
	flag.Parse()

	var old lock
	if raw, err := os.ReadFile("lock.json"); err == nil {
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
	}
	if *commit == "" {
		if old.SmithyCommit == "" {
			return fmt.Errorf("no -commit given and lock.json records none")
		}
		*commit = old.SmithyCommit
	}

	overrides, err := readOverrides()
	if err != nil {
		return err
	}
	ops := map[string][]string{}
	for _, o := range overrides {
		ops[o.Read.Model] = append(ops[o.Read.Model], o.Read.Operation)
		if o.List != nil {
			ops[o.Read.Model] = append(ops[o.Read.Model], o.List.Operation)
		}
		if o.Probe != nil {
			ops[o.Read.Model] = append(ops[o.Read.Model], o.Probe.Operation)
		}
		for _, also := range o.Also {
			ops[o.Read.Model] = append(ops[o.Read.Model], also.Operation)
		}
	}

	// Every fetch below writes into tmpRoot. The real models/, schemas/ and
	// lock.json are only touched once every fetch has succeeded, so a
	// failure partway through never leaves them half-written. tmpRoot is
	// always removed on return: on success its contents have already been
	// moved out, and on failure it still holds the discarded partial output.
	tmpRoot, err := os.MkdirTemp(".", ".extract-tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpRoot) }()

	tmpModels := filepath.Join(tmpRoot, "models")
	tmpSchemas := filepath.Join(tmpRoot, "schemas")
	if err := os.MkdirAll(tmpModels, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(tmpSchemas, 0o755); err != nil {
		return err
	}

	out := lock{SmithyCommit: *commit, Models: map[string]lockedFile{}, Schemas: map[string]lockedFile{}}

	models := make([]string, 0, len(ops))
	for m := range ops {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, model := range models {
		var raw []byte
		if *local != "" {
			raw, err = os.ReadFile(filepath.Join(*local, filepath.FromSlash(model)))
		} else {
			raw, err = fetch(fmt.Sprintf("https://raw.githubusercontent.com/aws/api-models-aws/%s/models/%s", *commit, model))
		}
		if err != nil {
			return err
		}
		subset, err := subsetModel(raw, ops[model])
		if err != nil {
			return fmt.Errorf("%s: %w", model, err)
		}
		file := "models/" + path.Base(model)
		if err := os.WriteFile(filepath.Join(tmpModels, path.Base(model)), subset, 0o644); err != nil {
			return err
		}
		out.Models[model] = lockedFile{File: file, Source: sum(raw), Subset: sum(subset)}
	}

	ctx := context.Background()
	var cf *cloudformation.Client
	client := func() (*cloudformation.Client, error) {
		if cf == nil {
			cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region))
			if err != nil {
				return nil, err
			}
			cf = cloudformation.NewFromConfig(cfg)
		}
		return cf, nil
	}
	for _, o := range overrides {
		base := strings.ReplaceAll(o.Type, "::", "--") + ".json"
		file := "schemas/" + base
		tmpFile := filepath.Join(tmpSchemas, base)

		if !*refresh {
			if entry, ok := old.Schemas[o.Type]; ok {
				if existing, err := os.ReadFile(file); err == nil {
					if err := os.WriteFile(tmpFile, existing, 0o644); err != nil {
						return err
					}
					out.Schemas[o.Type] = entry
					continue
				}
			}
		}

		var raw []byte
		if *cached != "" {
			if raw, err = os.ReadFile(filepath.Join(*cached, base)); err != nil {
				return err
			}
		} else {
			cf, err := client()
			if err != nil {
				return err
			}
			described, err := cf.DescribeType(ctx, &cloudformation.DescribeTypeInput{
				Type: cftypes.RegistryTypeResource, TypeName: aws.String(o.Type),
			})
			if err != nil {
				return fmt.Errorf("%s: %w", o.Type, err)
			}
			if described.Schema == nil {
				return fmt.Errorf("%s: DescribeType returned no schema", o.Type)
			}
			raw = []byte(*described.Schema)
		}
		pretty, err := canonical(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", o.Type, err)
		}
		if err := os.WriteFile(tmpFile, pretty, 0o644); err != nil {
			return err
		}
		out.Schemas[o.Type] = lockedFile{File: file, Source: sum(raw), Subset: sum(pretty)}
	}

	lockJSON, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpRoot, "lock.json"), append(lockJSON, '\n'), 0o644); err != nil {
		return err
	}

	for _, dir := range [][2]string{{tmpModels, "models"}, {tmpSchemas, "schemas"}} {
		if err := replaceDir(dir[0], dir[1], tmpRoot); err != nil {
			return err
		}
	}
	return os.Rename(filepath.Join(tmpRoot, "lock.json"), "lock.json")
}

func readOverrides() ([]override, error) {
	names, err := filepath.Glob("overrides/*.yaml")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out []override
	for _, name := range names {
		raw, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var o override
		if err := yaml.Unmarshal(raw, &o); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, o)
	}
	return out, nil
}

func fetch(url string) ([]byte, error) {
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// subsetModel keeps the service shape, trimmed to the chosen operations, and
// every shape those operations reach through their input, output and errors.
func subsetModel(raw []byte, operations []string) ([]byte, error) {
	var model map[string]any
	if err := json.Unmarshal(raw, &model); err != nil {
		return nil, err
	}
	shapes, _ := model["shapes"].(map[string]any)
	var service string
	for id, s := range shapes {
		if shape, _ := s.(map[string]any); shape["type"] == "service" {
			service = id
		}
	}
	if service == "" {
		return nil, fmt.Errorf("no service shape")
	}
	namespace := service[:strings.Index(service, "#")+1]

	keep := map[string]any{}
	var visit func(id string)
	visit = func(id string) {
		if strings.HasPrefix(id, "smithy.api#") {
			return
		}
		if _, done := keep[id]; done {
			return
		}
		s, ok := shapes[id].(map[string]any)
		if !ok {
			return
		}
		keep[id] = stripDocs(s)
		for _, ref := range targets(s) {
			visit(ref)
		}
	}
	var opIDs []any
	seen := map[string]bool{}
	for _, op := range operations {
		if seen[op] {
			continue
		}
		seen[op] = true
		id := namespace + op
		if _, ok := shapes[id]; !ok {
			return nil, fmt.Errorf("no operation %s", id)
		}
		visit(id)
		opIDs = append(opIDs, map[string]any{"target": id})
	}
	sort.Slice(opIDs, func(i, j int) bool {
		return opIDs[i].(map[string]any)["target"].(string) < opIDs[j].(map[string]any)["target"].(string)
	})
	svc := map[string]any{}
	for k, v := range stripDocs(shapes[service].(map[string]any)) {
		if k != "operations" && k != "resources" {
			svc[k] = v
		}
	}
	// Endpoint tests, the decision-diagram form of the rules and IAM
	// condition keys are large and read by nothing generated from the
	// subset; the rule set stays, for a model with no endpoint prefix.
	if traits, ok := svc["traits"].(map[string]any); ok {
		for _, t := range []string{"smithy.rules#endpointTests", "smithy.rules#endpointBdd", "aws.iam#defineConditionKeys"} {
			delete(traits, t)
		}
	}
	svc["operations"] = opIDs
	keep[service] = svc

	subset := map[string]any{"smithy": model["smithy"], "shapes": keep}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(subset); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// stripDocs drops documentation and example traits from s and its members:
// prose nothing generated from the subset reads.
func stripDocs(s map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range s {
		switch k {
		case "traits":
			out[k] = withoutDocs(v)
		case "members":
			members := map[string]any{}
			for name, m := range v.(map[string]any) {
				member := map[string]any{}
				for mk, mv := range m.(map[string]any) {
					if mk == "traits" {
						mv = withoutDocs(mv)
					}
					member[mk] = mv
				}
				members[name] = member
			}
			out[k] = members
		default:
			out[k] = v
		}
	}
	return out
}

func withoutDocs(traits any) any {
	t, ok := traits.(map[string]any)
	if !ok {
		return traits
	}
	out := map[string]any{}
	for k, v := range t {
		if k != "smithy.api#documentation" && k != "smithy.api#examples" {
			out[k] = v
		}
	}
	return out
}

// targets are the shape ids s refers to.
func targets(s map[string]any) []string {
	var out []string
	add := func(v any) {
		if m, ok := v.(map[string]any); ok {
			if t, ok := m["target"].(string); ok {
				out = append(out, t)
			}
		}
	}
	for _, key := range []string{"input", "output", "member", "key", "value"} {
		add(s[key])
	}
	if members, ok := s["members"].(map[string]any); ok {
		for _, m := range members {
			add(m)
		}
	}
	if errs, ok := s["errors"].([]any); ok {
		for _, e := range errs {
			add(e)
		}
	}
	return out
}

// canonical re-encodes JSON with sorted keys and two-space indentation.
func canonical(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// replaceDir moves next into place as dir, moving the old dir aside under
// tmpRoot first, so a failed rename restores it rather than losing both.
func replaceDir(next, dir, tmpRoot string) error {
	old := filepath.Join(tmpRoot, "old-"+dir)
	if err := os.Rename(dir, old); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(next, dir); err != nil {
		_ = os.Rename(old, dir)
		return err
	}
	return nil
}
