package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

//go:generate go run go.uber.org/mock/mockgen -source=capability.go -destination=mock_capability_test.go -package=plugin

// Capability is one host function a plugin may be granted permission to
// import (doc.go: "a documented, minimal set of host functions a plugin
// may import ... explicitly granted, never ambient"). Host wires a
// Capability into a specific plugin's runtime only when that plugin's
// Spec names it in Grants; a Capability this package knows about
// but no loaded plugin was granted is never reachable by anything.
type Capability interface {
	// Name is both the WASM import function name a plugin calls to reach
	// this capability (under HostNamespace) and the identifier a
	// Spec names in Grants to request it.
	Name() string
	// Invoke handles one call's raw input bytes (already read out of the
	// calling plugin's memory) and returns the raw output payload. An
	// error becomes StatusError plus the error's message in the envelope
	// the plugin sees — so an implementation must treat its error strings
	// as output to untrusted code, and keep credentials, internal
	// hostnames, and wrapped transport detail out of them.
	//
	// Invoke should never panic for an ordinary failure (a bad request, a
	// failed network call) — only Host's own plumbing panics, and only for
	// a plugin-side ABI violation it cannot recover from safely.
	Invoke(ctx context.Context, input []byte) ([]byte, error)
}

// CapabilityHTTPFetch is HTTPCapability's Name(): the one host capability
// this package ships as the documented example the Grants mechanism is
// built around. A real deployment may register more via NewHost; this one
// exists so the ABI contract has a concrete, testable capability rather
// than only a hypothetical one in a comment.
const CapabilityHTTPFetch = "http_fetch"

// HTTPDoer is the minimal HTTP interface HTTPCapability wraps — satisfied
// by *http.Client as-is, so a real caller wires in its own shared,
// rate-limit-tuned client, while tests inject a mock without touching the
// network.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// httpFetchRequest is CapabilityHTTPFetch's own wire format: the ABI
// envelope (abi.go) carries opaque bytes, and JSON is this capability's
// own choice for what's inside them — a plugin author calling this
// capability from Rust, TinyGo, or Go all just send this same JSON.
type httpFetchRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// httpFetchResponse is httpFetchRequest's corresponding response shape.
type httpFetchResponse struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// HTTPCapability grants a plugin the ability to make outbound HTTP calls
// through client. It is never reachable by a plugin unless that plugin's
// Spec names CapabilityHTTPFetch in Grants.
type HTTPCapability struct {
	client HTTPDoer
}

// NewHTTPCapability builds an HTTPCapability that issues requests through
// client. A nil client gets NewGuardedHTTPClient, so the default is a
// client that cannot reach internal infrastructure.
//
// A caller supplying its own client owns that guarantee itself: see
// NewGuardedHTTPClient on why an injected transport must compose
// GuardedDialContext. This constructor cannot check it — an HTTPDoer is
// an interface, and its dialing is entirely its own business.
func NewHTTPCapability(client HTTPDoer) *HTTPCapability {
	if client == nil {
		client = NewGuardedHTTPClient()
	}
	return &HTTPCapability{client: client}
}

// Name implements Capability.
func (c *HTTPCapability) Name() string {
	return CapabilityHTTPFetch
}

// Invoke implements Capability: decodes input as an httpFetchRequest,
// performs it through c.client, and returns an httpFetchResponse, both
// JSON-encoded.
func (c *HTTPCapability) Invoke(ctx context.Context, input []byte) ([]byte, error) {
	var req httpFetchRequest
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "decoding %s request", CapabilityHTTPFetch)
	}
	if req.Method == "" || req.URL == "" {
		return nil, kerrors.Validation("%s request requires method and url", CapabilityHTTPFetch)
	}
	// Scheme is checked here rather than left to the transport because
	// the transport is injectable: the dial-time egress guard covers
	// where a request may go, and this covers what a plugin may ask the
	// host to do at all. file:// and friends never reach a dialer, so
	// nothing downstream would catch them.
	parsed, err := url.Parse(req.URL)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "parsing %s url", CapabilityHTTPFetch)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, kerrors.Validation("%s refuses scheme %q: only http and https are permitted", CapabilityHTTPFetch, parsed.Scheme)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "building %s request", CapabilityHTTPFetch)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "%s %s %s", CapabilityHTTPFetch, req.Method, req.URL)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded the same way the ABI bounds any other guest-crossing
	// region: a capability response re-enters guest memory through the
	// same writeRegion path as everything else, so an unbounded remote
	// response is exactly the DoS lever MaxTransferBytes exists to cut
	// off, just arriving from the network side of the boundary instead
	// of the plugin side.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxTransferBytes+1))
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading %s response body", CapabilityHTTPFetch)
	}
	if len(body) > MaxTransferBytes {
		return nil, kerrors.Validation("%s response body exceeds the %d-byte maximum", CapabilityHTTPFetch, MaxTransferBytes)
	}

	encoded, err := json.Marshal(httpFetchResponse{Status: resp.StatusCode, Headers: resp.Header, Body: body})
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding %s response", CapabilityHTTPFetch)
	}
	return encoded, nil
}
