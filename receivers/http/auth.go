package httpreceiver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/JonasBorgesLM/moat/secret"
)

// ErrUnauthenticated means the request carried no usable credential, or one
// that did not verify.
//
// It is deliberately one error for both. Telling a caller whether the source
// identifier exists turns the credential store into an enumeration oracle, and
// the caller can do nothing different with the distinction anyway.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticator establishes who is calling.
//
// It returns the attested source identity, which is the only identity the
// pipeline will use: attribution, quotas and metrics all key on it, never on
// anything in the request body (ADR-0008, finding D-2).
//
// Standalone mode only. In embedded mode there is no receiver, because the
// host application owns the trust boundary (FR11).
type Authenticator interface {
	// Authenticate returns the source identity, or an error wrapping
	// ErrUnauthenticated. It must not put credential material in that error.
	Authenticate(r *http.Request) (source string, err error)
}

// StaticCredentials authenticates a shared secret per source, configured up
// front.
//
// mTLS is the recommended production alternative and is phase two (ADR-0008).
// This exists because a shared secret is what a small deployment will actually
// configure, and the alternative to supporting it well is people disabling
// authentication entirely.
//
// Safe for concurrent use.
type StaticCredentials struct {
	credentials map[string]secret.Value
	// decoy is compared against when the source is unknown, so an unknown
	// identifier and a wrong credential take the same path. Without it, the
	// unknown case returns before any comparison and the difference is
	// measurable.
	decoy secret.Value
}

var _ Authenticator = (*StaticCredentials)(nil)

// NewStaticCredentials validates the credential set eagerly (NFR4).
func NewStaticCredentials(credentials map[string]secret.Value) (*StaticCredentials, error) {
	if len(credentials) == 0 {
		// An authenticator that authenticates nobody accepts nobody, which is
		// safe, and is almost certainly a configuration mistake — so it is
		// refused rather than left to be discovered as "the endpoint rejects
		// everything".
		return nil, errors.New("no credentials configured; the receiver would reject every request")
	}

	stored := make(map[string]secret.Value, len(credentials))
	for source, credential := range credentials {
		if strings.TrimSpace(source) == "" {
			return nil, errors.New("a credential is configured under an empty source identifier")
		}
		if strings.ContainsAny(source, ": \t") {
			// The wire format separates identifier from secret with a colon.
			return nil, fmt.Errorf("source %q contains a colon or whitespace, which the credential format uses as a separator", source)
		}
		if credential.IsZero() {
			return nil, fmt.Errorf("source %q has an empty credential", source)
		}
		stored[source] = credential
	}

	return &StaticCredentials{
		credentials: stored,
		decoy:       secret.New([]byte("decoy-credential-for-constant-time-comparison")),
	}, nil
}

// Sources lists the configured source identifiers. Useful in a config dump,
// and safe in one — the credentials are not in it.
func (s *StaticCredentials) Sources() []string {
	out := make([]string, 0, len(s.credentials))
	for source := range s.credentials {
		out = append(out, source)
	}
	return out
}

// Authenticate implements Authenticator.
//
// The credential is carried as `Authorization: Bearer <source>:<secret>`. The
// source identifier is not an identity claim — it selects which credential to
// verify. Identity is only conferred once that credential verifies, which is
// what makes this server-derived rather than client-asserted (ADR-0008).
func (s *StaticCredentials) Authenticate(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", fmt.Errorf("%w: no Authorization header", ErrUnauthenticated)
	}

	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("%w: expected a Bearer credential", ErrUnauthenticated)
	}

	source, presented, found := strings.Cut(strings.TrimSpace(rest), ":")
	if !found || source == "" || presented == "" {
		// The shape is wrong, so there is nothing to compare. Named without
		// echoing any of what was sent.
		return "", fmt.Errorf("%w: credential is not in the form <source>:<secret>", ErrUnauthenticated)
	}

	stored, known := s.credentials[source]
	if !known {
		stored = s.decoy
	}

	// Constant time with respect to the contents, and it never assembles
	// either plaintext (moat's secret.Value).
	if !stored.Equal(secret.New([]byte(presented))) || !known {
		return "", fmt.Errorf("%w: credential rejected", ErrUnauthenticated)
	}
	return source, nil
}
