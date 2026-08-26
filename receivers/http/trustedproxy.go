package httpreceiver

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/JonasBorgesLM/moat/realip"
)

// DefaultIdentityHeader carries the principal a trusted gateway has already
// authenticated.
const DefaultIdentityHeader = "X-Authenticated-Source"

// TrustedProxyConfig configures identity derived from a reverse proxy's
// assertion. Build one with NewTrustedProxy, which validates eagerly (NFR4).
type TrustedProxyConfig struct {
	// TrustedCIDRs are the peers whose identity assertion is believed —
	// gateway-auth's addresses, not the internet. Required.
	TrustedCIDRs []string

	// IdentityHeader carries the asserted principal. Zero means
	// DefaultIdentityHeader.
	IdentityHeader string

	// InsecureTrustEveryPeer accepts a trusted set covering the default
	// route. Its name is the point: it has to be typed, and it shows up in a
	// diff and in review.
	InsecureTrustEveryPeer bool
}

// TrustedProxy derives identity from an upstream that has already
// authenticated the caller — crier behind gateway-auth (IR7, ADR-0008).
//
// This is never the default and cannot become one by omission: it exists only
// where an operator constructed it, having named the peers to believe.
//
// It is the exact failure mode moat found and fixed in its own realip package
// (finding M-2), which is why the trust decision is delegated there rather
// than re-derived here. A header is only identity if the peer that set it is
// one crier was told to believe; from anyone else it is an assertion by a
// stranger.
//
// Safe for concurrent use.
type TrustedProxy struct {
	extractor *realip.Extractor
	header    string
}

var _ Authenticator = (*TrustedProxy)(nil)

// NewTrustedProxy validates cfg and returns the authenticator.
func NewTrustedProxy(cfg TrustedProxyConfig) (*TrustedProxy, error) {
	var opts []realip.Option
	if cfg.InsecureTrustEveryPeer {
		opts = append(opts, realip.InsecureTrustEveryPeer())
	}

	extractor, err := realip.New(cfg.TrustedCIDRs, opts...)
	switch {
	case errors.Is(err, realip.ErrNoTrustedProxies):
		return nil, errors.New("trusted-proxy mode needs TrustedCIDRs: without them every peer could assert any identity")
	case errors.Is(err, realip.ErrDefaultRouteTrusted):
		// Loudly, per ADR-0008 and moat's precedent M-2. A configuration that
		// trusts everyone is indistinguishable from no authentication at all,
		// and it must not be reachable by leaving a field wide.
		return nil, fmt.Errorf("trusted-proxy configuration would trust every peer, making the identity header forgeable by any client: %w; "+
			"set InsecureTrustEveryPeer to accept that deliberately", err)
	case err != nil:
		return nil, fmt.Errorf("trusted-proxy configuration: %w", err)
	}

	header := cfg.IdentityHeader
	if header == "" {
		header = DefaultIdentityHeader
	}
	return &TrustedProxy{extractor: extractor, header: header}, nil
}

// TrustedPrefixes reports the peers whose assertion is believed. Useful in a
// config dump, where "who can claim to be anyone" is the question worth being
// able to answer.
func (t *TrustedProxy) TrustedPrefixes() []netip.Prefix { return t.extractor.TrustedPrefixes() }

// KeyFunc returns a rate-limit key that an untrusted peer cannot steer, for
// use with the middleware chain.
func (t *TrustedProxy) KeyFunc() func(*http.Request) (string, error) { return t.extractor.KeyFunc() }

// Authenticate implements Authenticator.
//
// A peer outside the trusted set is rejected outright. The header is never
// treated as probably fine: that is precisely how a direct client forges an
// identity, and the whole point of naming the trusted peers is that anyone
// else's assertion means nothing.
func (t *TrustedProxy) Authenticate(r *http.Request) (string, error) {
	peer, err := peerAddr(r)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	if !t.extractor.IsTrusted(peer) {
		// Not "the header is missing" — the peer is not one we believe, and
		// saying so separately would tell a prober which half to work on.
		return "", fmt.Errorf("%w: peer is not a trusted proxy", ErrUnauthenticated)
	}

	source := strings.TrimSpace(r.Header.Get(t.header))
	if source == "" {
		return "", fmt.Errorf("%w: trusted proxy asserted no identity", ErrUnauthenticated)
	}
	return source, nil
}

// peerAddr is the address of whoever actually opened the connection. It is the
// one thing in a request that a client cannot set.
func peerAddr(r *http.Request) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port: some transports and tests hand over a bare address.
		host = r.RemoteAddr
	}
	addr, parseErr := netip.ParseAddr(strings.Trim(host, "[]"))
	if parseErr != nil {
		return netip.Addr{}, errors.New("request has no usable peer address")
	}
	return addr.Unmap(), nil
}
