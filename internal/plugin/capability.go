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
// import. Host wires it into a plugin's runtime only when that plugin's Spec
// names it in Grants.
type Capability interface {
	// Name is both the import function name a plugin calls, under
	// HostNamespace, and the identifier a Spec names in Grants.
	Name() string
	// Invoke handles one call's raw input and returns the raw output. An
	// error becomes StatusError plus its message in the envelope the plugin
	// sees, so an implementation must keep credentials and internal detail
	// out of its error strings. Invoke should never panic for an ordinary
	// failure.
	Invoke(ctx context.Context, input []byte) ([]byte, error)
}

// CapabilityHTTPFetch is HTTPCapability's Name: the one host capability
// this package ships, so the Grants mechanism has a concrete, testable
// capability.
const CapabilityHTTPFetch = "http_fetch"

// HTTPDoer is the minimal HTTP interface HTTPCapability wraps, satisfied by
// *http.Client, so a caller wires in its own shared client and tests inject
// a mock.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// httpFetchRequest is CapabilityHTTPFetch's wire format inside the ABI
// envelope, JSON so a plugin in any language sends the same thing.
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

// HTTPCapability grants a plugin outbound HTTP calls through client.
type HTTPCapability struct {
	client HTTPDoer
}

// NewHTTPCapability builds an HTTPCapability that issues requests through
// client. A nil client gets NewGuardedHTTPClient, so the default cannot
// reach internal infrastructure. A caller supplying its own client owns
// that guarantee; see NewGuardedHTTPClient.
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
// performs it, and returns an httpFetchResponse, both JSON.
func (c *HTTPCapability) Invoke(ctx context.Context, input []byte) ([]byte, error) {
	var req httpFetchRequest
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "decoding %s request", CapabilityHTTPFetch)
	}
	if req.Method == "" || req.URL == "" {
		return nil, kerrors.Validation("%s request requires method and url", CapabilityHTTPFetch)
	}
	// Checked here because the transport is injectable: the dial guard
	// covers where a request may go, and file:// never reaches a dialer.
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

	// A response re-enters guest memory through writeRegion, so an
	// unbounded remote body is the DoS lever MaxTransferBytes cuts off.
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
