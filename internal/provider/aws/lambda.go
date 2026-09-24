package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
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

// resolveEnv builds the function's environment variables: settings.Env
// verbatim, plus settings.EnvSecrets resolved at the point of use — through
// client's own backends for a secret reference, through spec.Secret for a
// binding key, exactly as every envSecrets entry resolved before references
// existed. Beside a value this version-diff feature can track (a
// reference, or a binding key naming a manifest.CapabilitySecrets entry),
// it also writes that value's marker variable (see secretVersionMarkerName)
// carrying the store's own version, so a later plan can tell whether the
// value is still current without ever reading it.
//
// Resolved in sorted order by environment variable name, rather than Go's
// randomized map order, so a manifest with more than one entry behaves the
// same on every run: which one fails first, and which ones were already
// fetched before it did, is otherwise not reproducible.
func resolveEnv(ctx context.Context, client *Client, spec resource.Spec, settings LambdaSettings) (map[string]any, error) {
	env := make(map[string]any, len(settings.Env)+2*len(settings.EnvSecrets))
	for k, v := range settings.Env {
		env[k] = v
	}

	envVars := make([]string, 0, len(settings.EnvSecrets))
	for envVar := range settings.EnvSecrets {
		envVars = append(envVars, envVar)
	}
	sort.Strings(envVars)

	for _, envVar := range envVars {
		value, version, err := resolveEnvSecret(ctx, client, spec, settings.EnvSecrets[envVar])
		if err != nil {
			return nil, kerrors.Wrap(err, codeOf(err), "environment variable %q", envVar)
		}
		env[envVar] = value
		if version != "" {
			env[secretVersionMarkerName(envVar)] = version
		}
	}
	return env, nil
}

// codeOf returns err's kerrors.Code, or CodeUnexpected when it carries
// none, so adding context to a secret reference failure never downgrades a
// transient one (CodeUnexpected: throttled, network) into a validation one.
func codeOf(err error) kerrors.Code {
	var kerr *kerrors.KError
	if errors.As(err, &kerr) {
		return kerr.Code()
	}
	return kerrors.CodeUnexpected
}

// resolveEnvSecret resolves one envSecrets value to its live value and, for
// a value this version-diff feature can track, the store's own version for
// it: a reference (resolved through client, exactly as before this feature
// existed) always carries one; a binding key naming a
// manifest.CapabilitySecrets entry carries one too, fetched through a
// separate metadata-only call and, deliberately, before the value itself —
// see the ordering note below. Any other binding key (a database's
// connection_uri, e.g.) is resolved through spec.Secret exactly as every
// envSecrets entry resolved before this feature or references existed, and
// returns an empty version: out of scope, nothing to compare.
//
// The returned error, wrapped by resolveEnv with only the environment
// variable's name, keeps whatever kerrors.Code the failure already carries:
// a missing reference is a validation error, but a throttled or otherwise
// failed provider call is not, and neither this function nor its caller
// should downgrade the second into the first.
//
// Ordering for a binding key: the version is fetched before the value.
// Under a rotation racing this apply, that means the marker can only
// under-state freshness — a value written after a newer version replaced
// the one the marker names — never over-state it. An under-stated marker
// makes the next plan see it as stale and redeploy, wasteful but correct; an
// over-stated one would mark an already-stale value as current and strand
// it. A reference has no such ordering to get right: its version comes from
// the very same call that returned its value, so the two can never
// disagree.
func resolveEnvSecret(ctx context.Context, client *Client, spec resource.Spec, raw string) (value, version string, err error) {
	ref, isRef, err := secretref.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if isRef {
		return resolveVersionedURIRef(ctx, client, ref)
	}

	binding, entry, hasEntry := strings.Cut(raw, ".")
	if hasEntry {
		name, found, err := entryParameterName(spec, binding, entry)
		if err != nil {
			return "", "", err
		}
		if found {
			if version, err = client.currentSSMParameterVersion(ctx, name); err != nil {
				return "", "", err
			}
		}
	}
	value, err = spec.Secret(ctx, raw)
	return value, version, err
}

// addBindingEnv publishes what each of the service's AWS bindings resolved
// to, so the function can reach it by its binding's name: a queues binding
// JOBS becomes JOBS_QUEUE_URL and JOBS_QUEUE_ARN, an objects binding ASSETS
// becomes ASSETS_BUCKET_NAME. A binding another vendor fulfils publishes a
// credential, which envSecrets maps by hand. A variable the manifest already
// set is a conflict to report, not to resolve quietly.
func addBindingEnv(ctx context.Context, spec resource.Spec, env map[string]any) error {
	variables, err := bindingVariables(spec)
	if err != nil {
		return err
	}
	for _, v := range variables {
		if _, taken := env[v.name]; taken {
			return kerrors.Validation(
				"binding %q: environment variable %q is set by settings and by a binding; rename one",
				spec.Binding, v.name)
		}
		value, err := v.value(ctx, spec)
		if err != nil {
			return err
		}
		env[v.name] = value
	}
	return nil
}

// bindingVariable is one environment variable a binding gives the function:
// its name, known from the manifest alone, and its value, which may need
// what the binding's resource published and so is resolved only at apply.
type bindingVariable struct {
	name  string
	value func(ctx context.Context, spec resource.Spec) (any, error)
}

// bindingVariables lists the variables the service's AWS bindings give the
// function. Names come from the binding and its capability, so a plan can
// know which variables a function should carry without resolving any.
func bindingVariables(spec resource.Spec) ([]bindingVariable, error) {
	bindings, err := decodeServiceBindings(spec)
	if err != nil {
		return nil, err
	}
	var out []bindingVariable
	for _, b := range bindings {
		if b.Vendor != Provider {
			continue
		}
		prefix := b.envPrefix()
		if b.Capability == manifest.CapabilityAWS {
			variables, err := nativeVariables(spec, b, prefix)
			if err != nil {
				return nil, err
			}
			out = append(out, variables...)
			continue
		}
		switch b.Capability {
		case manifest.CapabilityQueues:
			queueKey := b.attributeKey(spec, TypeSQSQueue)
			out = append(out,
				bindingVariable{name: prefix + "_QUEUE_URL", value: func(_ context.Context, spec resource.Spec) (any, error) {
					return spec.Attribute(queueKey, "QueueUrl")
				}},
				bindingVariable{name: prefix + "_QUEUE_ARN", value: func(_ context.Context, spec resource.Spec) (any, error) {
					return spec.Attribute(queueKey, "Arn")
				}})
		case manifest.CapabilityObjects:
			// The bucket's name is its identity, derived rather than
			// published.
			out = append(out, bindingVariable{name: prefix + "_BUCKET_NAME", value: func(context.Context, resource.Spec) (any, error) {
				return b.Name, nil
			}})
		case manifest.CapabilityKeyValue:
			// The cache's endpoint exists only once created. Reachable only
			// from inside the cache's network, which vpcConfigFor places the
			// function in.
			if driver, _ := b.Config["driver"].(string); driver != DriverRedis {
				continue
			}
			cacheKey := b.attributeKey(spec, TypeElastiCacheServerlessCache)
			out = append(out, bindingVariable{name: prefix + "_REDIS_URL", value: func(_ context.Context, spec resource.Spec) (any, error) {
				return cacheURL(spec, cacheKey)
			}})
		case manifest.CapabilityDatabase:
			switch driver, _ := b.Config["driver"].(string); driver {
			case DriverDynamoDB:
				out = append(out, bindingVariable{name: prefix + "_TABLE_NAME", value: func(context.Context, resource.Spec) (any, error) {
					return b.Name, nil
				}})
			case DriverPostgres:
				if engine, _ := b.Config["engine"].(string); engine == engineAurora {
					// The cluster's URL carries its master password, so it
					// is a credential, produced by aurora.go and resolved
					// here at the moment of use.
					secretKey := b.Binding + "." + SecretConnectionURI
					out = append(out, bindingVariable{name: prefix + "_DATABASE_URL", value: func(ctx context.Context, spec resource.Spec) (any, error) {
						return spec.Secret(ctx, secretKey)
					}})
					continue
				}
				// The DSQL URL carries no password: the function signs an
				// IAM token when it connects.
				clusterKey := b.attributeKey(spec, TypeDSQLCluster)
				out = append(out, bindingVariable{name: prefix + "_DATABASE_URL", value: func(_ context.Context, spec resource.Spec) (any, error) {
					return dsqlURL(spec, clusterKey)
				}})
			}
		}
	}
	return out, nil
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

// environmentMatches reports whether the live function's environment is the
// one the spec would produce, as far as a plan can tell: every literal
// variable carries its value; every secret or binding variable exists; a
// secret-backed variable this feature tracks a version for also carries a
// marker whose value matches the store's current one (expectedSecretVersion
// — the one live call this makes, metadata only, never a decrypt); and
// nothing else is present.
func environmentMatches(ctx context.Context, client *Client, spec resource.Spec, settings LambdaSettings, state *resource.State) (bool, error) {
	environment, _ := state.Attributes["Environment"].(map[string]any)
	live, _ := environment["Variables"].(map[string]any)

	expected := make(map[string]bool, len(settings.Env)+2*len(settings.EnvSecrets))
	for name, want := range settings.Env {
		expected[name] = true
		if got, _ := live[name].(string); got != want {
			return false, nil
		}
	}
	for envVar, raw := range settings.EnvSecrets {
		expected[envVar] = true
		version, found, err := expectedSecretVersion(ctx, client, spec, raw)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		marker := secretVersionMarkerName(envVar)
		expected[marker] = true
		if got, _ := live[marker].(string); got != version {
			return false, nil
		}
	}
	variables, err := bindingVariables(spec)
	if err != nil {
		return false, err
	}
	for _, v := range variables {
		expected[v.name] = true
	}
	for name := range expected {
		if _, present := live[name]; !present {
			return false, nil
		}
	}
	for name := range live {
		if !expected[name] {
			return false, nil
		}
	}
	return true, nil
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

// nativeVariables are the environment variables a native binding gives the
// function: <BINDING>_<PROPERTY> for each property the type publishes, its
// name for a type the name identifies and every property the vendor
// assigns. A string is passed as it is, a number or boolean formatted, and
// anything else as JSON.
func nativeVariables(spec resource.Spec, b serviceBinding, prefix string) ([]bindingVariable, error) {
	typeName, _ := b.Config[nativeTypeKey].(string)
	facts, err := cfschema.Lookup(typeName)
	if err != nil {
		return nil, err
	}
	attributes := b.attributeKey(spec, resource.RoleType(typeName, nativeRole))
	var out []bindingVariable
	for _, property := range publishedProperties(facts) {
		out = append(out, bindingVariable{
			name: prefix + "_" + envName(property),
			value: func(_ context.Context, spec resource.Spec) (any, error) {
				value, ok := spec.Attributes[attributes][property]
				if !ok {
					return nil, kerrors.Validation("binding %q: %s published no %s", spec.Binding, b.Binding, property)
				}
				switch v := value.(type) {
				case string:
					return v, nil
				case bool, int, int64, float64:
					return fmt.Sprint(v), nil
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding %s of %s", property, b.Binding)
				}
				return string(encoded), nil
			},
		})
	}
	return out, nil
}

// envName spells a property name as an environment variable name: a word
// break before each capital that starts a word, acronyms kept whole, so
// QueueUrl is QUEUE_URL and DBClusterArn is DB_CLUSTER_ARN.
func envName(property string) string {
	runes := []rune(property)
	var sb strings.Builder
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prevLower := unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || (unicode.IsUpper(runes[i-1]) && nextLower) {
				sb.WriteByte('_')
			}
		}
		sb.WriteRune(unicode.ToUpper(r))
	}
	return sb.String()
}
