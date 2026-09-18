package plugin

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Plugin HTTP egress is guarded at *dial* time, against the resolved IP,
// rather than by matching the URL string a plugin supplied.
//
// The difference is the whole point. A plugin granted CapabilityHTTPFetch
// chooses the method, the URL, and the headers, and the host issues the
// request carrying the host's own network identity — its VPC placement,
// its instance role, its side of any private link. The URL a plugin hands
// over is therefore the least trustworthy part of the request, and three
// standard tricks defeat any check made on it:
//
//   - Redirect. A plugin names an allowed public host that answers 302
//     Location: http://169.254.169.254/. net/http follows it by default;
//     a check that ran once on the original URL never sees the second
//     request.
//   - DNS rebinding. A name the plugin controls resolves to a public
//     address when checked and to a link-local one microseconds later
//     when dialed.
//   - IPv4-in-IPv6. http://[::ffff:169.254.169.254]/ is the metadata
//     service wearing a different spelling, and reaches it on any
//     dual-stack host.
//
// Checking the address the connection is actually about to be made to
// closes all three at once: every redirect hop dials again, so every hop
// is checked; the vetted address is the one dialed, so there is no window
// to rebind in; and Unmap collapses the IPv6 spelling before the check.
//
// What this is not: an allowlist. It denies the infrastructure a plugin
// has no business reaching — link-local (169.254.169.254 and its
// equivalent on every major cloud), loopback, RFC1918, CGNAT, and the
// IPv6 forms of the same — and permits the rest of the internet. Deciding
// *which* public hosts a given plugin may reach is a policy question that
// belongs in the manifest, and is deliberately not answered here.

// ErrBlockedEgress is returned when a plugin's HTTP request resolves to
// an address inside infrastructure it is not permitted to reach.
var ErrBlockedEgress = kerrors.Validation("plugin egress to internal or link-local addresses is blocked")

// isBlockedEgressIP reports whether ip belongs to a range a plugin must
// never reach through a host capability.
//
// Unmap first. netip's IsLoopback/IsPrivate/IsLinkLocalUnicast already
// see through an IPv4-mapped IPv6 address on their own, so those are not
// why. Is4 is: it reports false for ::ffff:100.64.0.1, which would skip
// the IPv4-only ranges below entirely and hand back CGNAT, 0.0.0.0/8 and
// broadcast in a spelling that walks straight past them.
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
			// 0.0.0.0/8 "this network" — routed to the local host by some
			// stacks, which makes it a loopback alias in practice.
			return true
		case b[0] == 100 && b[1]&0xC0 == 64:
			// 100.64.0.0/10 carrier-grade NAT, also where several
			// container and mesh runtimes put internal services.
			return true
		case b[0] == 255 && b[1] == 255 && b[2] == 255 && b[3] == 255:
			return true
		}
		return false
	}
	// 64:ff9b::/96 (NAT64) and 2002::/16 (6to4) both embed an IPv4
	// address that the far side unwraps and routes as v4, so either one
	// smuggles a blocked v4 destination past a v6-only check. Neither has
	// any legitimate use in a plugin's outbound request.
	if nat64 := netip.MustParsePrefix("64:ff9b::/96"); nat64.Contains(ip) {
		return true
	}
	if sixToFour := netip.MustParsePrefix("2002::/16"); sixToFour.Contains(ip) {
		return true
	}
	return false
}

// guardedDialContext wraps base so every connection is made to an address
// that has already been vetted.
//
// Resolution happens here rather than in base precisely so the vetted
// address is the dialed address: looking a name up, approving it, and
// then handing the *name* back to the dialer would let the second lookup
// return something the first never saw.
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

		// Every resolved address must pass, not merely one: a name that
		// answers with both a public and a link-local address is a
		// rebinding attempt wearing a single response, and picking the
		// permitted one out of the set would honor it.
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
// caller does not supply its own: an ordinary client whose transport
// cannot open a connection to internal infrastructure.
//
// A caller wiring in its own rate-limit-tuned client keeps full control of
// pooling, timeouts, and proxying, but MUST build its transport's
// DialContext from GuardedDialContext — otherwise it hands every granted
// plugin the host's whole internal network. HTTPCapability cannot enforce
// this for an injected client, which is exactly why the guarded client is
// what it falls back to.
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
// timeouts and keep-alive; nil gets a reasonable default.
func GuardedDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if base == nil {
		base = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	}
	return guardedDialContext(base)
}
