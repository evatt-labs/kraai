package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// artifactObjectKey derives the deterministic S3 key a service's packaged
// artifact is stored under: {serviceName}/{sha256}.zip. Content-addressed
// rather than a fixed "latest.zip"-style key so two applies of identical
// source produce the identical key — the same object, no upload, no Code
// diff — while any real code change produces a new key and, downstream, a
// visible Lambda code update.
func artifactObjectKey(serviceName, sha256Hex string) string {
	return serviceName + "/" + sha256Hex + ".zip"
}

// lambdaFunctionResource provisions a service's Lambda function: packaging
// its deployment artifact, uploading it to the per-service artifact bucket,
// and wiring its execution role, environment and Web Adapter layer before
// delegating to the generic Cloud Control engine.
//
// # Why packaging lives in a wrapper, not the generic engine
//
// resourceType (resource.go) submits Spec.Config as Cloud Control's desired
// state directly — correct for a binding capability's resources, where
// internal/plan's expandBinding already builds a type-appropriate Config
// per registration. expandCompute (out of scope, unchanged by this
// workstream) builds one generic Config per service — {dir, settings,
// trigger, handler, schedule} — shared across every Tier 1/2 compute
// registration for that service, not a Lambda::Function property map. This
// type (and every other Tier 2 type in this package) translates that
// generic shape into its own real Cloud Control properties before
// delegating; "no new engine work" (the brief's own scope line) means
// resource.go's polling/patch/schema-diff mechanics stay untouched, not
// that every registration can skip translation.
//
// # Why plan-time and apply-time packaging are split
//
// Building the deployment zip is pure local file I/O — safe to do during
// `kraai plan`, which only ever calls Get and, where implemented,
// Diff (internal/plan narrows every resource.Resource to a
// getter, see that package's own doc). Uploading the built artifact to S3
// is not safe there: `kraai plan` must never mutate anything, and
// Diff's signature carries no context to run a network call
// under cleanly regardless. So Diff here builds the artifact to hash it
// and compares the hash against the one the last deploy recorded on the
// function (see Diff), while the upload runs exclusively inside
// Create/Update, which only `kraai apply` ever calls.
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
// execution role's ARN and its environment (literal and secret-sourced),
// and builds the real AWS::Lambda::Function desired state. Every step here
// either performs local I/O (packaging) or a network call this method's
// ctx already carries — nothing here can run during `kraai plan`, only
// `kraai apply`'s Create/Update.
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
	// The execution role's own real name is spec.Name — the same derived
	// name every Tier 2 registration for this service shares (iamrole.go
	// sets RoleName to exactly this) — so its ARN is constructed the same
	// way eventsrule.go constructs the function's own: no live lookup. The
	// role is now a real DependsOn ahead of this function (register.go), so
	// a live lookup would be safe too; constructing it locally still avoids
	// a needless extra API call for a value this package can already
	// derive.
	execRoleARN := roleARN(account, spec.Name)

	env, err := resolveEnv(ctx, spec, declared.settings)
	if err != nil {
		return resource.Spec{}, err
	}
	if err := addBindingEnv(spec, env); err != nil {
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
// deployment package it runs. Code is write-only in Cloud Control, so
// without this a plan could not tell a function running yesterday's source
// from one running today's.
const artifactTagKey = "kraai:artifact-sha256"

// declaredFunctionProperties is what the manifest says about a function
// before anything is packaged, uploaded or resolved: the part of its
// desired state a plan can compute with no attributes, no secrets and no
// network.
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

	// include is set only when the service's manifest entry declares one
	// (internal/plan's expandCompute omits an empty slice from Config
	// entirely — see its own doc comment) so a plain type assertion, not a
	// defensive multi-type read like settingStr's: this comes straight from
	// manifest.Compute.Include, a typed []string field, never through a
	// free-form settings map that could carry some other shape.
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
	// Layers is emitted only when one was configured. A directly-invoked
	// function needs no layer, and sending Layers: [""] for it would be an
	// invalid ARN that Cloud Control rejects outright.
	if lambdaSettings.LayerArn != "" {
		properties["Layers"] = []any{lambdaSettings.LayerArn}
	}
	// ReservedConcurrentExecutions is set only when the manifest actually
	// declared one — nil means "no opinion," not zero, and the two must
	// never collapse into the same desired-state shape. See
	// LambdaSettings.ReservedConcurrentExecutions' own doc comment
	// (compute_settings.go) for why, and for the live schema evidence that
	// this is the correct Cloud Control property name.
	if lambdaSettings.ReservedConcurrentExecutions != nil {
		properties["ReservedConcurrentExecutions"] = *lambdaSettings.ReservedConcurrentExecutions
	}
	return declaredFunctionProperties{dir: dir, include: include, settings: lambdaSettings, properties: properties}, nil
}

// serviceNetwork returns the one network binding this provider fulfils on
// the service, or none. A function runs inside at most one VPC, so a second
// network binding is refused by name rather than one of the two winning
// quietly.
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
// when it declares one: the binding's subnet, and the VPC's own default
// security group, which allows every outbound connection and is what a
// function needs. Nothing inside the VPC has to accept inbound from the
// function; the cache's security group admits the VPC's address range.
//
// Read from what the VPC and subnet published, which this type has because
// it reads every binding on its service (register.go). A function inside a
// VPC reaches the internet only through a NAT gateway, which the network
// binding does not yet provision (evatt-labs/kraai#244): a service that
// declares a network reaches what is inside it and nothing else.
func vpcConfigFor(spec resource.Spec) (map[string]any, error) {
	network, err := serviceNetwork(spec)
	if err != nil || network == nil {
		return nil, err
	}
	subnetID, err := spec.Attribute(network.attributeKey(spec, TypeSubnet), "SubnetId")
	if err != nil {
		return nil, err
	}
	groupID, err := spec.Attribute(network.attributeKey(spec, TypeVPC), "DefaultSecurityGroup")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"SubnetIds":        []any{subnetID},
		"SecurityGroupIds": []any{groupID},
	}, nil
}

// resolveEnv builds the function's environment variables: settings.Env
// passed through verbatim, plus settings.EnvSecrets resolved through
// spec.Secret at the point of use — the credential contract this package
// is written against (see the compute_settings.go LambdaSettings.EnvSecrets
// doc comment for the namespacing rule and why no binding name is ever
// hardcoded here).
func resolveEnv(ctx context.Context, spec resource.Spec, settings LambdaSettings) (map[string]any, error) {
	env := make(map[string]any, len(settings.Env)+len(settings.EnvSecrets))
	for k, v := range settings.Env {
		env[k] = v
	}
	for envVar, secretKey := range settings.EnvSecrets {
		value, err := spec.Secret(ctx, secretKey)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation,
				"resolving %q for environment variable %q", secretKey, envVar)
		}
		env[envVar] = value
	}
	return env, nil
}

// addBindingEnv publishes what each of the service's AWS bindings resolved
// to, so the function can reach it by its binding's name: a queues binding
// JOBS becomes JOBS_QUEUE_URL and JOBS_QUEUE_ARN, an objects binding ASSETS
// becomes ASSETS_BUCKET_NAME. Read from Spec.Attributes, which the applier
// fills from what the binding's own resource published: this type reads
// every binding on its service (register.go), so it always runs after them.
//
// A binding another vendor fulfils publishes a credential, which
// settings.envSecrets maps by hand (resolveEnv). A name the manifest already
// chose for a variable is not overwritten: two sources for one variable is
// a conflict to report, not to resolve quietly.
func addBindingEnv(spec resource.Spec, env map[string]any) error {
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
		value, err := v.value(spec)
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
	value func(spec resource.Spec) (any, error)
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
		switch b.Capability {
		case manifest.CapabilityQueues:
			queueKey := b.attributeKey(spec, TypeSQSQueue)
			out = append(out,
				bindingVariable{name: prefix + "_QUEUE_URL", value: func(spec resource.Spec) (any, error) {
					return spec.Attribute(queueKey, "QueueUrl")
				}},
				bindingVariable{name: prefix + "_QUEUE_ARN", value: func(spec resource.Spec) (any, error) {
					return spec.Attribute(queueKey, "Arn")
				}})
		case manifest.CapabilityObjects:
			// The bucket's name is its identity, derived rather than
			// published, so nothing has to be read back.
			out = append(out, bindingVariable{name: prefix + "_BUCKET_NAME", value: func(resource.Spec) (any, error) {
				return b.Name, nil
			}})
		case manifest.CapabilityKeyValue:
			// The cache's endpoint only exists once created, so it is read
			// from what the cache published. Reachable only from inside
			// the cache's network, which the service's own network binding
			// places the function in (vpcConfigFor).
			if driver, _ := b.Config["driver"].(string); driver != DriverRedis {
				continue
			}
			cacheKey := b.attributeKey(spec, TypeElastiCacheServerlessCache)
			out = append(out, bindingVariable{name: prefix + "_REDIS_URL", value: func(spec resource.Spec) (any, error) {
				return cacheURL(spec, cacheKey)
			}})
		case manifest.CapabilityDatabase:
			switch driver, _ := b.Config["driver"].(string); driver {
			case DriverDynamoDB:
				// Likewise the table's name.
				out = append(out, bindingVariable{name: prefix + "_TABLE_NAME", value: func(resource.Spec) (any, error) {
					return b.Name, nil
				}})
			case DriverPostgres:
				// The cluster's endpoint is assigned at create and read
				// from what the cluster published. The URL carries no
				// password: the function signs an IAM token for the
				// endpoint when it connects (dsql.go).
				clusterKey := b.attributeKey(spec, TypeDSQLCluster)
				out = append(out, bindingVariable{name: prefix + "_DATABASE_URL", value: func(spec resource.Spec) (any, error) {
					return dsqlURL(spec, clusterKey)
				}})
			}
		}
	}
	return out, nil
}

// Diff decides whether an existing function needs a redeploy from what a
// plan can know: the manifest, the source on disk and the live function.
// Nothing is uploaded and no secret or attribute is resolved, since a plan
// has none of those to hand.
//
// Compared, in order: FunctionName, the one createOnly property, a
// difference in which is a replace; the declared properties (handler,
// runtime, architecture, sizing, layer, reserved concurrency) through the
// generic comparison; the artifact, by hashing the source directory and
// reading the hash the last deploy recorded in the function's tags; the
// environment, where a literal variable must match its value and a
// secret-sourced or binding-derived one must exist under its name; and
// whether the function is inside a VPC, which follows from whether the
// service declares a network.
//
// It used to compare only FunctionName, so a function was deployed once
// and never again (evatt-labs/kraai#245).
func (l *lambdaFunctionResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
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

	same, err := environmentMatches(spec, declared.settings, state)
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
// variable carries its value, every secret-sourced or binding-derived
// variable exists, and nothing else does. A secret's or a published
// attribute's value cannot be compared here and is not.
func environmentMatches(spec resource.Spec, settings LambdaSettings, state *resource.State) (bool, error) {
	environment, _ := state.Attributes["Environment"].(map[string]any)
	live, _ := environment["Variables"].(map[string]any)

	expected := make(map[string]bool, len(settings.Env)+len(settings.EnvSecrets))
	for name, want := range settings.Env {
		expected[name] = true
		if got, _ := live[name].(string); got != want {
			return false, nil
		}
	}
	for name := range settings.EnvSecrets {
		expected[name] = true
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
// reports a detached function either without VpcConfig or with one whose
// subnet list is empty.
func insideVPC(state *resource.State) bool {
	vpcConfig, _ := state.Attributes["VpcConfig"].(map[string]any)
	subnets, _ := vpcConfig["SubnetIds"].([]any)
	return len(subnets) > 0
}

// ValidateSpec implements plan.SpecValidator: decodeLambdaSettings is pure
// (no I/O) validation of the merged settings map — required
// runtime/architecture/layerArn, reservedConcurrency's type and sign,
// package's one accepted value, httpFrontDoor's two accepted values, and
// the unknown-key check (validateKnownSettings, settings_validate.go) —
// and this is where it runs unconditionally, before internal/plan's decide
// ever calls Get. See plan.SpecValidator's own doc comment for why this
// replaced hanging the same check off Diff, and for the real
// `kraai plan` evidence (a typo'd reservedConcurrency, an invalid
// httpFrontDoor) that Diff alone missed on a fresh environment.
func (l *lambdaFunctionResource) ValidateSpec(spec resource.Spec) error {
	settingsMap, _ := spec.Config["settings"].(map[string]any)
	_, err := decodeLambdaSettings(settingsMap)
	return err
}
