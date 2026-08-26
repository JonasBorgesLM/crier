package httpreceiver

import (
	"crypto/rand"
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
// One timing residue is known and accepted. secret.Value.Equal leaks whether
// the two lengths differ — which moat documents and subtle's comparison cannot
// avoid — so the unknown-source path, comparing against a fixed-length decoy,
// is not perfectly indistinguishable from a known source whose credential
// happens to be a different length. That is far below the difference an early
// return would produce, and closing it would mean holding a decoy per length
// the store contains, which leaks the same thing from the other side.
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

	// compare is the credential comparison. Nil means secret.Value.Equal.
	//
	// It is a test seam, and it exists for one reason: without it the decoy is
	// code that nothing verifies. A timing assertion cannot be made reliable
	// on a shared CI runner, so the property is pinned the other way — a test
	// observes that the unknown-source path performs a comparison at all,
	// which is what a refactor that "optimises" the early return would break.
	compare func(stored, presented secret.Value) bool
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

	decoy, err := randomSecret()
	if err != nil {
		return nil, fmt.Errorf("generating the comparison decoy: %w", err)
	}

	return &StaticCredentials{credentials: stored, decoy: decoy}, nil
}

// randomSecret produces the decoy compared against on the unknown-source path.
//
// Generated rather than written down. A constant in the source is a value an
// attacker can present deliberately, and while a matching decoy still cannot
// authenticate anyone — `known` is false either way — a decoy nobody can
// predict is one nobody can align anything against.
func randomSecret() (secret.Value, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return secret.Value{}, err
	}
	return secret.New(buf), nil
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

	// Evaluated before the `known` check, and deliberately: the comparison
	// must happen on both paths, or an unknown identifier returns faster than
	// a wrong secret and the store becomes an enumeration oracle by timing.
	//
	// The comparison itself is constant time with respect to the contents and
	// never assembles either plaintext (moat's secret.Value).
	matched := s.equal(stored, secret.New([]byte(presented)))
	if !matched || !known {
		return "", fmt.Errorf("%w: credential rejected", ErrUnauthenticated)
	}
	return source, nil
}

// equal compares a presented credential against a stored one.
func (s *StaticCredentials) equal(stored, presented secret.Value) bool {
	if s.compare != nil {
		return s.compare(stored, presented)
	}
	return stored.Equal(presented)
}
