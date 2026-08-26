package httpreceiver

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/JonasBorgesLM/moat/middleware"
	"github.com/JonasBorgesLM/moat/ratelimit"
	"github.com/JonasBorgesLM/moat/secureheaders"
	"github.com/JonasBorgesLM/moat/validate"
)

// Chain defaults.
const (
	// DefaultMaxBodyBytes bounds one request body. This is step 1 of the
	// canonical stage order — a transport limit, applied before anything
	// reads (ADR-0010).
	DefaultMaxBodyBytes = 4 << 20 // 4 MiB
	// DefaultRateLimitBurst is how many requests a key may make at once.
	DefaultRateLimitBurst = 200
	// DefaultRateLimitPerSecond is the sustained rate per key.
	DefaultRateLimitPerSecond = 100
)

// ContentTypeJSON is the only media type v1 accepts.
const ContentTypeJSON = "application/json"

// ChainConfig configures the request-level guards wrapped around the receiver.
//
// They come from moat rather than being reimplemented here (IR1): a rate
// limiter and a body-size guard written a second time are two more things to
// get subtly wrong, and moat's are the ones with the test suites.
type ChainConfig struct {
	// MaxBodyBytes bounds one request body. Zero means DefaultMaxBodyBytes.
	MaxBodyBytes int64

	// RateLimitBurst and RateLimitPerSecond bound how fast one key may send.
	// Zero means the defaults.
	RateLimitBurst     int
	RateLimitPerSecond float64

	// RateLimitKey derives the key a request is limited under. Nil limits by
	// peer address.
	//
	// Behind a reverse proxy that is wrong — every request arrives from the
	// proxy, so one key covers every tenant. Pass TrustedProxy.KeyFunc there,
	// which an untrusted peer cannot steer.
	RateLimitKey func(*http.Request) (string, error)

	// DisableRateLimit omits rate limiting.
	//
	// It exists so that omitting a control is something someone typed rather
	// than something a zero value did, following moat's own reasoning about
	// presets that quietly drop a protection they were not given.
	DisableRateLimit bool
}

// Handler returns the receiver wrapped in the request-level guards (IR1).
//
// Order is a security property, not a style choice, and this one is:
//
//	secure headers -> rate limit -> content type -> body size -> receiver
//
// Security headers are outermost so that *error* responses carry them too —
// moat's own ordering lesson, and the reason it is worth restating is that the
// mistake is invisible in the happy path, where every response already looks
// right.
//
// The body-size limit sits inside the cheap rejections and outside the
// handler, because everything below it reads the body: a guard that runs after
// the read has already allowed the work it exists to prevent.
func (rc *Receiver) Handler(cfg ChainConfig) (http.Handler, error) {
	maxBody := cfg.MaxBodyBytes
	if maxBody == 0 {
		maxBody = DefaultMaxBodyBytes
	}
	if maxBody < 0 {
		return nil, fmt.Errorf("negative MaxBodyBytes %d", maxBody)
	}

	chain := middleware.New(
		// Outermost: every response, including the ones the guards below
		// generate, carries the headers.
		secureheaders.Middleware(
			// There is no browser here. A content policy for a machine-to-
			// machine JSON endpoint protects nobody and only invites someone
			// to relax it later for a reason that will not apply.
			secureheaders.WithoutCSP(),
			secureheaders.WithoutFrameOptions(),
		),
	)

	if !cfg.DisableRateLimit {
		limiter, err := rc.limiter(cfg)
		if err != nil {
			return nil, err
		}
		chain = chain.Append(limiter.Middleware)
	}

	chain = chain.Append(
		validate.RequireContentType([]string{ContentTypeJSON}),
		validate.MaxBodyBytes(maxBody),
	)

	return chain.Then(rc.Mux()), nil
}

// limiter builds the rate limiter for the chain.
func (rc *Receiver) limiter(cfg ChainConfig) (*ratelimit.Limiter, error) {
	burst := cfg.RateLimitBurst
	if burst == 0 {
		burst = DefaultRateLimitBurst
	}
	perSecond := cfg.RateLimitPerSecond
	if perSecond == 0 {
		perSecond = DefaultRateLimitPerSecond
	}
	if burst < 0 || perSecond < 0 {
		return nil, fmt.Errorf("negative rate limit: burst %d, %v/s", burst, perSecond)
	}

	opts := []ratelimit.Option{
		// A store failure must not become an open door. Telemetry is
		// droppable; an unbounded ingestion endpoint is not.
		ratelimit.WithFailureMode(ratelimit.FailClosed),
		ratelimit.WithStore(ratelimit.NewMemoryStore(
			ratelimit.WithIdleTTL(10 * time.Minute),
		)),
	}
	if cfg.RateLimitKey != nil {
		opts = append(opts, ratelimit.WithKeyFunc(cfg.RateLimitKey))
	}

	limiter := ratelimit.New(burst, perSecond, opts...)
	if limiter == nil {
		return nil, errors.New("rate limiter could not be built")
	}
	return limiter, nil
}
