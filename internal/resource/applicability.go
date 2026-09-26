package resource

// ApplicabilityContext is everything a condition may ask about: the
// manifest's vendor choices and, when the caller has one, the service the
// registrations are being resolved for.
//
// Trigger, Settings, CustomDomain and Binding are zero-valued when the caller
// has no service or no entry, and each condition decides what a zero value
// means. RequiresTrigger treats an absent trigger as satisfied; the others do
// not. See each one for why.
type ApplicabilityContext struct {
	// Vendors maps each configured capability to the vendor fulfilling it.
	Vendors map[string]string
	// Trigger is what invokes the service, "" when it declares no compute
	// block or when a binding rather than compute is being resolved.
	Trigger string
	// Settings is the service's merged compute settings, nil for a caller
	// with none.
	Settings map[string]any
	// CustomDomain reports whether the service has a route declaring one.
	CustomDomain bool
	// Binding is the manifest entry being resolved for, without its name.
	// Nil when resolving compute, which has no entry.
	Binding map[string]any
}

// Applicability reports whether a registration applies in a context.
type Applicability func(ApplicabilityContext) bool

// RequiresCapabilityVendor is satisfied only when capability is fulfilled by
// vendor. A companion resource can depend on a capability other than its
// own: Cloudflare Hyperdrive is asked for by choosing Neon for a database,
// but it is a Workers connection pooler and belongs only when compute is
// Workers too.
func RequiresCapabilityVendor(capability, vendor string) Applicability {
	return func(ctx ApplicabilityContext) bool { return ctx.Vendors[capability] == vendor }
}

// RequiresTrigger is satisfied when the service declares one of triggers,
// the strings internal/manifest exports ("http", "schedule"). A service that
// declares no trigger satisfies it too, which is what lets a binding be
// resolved at the same point as compute without a trigger condition ever
// narrowing it.
//
// With no triggers it narrows to services that declare none, which is almost
// certainly not what the caller meant. A provider's own "capabilities cover
// registrations" test is where that is caught.
func RequiresTrigger(triggers ...string) Applicability {
	return func(ctx ApplicabilityContext) bool {
		if ctx.Trigger == "" {
			return true
		}
		for _, t := range triggers {
			if t == ctx.Trigger {
				return true
			}
		}
		return false
	}
}

// RequiresSettings is satisfied when the service's merged compute settings
// satisfy want. It is for the choice a trigger cannot express: two
// registrations that apply to the same trigger, of which a manifest must pick
// exactly one (a Lambda function URL or an API Gateway HTTP API).
//
// want sees nil settings too, unlike RequiresTrigger's treatment of an absent
// trigger. Waving a settings condition through on nil would make both of a
// mutually exclusive pair apply at once, the bug this condition exists to
// prevent. Nothing calls it without settings: every settings-conditioned
// registration is compute, and the planner resolves compute with
// manifest.MergeSettings' output, which is never nil.
func RequiresSettings(want func(settings map[string]any) bool) Applicability {
	return func(ctx ApplicabilityContext) bool { return want(ctx.Settings) }
}

// RequiresCustomDomain is satisfied when the service has a route declaring a
// custom domain. An absent service does not satisfy it: a custom domain is
// asked for by name, and "nobody asked" must not read as "everyone gets one".
func RequiresCustomDomain() Applicability {
	return func(ctx ApplicabilityContext) bool { return ctx.CustomDomain }
}

// RequiresBindingKey is satisfied when the binding entry being resolved for
// carries key. An absent entry does not satisfy it, for the same reason as
// RequiresCustomDomain.
func RequiresBindingKey(key string) Applicability {
	return func(ctx ApplicabilityContext) bool {
		_, ok := ctx.Binding[key]
		return ok
	}
}

// And is satisfied when every one of conditions is. Registration.Applies
// already ANDs its entries; this is for nesting inside Or or Not.
func And(conditions ...Applicability) Applicability {
	return func(ctx ApplicabilityContext) bool {
		for _, c := range conditions {
			if !c(ctx) {
				return false
			}
		}
		return true
	}
}

// Or is satisfied when any one of conditions is. No conditions is not
// satisfied.
func Or(conditions ...Applicability) Applicability {
	return func(ctx ApplicabilityContext) bool {
		for _, c := range conditions {
			if c(ctx) {
				return true
			}
		}
		return false
	}
}

// Not inverts condition.
func Not(condition Applicability) Applicability {
	return func(ctx ApplicabilityContext) bool { return !condition(ctx) }
}
