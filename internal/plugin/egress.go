package plugin

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Plugin HTTP egress is guarded at dial time, against the resolved IP,
// rather than by matching the URL a plugin supplied. The host issues the
// request with its own network identity while the plugin chooses the URL,
// and three standard tricks defeat a check on the string: a redirect to a
// link-local address, a DNS answer that rebinds between check and dial, and
// an IPv4-mapped IPv6 literal. Checking the address about to be dialed
// closes all three: every redirect hop dials again, the vetted address is
// the dialed one, and Unmap collapses the spelling.
//
// This denies the infrastructure a plugin has no business reaching and
// permits the rest of the internet; which public hosts a plugin may reach
// is manifest policy.

// ErrBlockedEgress is returned when a plugin's HTTP request resolves to an
// address inside infrastructure it is not permitted to reach.
var ErrBlockedEgress = kerrors.Validation("plugin egress to internal or link-local addresses is blocked")

// isBlockedEgressIP reports whether ip belongs to a range a plugin must
// never reach. Unmap first: Is4 reports false for ::ffff:100.64.0.1, which
// would skip the IPv4-only ranges below.
func isBlockedEgressIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() {
		return true
	}
	if ip.Is4() {
		b := ip.As4()
		switch {
		case b[0] == 0:
			// 0.0.0.0/8, routed to the local host by some stacks.
			return true
		case b[0] == 100 && b[1]&0xC0 == 64:
			// 100.64.0.0/10, carrier-grade NAT and several container and
			// mesh runtimes' internal services.
			return true
		case b[0] == 255 && b[1] == 255 && b[2] == 255 && b[3] == 255:
			return true
		}
		return false
	}
	// NAT64 and 6to4 both embed an IPv4 address the far side unwraps, so
	// either smuggles a blocked v4 destination past a v6-only check.
	if nat64 := netip.MustParsePrefix("64:ff9b::/96"); nat64.Contains(ip) {
		return true
	}
	if sixToFour := netip.MustParsePrefix("2002::/16"); sixToFour.Contains(ip) {
		return true
	}
	return false
}

// guardedDialContext wraps base so every connection is made to an address
// that has already been vetted. Resolution happens here so the vetted
// address is the dialed address; handing the name back to the dialer would
// let a second lookup return something the first never saw.
func guardedDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation, "parsing plugin egress address %q", addr)
		}

		var candidates []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			candidates = []netip.Addr{ip}
		} else {
			candidates, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "resolving plugin egress host %q", host)
			}
		}

		// Every resolved address must pass: a name answering with both a
		// public and a link-local address is a rebinding attempt in one
		// response.
		for _, ip := range candidates {
			if isBlockedEgressIP(ip) {
				return nil, kerrors.Wrap(ErrBlockedEgress, kerrors.CodeValidation,
					"plugin egress to %s resolved to %s", host, ip)
			}
		}
		if len(candidates) == 0 {
			return nil, kerrors.Validation("plugin egress host %q resolved to no addresses", host)
		}

		var lastErr error
		for _, ip := range candidates {
			conn, err := base.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, kerrors.Wrap(lastErr, kerrors.CodeUnexpected, "dialing plugin egress host %q", host)
	}
}

// NewGuardedHTTPClient returns the http.Client HTTPCapability uses when a
// caller does not supply its own: one whose transport cannot open a
// connection to internal infrastructure. A caller wiring in its own client
// MUST build its transport's DialContext from GuardedDialContext, or it
// hands every granted plugin the host's whole internal network; nothing
// here can check an injected client.
func NewGuardedHTTPClient() *http.Client {
	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:           guardedDialContext(base),
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}

// GuardedDialContext exposes the dial-time egress guard so a caller
// building its own transport composes the same check. base supplies the
// timeouts and keep-alive; nil gets a default.
func GuardedDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if base == nil {
		base = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	}
	return guardedDialContext(base)
}
