package aws

import (
	"context"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
	"github.com/evatt-labs/kraai/internal/secretref"
)

// artifactObjectKey derives the S3 key a service's packaged artifact is
// stored under: {serviceName}/{sha256}.zip. Content-addressed, so identical
// source produces the same key and no upload, and any change a new key.
func artifactObjectKey(serviceName, sha256Hex string) string {
	return serviceName + "/" + sha256Hex + ".zip"
}

// lambdaFunctionResource provisions a service's Lambda function: packaging
// its deployment artifact, uploading it to the per-service artifact bucket,
// and wiring its execution role, environment and layer before delegating to
// the generic engine.
//
// Packaging and upload are split: building the zip is local file I/O and
// safe during a plan, where Diff hashes it to compare against the last
// deploy; uploading is a mutation and happens only inside translate, which
// only Create and Update reach.
type lambdaFunctionResource struct {
	*resourceType
	client *Client
}

func newLambdaFunctionResource(client *Client) *lambdaFunctionResource {
	l := &lambdaFunctionResource{
		resourceType: &resourceType{provider: Provider, typeName: TypeLambdaFunction, lookup: resource.LookupByName, client: client},
		client:       client,
	}
	l.resourceType.translate = l.translate
	return l
}

// translate packages the service's artifact, uploads it, resolves its
// execution role's ARN and its environment, and builds the real
// AWS::Lambda::Function desired state. Reached only from Create and Update.
func (l *lambdaFunctionResource) translate(ctx context.Context, spec resource.Spec) (resource.Spec, error) {
	declared, err := declaredFunction(spec)
	if err != nil {
		return resource.Spec{}, err
	}

	data, sha256Hex, err := buildArtifact(declared.dir, declared.include)
	if err != nil {
		return resource.Spec{}, err
	}
	bucket := artifactBucketName(spec.Name)
	key := artifactObjectKey(spec.Name, sha256Hex)
	if err := l.client.PutObject(ctx, bucket, key, data); err != nil {
		return resource.Spec{}, err
	}

	account, err := l.client.AccountID(ctx)
	if err != nil {
		return resource.Spec{}, err
	}
	// The role's name is spec.Name, the derived name every compute
	// registration for the service shares, so its ARN is built locally.
	execRoleARN := roleARN(account, spec.Name)

	env, err := resolveEnv(ctx, l.client, spec, declared.settings)
	if err != nil {
		return resource.Spec{}, err
	}
	if err := addBindingEnv(ctx, spec, env); err != nil {
		return resource.Spec{}, err
	}

	translated := spec
	translated.Config = declared.properties
	translated.Config["Code"] = map[string]any{
		"S3Bucket": bucket,
		"S3Key":    key,
	}
	translated.Config["Role"] = execRoleARN
	translated.Config["Environment"] = map[string]any{"Variables": env}
	// Code is write-only: a read never returns which artifact a function
	// runs, so the artifact's hash is recorded where a read does return
	// it, for Diff to compare against the source on the next plan.
	translated.Config["Tags"] = []any{map[string]any{"Key": artifactTagKey, "Value": sha256Hex}}

	vpcConfig, err := vpcConfigFor(spec)
	if err != nil {
		return resource.Spec{}, err
	}
	if vpcConfig != nil {
		translated.Config["VpcConfig"] = vpcConfig
	}
	return translated, nil
}

// artifactTagKey is the tag a function carries naming the SHA-256 of the
// deployment package it runs.
const artifactTagKey = "kraai:artifact-sha256"

// declaredFunctionProperties is what the manifest says about a function
// before anything is packaged, uploaded or resolved: the part of its
// desired state a plan can compute with no attributes, secrets or network.
type declaredFunctionProperties struct {
	dir      string
	include  []string
	settings LambdaSettings
	// properties are the AWS::Lambda::Function properties that follow from
	// the manifest alone: name, handler, runtime, architecture, sizing,
	// layer and reserved concurrency.
	properties map[string]any
}

// declaredFunction reads the manifest-declared part of a function out of
// its spec, or fails naming what is missing.
func declaredFunction(spec resource.Spec) (declaredFunctionProperties, error) {
	dir, _ := spec.Config["dir"].(string)
	if dir == "" {
		return declaredFunctionProperties{}, kerrors.Validation("binding %q declares no dir to package", spec.Binding)
	}
	handler, _ := spec.Config["handler"].(string)
	if handler == "" {
		return declaredFunctionProperties{}, kerrors.Validation("binding %q declares no compute.handler", spec.Binding)
	}

	settingsMap, _ := spec.Config["settings"].(map[string]any)
	lambdaSettings, err := decodeLambdaSettings(settingsMap)
	if err != nil {
		return declaredFunctionProperties{}, err
	}

	// A typed []string straight from manifest.Compute.Include, set only
	// when the entry declares one.
	include, _ := spec.Config["include"].([]string)

	properties := map[string]any{
		"FunctionName":  spec.Name,
		"PackageType":   "Zip",
		"Handler":       handler,
		"Runtime":       lambdaSettings.Runtime,
		"Architectures": []any{lambdaSettings.Architecture},
		"MemorySize":    lambdaSettings.MemorySize,
		"Timeout":       lambdaSettings.Timeout,
	}
	// Layers only when configured: Layers: [""] is an invalid ARN.
	if lambdaSettings.LayerArn != "" {
		properties["Layers"] = []any{lambdaSettings.LayerArn}
	}
	// Only when declared: nil means no opinion, not zero.
	if lambdaSettings.ReservedConcurrentExecutions != nil {
		properties["ReservedConcurrentExecutions"] = *lambdaSettings.ReservedConcurrentExecutions
	}
	return declaredFunctionProperties{dir: dir, include: include, settings: lambdaSettings, properties: properties}, nil
}

// serviceNetwork returns the one network binding this provider fulfils on
// the service, or none. A function runs inside at most one VPC, so a second
// network binding is refused by name rather than one winning quietly.
func serviceNetwork(spec resource.Spec) (*serviceBinding, error) {
	bindings, err := decodeServiceBindings(spec)
	if err != nil {
		return nil, err
	}
	var network *serviceBinding
	for i := range bindings {
		b := bindings[i]
		if b.Capability != manifest.CapabilityNetwork || b.Vendor != Provider {
			continue
		}
		if network != nil {
			return nil, kerrors.Validation(
				"binding %q: the service declares network bindings %q and %q, and a function runs inside one VPC",
				spec.Binding, network.Binding, b.Binding)
		}
		network = &b
	}
	return network, nil
}

// vpcConfigFor places the function inside the service's network binding,
// when it declares one: both subnets of a tier, one per zone, and the VPC's
// default security group, which allows every outbound connection. The
// private tier when the network has one, since that is the tier with a NAT
// route to the internet; the public tier otherwise, where the function
// reaches the VPC and its gateway endpoints and nothing beyond. Read from
// what the VPC and subnets published, which this type has because it reads
// every binding on its service.
func vpcConfigFor(spec resource.Spec) (map[string]any, error) {
	network, err := serviceNetwork(spec)
	if err != nil || network == nil {
		return nil, err
	}
	var subnetIDs []any
	for _, subnetKey := range tierSubnetKeys(hasPrivateSubnet(network.Config)) {
		subnetID, err := spec.Attribute(network.Binding+"."+subnetKey, "SubnetId")
		if network.Binding == spec.Binding {
			subnetID, err = spec.Attribute(subnetKey, "SubnetId")
		}
		if err != nil {
			return nil, err
		}
		subnetIDs = append(subnetIDs, subnetID)
	}
	groupID, err := spec.Attribute(network.attributeKey(spec, TypeVPC), "DefaultSecurityGroup")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"SubnetIds":        subnetIDs,
		"SecurityGroupIds": []any{groupID},
	}, nil
}

// DiffLive decides whether an existing function needs a redeploy from what
// a plan can know: the manifest, the source on disk, the live function, and
// — the one live call this type's comparison makes — the secret store's
// own metadata for whichever secret-backed environment variables the
// function declares. Compared in order: the declared properties through
// the generic comparison (FunctionName is createOnly, so a difference there
// is a replace); the artifact, by hashing the source and reading the hash
// the last deploy recorded in the tags; the environment, where a literal
// variable must match, a secret or binding variable must exist, and a
// secret-backed one's version marker must match the store's current
// version (environmentMatches); and whether the function is inside a VPC.
//
// Implements plan.LiveDiffer rather than plan.Differ: environmentMatches'
// version check needs a call of its own beside spec and state, which Differ
// has no context for. That call reads metadata only — DescribeParameters
// or DescribeSecret, never GetParameter or GetSecretValue — the same
// contract every other verb in this package that touches a secret honors:
// a plan never resolves a live credential.
func (l *lambdaFunctionResource) DiffLive(ctx context.Context, spec resource.Spec, state *resource.State) (resource.Difference, error) {
	if state == nil {
		return resource.Same, nil
	}
	declared, err := declaredFunction(spec)
	if err != nil {
		return resource.Same, err
	}
	comparable := spec
	comparable.Config = declared.properties
	difference, err := l.compare(comparable, state)
	if err != nil || difference != resource.Same {
		return difference, err
	}

	_, sha256Hex, err := buildArtifact(declared.dir, declared.include)
	if err != nil {
		return resource.Same, err
	}
	if tagValue(state.Attributes, artifactTagKey) != sha256Hex {
		return resource.Mutable, nil
	}

	same, err := environmentMatches(ctx, l.client, spec, declared.settings, state)
	if err != nil || !same {
		return resource.Mutable, err
	}

	network, err := serviceNetwork(spec)
	if err != nil {
		return resource.Same, err
	}
	if (network != nil) != insideVPC(state) {
		return resource.Mutable, nil
	}
	return resource.Same, nil
}

// tagValue reads one tag's value out of a live state's array-shaped Tags,
// empty when the tag is absent.
func tagValue(properties map[string]any, key string) string {
	tags, _ := properties["Tags"].([]any)
	for _, t := range tags {
		tag, _ := t.(map[string]any)
		if k, _ := tag["Key"].(string); k == key {
			value, _ := tag["Value"].(string)
			return value
		}
	}
	return ""
}

// insideVPC reports whether a live function is attached to a VPC. Lambda
// reports a detached function either without VpcConfig or with an empty
// subnet list.
func insideVPC(state *resource.State) bool {
	vpcConfig, _ := state.Attributes["VpcConfig"].(map[string]any)
	subnets, _ := vpcConfig["SubnetIds"].([]any)
	return len(subnets) > 0
}

// ValidateSpec implements plan.SpecValidator with the pure validation of
// the merged settings, so a typo or an invalid value fails a fresh
// environment's first plan, where Diff is never reached.
func (l *lambdaFunctionResource) ValidateSpec(spec resource.Spec) error {
	settingsMap, _ := spec.Config["settings"].(map[string]any)
	_, err := decodeLambdaSettings(settingsMap)
	return err
}

// SecretRefs implements resource.SecretRefResolver: every secret reference
// this function's envSecrets declares, sorted by environment variable name
// for a deterministic order. A binding key such as "DB.connection_uri" is
// not a reference and is not returned.
func (l *lambdaFunctionResource) SecretRefs(spec resource.Spec) ([]secretref.Ref, error) {
	settingsMap, _ := spec.Config["settings"].(map[string]any)
	settings, err := decodeLambdaSettings(settingsMap)
	if err != nil {
		return nil, err
	}
	envVars := make([]string, 0, len(settings.EnvSecrets))
	for envVar := range settings.EnvSecrets {
		envVars = append(envVars, envVar)
	}
	sort.Strings(envVars)

	// The error return here is unreachable in normal operation:
	// decodeLambdaSettings above already ran validateSecretRef over every
	// value in settings.EnvSecrets, which itself calls secretref.Parse
	// first, so decode would have already failed on anything this second
	// parse could reject. Kept anyway as defense in depth against the two
	// checks drifting apart, the same reasoning resolveSecretRef's own
	// default case documents.
	var refs []secretref.Ref
	for _, envVar := range envVars {
		ref, isRef, err := secretref.Parse(settings.EnvSecrets[envVar])
		if err != nil {
			return nil, err
		}
		if isRef {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// ResolveSecretRef implements resource.SecretRefResolver, dispatching to
// this function's own client.
func (l *lambdaFunctionResource) ResolveSecretRef(_ context.Context, ref secretref.Ref) (resource.Secret, error) {
	return l.client.resolveSecretRef(ref)
}
