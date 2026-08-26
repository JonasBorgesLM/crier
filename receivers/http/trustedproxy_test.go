package httpreceiver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func proxyRequest(t *testing.T, remoteAddr, assertedSource string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, V1.Path(),
		strings.NewReader(`{"records":[]}`))
	r.RemoteAddr = remoteAddr
	if assertedSource != "" {
		r.Header.Set(DefaultIdentityHeader, assertedSource)
	}
	return r
}

// Trusting every peer is indistinguishable from having no authentication, and
// it must not be reachable by leaving a field wide (ADR-0008, moat's M-2).
func TestTrustedProxyRefusesToTrustEveryone(t *testing.T) {
	for _, cidr := range []string{"0.0.0.0/0", "::/0"} {
		t.Run(cidr, func(t *testing.T) {
			_, err := NewTrustedProxy(TrustedProxyConfig{TrustedCIDRs: []string{cidr}})
			if err == nil {
				t.Fatal("configuration accepted; a set covering the default route must fail loudly")
			}
			if !strings.Contains(err.Error(), "would trust every peer") {
				t.Errorf("error = %q, want it to say what is wrong", err)
			}
			if !strings.Contains(err.Error(), "InsecureTrustEveryPeer") {
				t.Errorf("error = %q, want it to name the deliberate opt-out", err)
			}
		})
	}
}

// The opt-out exists, and typing its name is the point.
func TestTrustEveryPeerIsAvailableButHasToBeTyped(t *testing.T) {
	proxy, err := NewTrustedProxy(TrustedProxyConfig{
		TrustedCIDRs:           []string{"0.0.0.0/0"},
		InsecureTrustEveryPeer: true,
	})
	if err != nil {
		t.Fatalf("NewTrustedProxy: %v", err)
	}
	source, err := proxy.Authenticate(proxyRequest(t, "203.0.113.9:4444", "task-api"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if source != "task-api" {
		t.Errorf("source = %q", source)
	}
}

func TestTrustedProxyNeedsTrustedCIDRs(t *testing.T) {
	_, err := NewTrustedProxy(TrustedProxyConfig{})
	if err == nil {
		t.Fatal("configuration accepted without a trusted set")
	}
	if !strings.Contains(err.Error(), "needs TrustedCIDRs") {
		t.Errorf("error = %q", err)
	}
}

// The acceptance criterion for #25: a direct client forging the header is
// rejected when the trusted-proxy configuration does not cover it.
func TestForgedIdentityHeaderFromAnUntrustedPeerIsRejected(t *testing.T) {
	proxy, err := NewTrustedProxy(TrustedProxyConfig{TrustedCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatalf("NewTrustedProxy: %v", err)
	}

	for _, tc := range []struct {
		name       string
		remoteAddr string
		asserted   string
	}{
		{"direct client forging the header", "203.0.113.9:4444", "task-api"},
		{"neighbouring range, one bit outside", "11.0.0.1:4444", "task-api"},
		{"loopback, which is not the gateway", "127.0.0.1:4444", "task-api"},
		{"unparsable peer", "not-an-address", "task-api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := proxy.Authenticate(proxyRequest(t, tc.remoteAddr, tc.asserted))
			if err == nil {
				t.Fatalf("authenticated as %q; the header is not identity from an untrusted peer", source)
			}
			if !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("error does not wrap ErrUnauthenticated: %v", err)
			}
		})
	}
}

func TestTrustedPeerAssertionIsBelieved(t *testing.T) {
	proxy, err := NewTrustedProxy(TrustedProxyConfig{TrustedCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatalf("NewTrustedProxy: %v", err)
	}

	source, err := proxy.Authenticate(proxyRequest(t, "10.1.2.3:4444", "gateway-auth"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if source != "gateway-auth" {
		t.Errorf("source = %q, want the asserted identity", source)
	}
}

// A trusted peer that asserts nothing is not an anonymous pass: there is no
// identity to attribute the records to.
func TestTrustedPeerWithNoAssertionIsRejected(t *testing.T) {
	proxy, err := NewTrustedProxy(TrustedProxyConfig{TrustedCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatalf("NewTrustedProxy: %v", err)
	}

	if source, err := proxy.Authenticate(proxyRequest(t, "10.1.2.3:4444", "")); err == nil {
		t.Fatalf("authenticated as %q with no asserted identity", source)
	}
}

func TestCustomIdentityHeaderIsHonoured(t *testing.T) {
	proxy, err := NewTrustedProxy(TrustedProxyConfig{
		TrustedCIDRs:   []string{"10.0.0.0/8"},
		IdentityHeader: "X-Gateway-Principal",
	})
	if err != nil {
		t.Fatalf("NewTrustedProxy: %v", err)
	}

	r := proxyRequest(t, "10.1.2.3:4444", "")
	r.Header.Set("X-Gateway-Principal", "task-api")
	source, err := proxy.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if source != "task-api" {
		t.Errorf("source = %q", source)
	}
	// The default header must not still be read once another is named.
	r2 := proxyRequest(t, "10.1.2.3:4444", "someone-else")
	if source, err := proxy.Authenticate(r2); err == nil {
		t.Errorf("the default header was honoured as %q despite a custom one being configured", source)
	}
}

func TestTrustedPrefixesAreReportable(t *testing.T) {
	proxy, err := NewTrustedProxy(TrustedProxyConfig{TrustedCIDRs: []string{"10.0.0.0/8", "192.168.0.0/16"}})
	if err != nil {
		t.Fatalf("NewTrustedProxy: %v", err)
	}
	if got := proxy.TrustedPrefixes(); len(got) != 2 {
		t.Errorf("TrustedPrefixes() = %v, want 2", got)
	}
}
