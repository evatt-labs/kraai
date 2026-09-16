package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// fieldManager identifies kraai's ownership of the fields it applies.
//
// Server-side apply tracks which manager set which field, so a field kraai
// stops declaring is removed on the next apply rather than lingering, and a
// field another manager owns produces a reported conflict rather than a
// silent overwrite.
const fieldManager = "kraai"

// applyPatchContentType is the media type that makes a PATCH a server-side
// apply rather than a merge or a JSON patch. The body is a complete desired
// object, not a diff.
const applyPatchContentType = "application/apply-patch+yaml"

// GVK identifies a Kubernetes kind. Group is empty for the core group
// ("v1" kinds: Pod, Service, ConfigMap, Secret).
type GVK struct {
	Group   string
	Version string
	Kind    string
}

func (g GVK) String() string {
	if g.Group == "" {
		return g.Version + "/" + g.Kind
	}
	return g.Group + "/" + g.Version + "/" + g.Kind
}

// apiResource is what discovery reports about one kind: the plural path
// segment it lives under, and whether it is namespaced.
type apiResource struct {
	Plural     string
	Namespaced bool
}

// APIError is a non-2xx response from the API server, carrying the status and
// the message Kubernetes itself produced.
//
// Named fields rather than the raw body: a response body can contain the
// object that was sent, and for a Secret that is the credential itself.
type APIError struct {
	Status  int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("kubernetes API %d (%s): %s", e.Status, e.Reason, e.Message)
	}
	return fmt.Sprintf("kubernetes API %d: %s", e.Status, e.Message)
}

// Client talks to one cluster's API server.
type Client struct {
	cfg  *Config
	http *http.Client

	// discovery caches each group/version's resource list. A cluster's set
	// of kinds does not change during a run, and a plan asks about the same
	// few group/versions once per resource.
	discoMu sync.Mutex
	disco   map[string]map[string]apiResource
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the transport, for tests.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New builds a Client for cfg.
func New(cfg *Config, opts ...Option) *Client {
	c := &Client{
		cfg:   cfg,
		disco: map[string]map[string]apiResource{},
		http: &http.Client{
			Timeout:   60 * time.Second,
			Transport: &http.Transport{TLSClientConfig: cfg.TLS},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// groupVersionPath is the API prefix for a group/version. The core group
// lives under /api, everything else under /apis — the one irregularity in
// Kubernetes' URL scheme, and the reason this is a function rather than
// string concatenation at each call site.
func groupVersionPath(group, version string) string {
	if group == "" {
		return "/api/" + url.PathEscape(version)
	}
	return "/apis/" + url.PathEscape(group) + "/" + url.PathEscape(version)
}

// resourceFor resolves a kind to its plural path segment via discovery.
func (c *Client) resourceFor(ctx context.Context, gvk GVK) (apiResource, error) {
	gv := groupVersionPath(gvk.Group, gvk.Version)

	c.discoMu.Lock()
	cached, ok := c.disco[gv]
	c.discoMu.Unlock()

	if !ok {
		var list struct {
			Resources []struct {
				Name       string `json:"name"`
				Kind       string `json:"kind"`
				Namespaced bool   `json:"namespaced"`
			} `json:"resources"`
		}
		if err := c.do(ctx, http.MethodGet, gv, "", nil, &list); err != nil {
			return apiResource{}, kerrors.Wrap(err, kerrors.CodeUnexpected,
				"discovering %s", gvk)
		}
		cached = make(map[string]apiResource, len(list.Resources))
		for _, r := range list.Resources {
			// Subresources are listed as "deployments/status" and are not
			// separately addressable kinds.
			if strings.Contains(r.Name, "/") {
				continue
			}
			cached[r.Kind] = apiResource{Plural: r.Name, Namespaced: r.Namespaced}
		}
		c.discoMu.Lock()
		c.disco[gv] = cached
		c.discoMu.Unlock()
	}

	found, ok := cached[gvk.Kind]
	if !ok {
		return apiResource{}, kerrors.Validation(
			"the cluster serves no %s — check the kind's spelling and that any CRD providing it is installed", gvk)
	}
	return found, nil
}

// objectPath builds the URL path for one named object.
func (c *Client) objectPath(res apiResource, gvk GVK, namespace, name string) (string, error) {
	if res.Namespaced && namespace == "" {
		return "", kerrors.Validation("%s is namespaced but no namespace was given", gvk)
	}
	path := groupVersionPath(gvk.Group, gvk.Version)
	if res.Namespaced {
		path += "/namespaces/" + url.PathEscape(namespace)
	}
	path += "/" + res.Plural
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	return path, nil
}

// Get returns an object's current content, or found=false when it does not
// exist.
//
// Absence is an answer, not a failure: a plan asks this about every declared
// resource, and most of them do not exist yet on a first run.
func (c *Client) Get(ctx context.Context, gvk GVK, namespace, name string) (map[string]any, bool, error) {
	res, err := c.resourceFor(ctx, gvk)
	if err != nil {
		return nil, false, err
	}
	path, err := c.objectPath(res, gvk, namespace, name)
	if err != nil {
		return nil, false, err
	}

	var object map[string]any
	if err := c.do(ctx, http.MethodGet, path, "", nil, &object); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	return object, true, nil
}

// Apply writes object as kraai's declared desired state, creating it if it is
// absent and reconciling it if it is not.
//
// One verb rather than a create/update pair because server-side apply is one
// request either way. force resolves conflicts in kraai's favour: a field
// another manager has claimed is taken over rather than failing the run,
// which matches kraai's contract that the manifest is the only source of
// truth.
func (c *Client) Apply(ctx context.Context, gvk GVK, namespace, name string, object map[string]any) (map[string]any, error) {
	res, err := c.resourceFor(ctx, gvk)
	if err != nil {
		return nil, err
	}
	path, err := c.objectPath(res, gvk, namespace, name)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(object)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding %s %q", gvk, name)
	}

	query := url.Values{"fieldManager": {fieldManager}, "force": {"true"}}
	var applied map[string]any
	if err := c.do(ctx, http.MethodPatch, path+"?"+query.Encode(), applyPatchContentType, body, &applied); err != nil {
		return nil, err
	}
	return applied, nil
}

// Delete removes an object. Deleting something already gone is success, which
// is what teardown's idempotency contract requires.
func (c *Client) Delete(ctx context.Context, gvk GVK, namespace, name string) error {
	res, err := c.resourceFor(ctx, gvk)
	if err != nil {
		return err
	}
	path, err := c.objectPath(res, gvk, namespace, name)
	if err != nil {
		return err
	}

	if err := c.do(ctx, http.MethodDelete, path, "", nil, nil); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

// do performs one request and decodes a successful response into out.
func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.Server+path, reader)
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "building %s %s", method, path)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// The URL is not wrapped in: it carries the object's namespace and
		// name, and a transport error is about reachability, not identity.
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "%s to the kubernetes API", method)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "reading the kubernetes API response")
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(resp.StatusCode, payload)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding the kubernetes API response")
	}
	return nil
}

// statusError turns a non-2xx response into an APIError, reading Kubernetes'
// own Status object for the reason and message.
//
// Only those two named fields are carried. The body of a failed write echoes
// the object that was sent, which for a Secret is the credential itself.
func statusError(status int, payload []byte) error {
	var s struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &s)
	if s.Message == "" {
		s.Message = http.StatusText(status)
	}
	return &APIError{Status: status, Reason: s.Reason, Message: s.Message}
}
