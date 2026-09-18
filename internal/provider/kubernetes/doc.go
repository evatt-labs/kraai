// Package kubernetes adapts a cluster's API to the resource contract.
//
// # One engine, not a type per kind
//
// Every Kubernetes object is reached the same way — a group, a version, a
// kind, a namespace and a name — so this is one generic client parameterized
// per call by GVK, the same shape internal/provider/aws takes over Cloud
// Control. Discovery resolves a kind to the plural path segment it lives
// under, so a kind kraai was never compiled against, including one a CRD
// installed after the fact, is reachable without new code.
//
// # Identity needs nothing extra here
//
// A Kubernetes object is addressed by a name the caller chooses, which is
// exactly the derived name kraai already computes. There is no tag to stamp,
// no list-and-match, and no provider-assigned identifier to pass between
// resources — the machinery the AWS provider needs for all three is simply
// not required. kraai keeping no state document is not something a cluster
// tolerates; it is how the API is meant to be used.
//
// # Apply rather than create-then-update
//
// Writes go through server-side apply: one PATCH carrying the complete
// desired object, which the API server creates or reconciles as needed. The
// field manager records which fields kraai owns, so a field the manifest
// stops declaring is removed rather than left behind. Conflicts are forced in
// kraai's favour, matching its contract that the manifest is the only source
// of truth.
//
// # No client-go
//
// This package speaks HTTP to the API server directly, like the Neon and
// Cloudflare providers do to theirs. Importing client-go for these four verbs
// measured at 73 additional modules against kraai's 117, pulling in a
// websocket library, a SPDY implementation and an OpenAPI stack that nothing
// here touches.
//
// The cost of that choice is exec credential plugins: a managed cluster's
// kubeconfig mints its token by running a binary, and ParseConfig refuses
// those by name rather than sending unauthenticated requests. A cluster kraai
// provisions itself authenticates with a client certificate and is
// unaffected.
package kubernetes
