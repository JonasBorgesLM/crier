package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/JonasBorgesLM/moat/secret"

	"github.com/JonasBorgesLM/crier/core"
	"github.com/JonasBorgesLM/crier/exporters/otlp"
	httpreceiver "github.com/JonasBorgesLM/crier/receivers/http"
)

// Defaults for the daemon.
const (
	defaultListen      = ":4318"
	defaultAdminListen = "127.0.0.1:9464"
	defaultDrain       = 30 * time.Second
)

// Config is the daemon's whole configuration (NFR4).
//
// It is a file plus environment overrides, and it is validated by being used:
// every component below builds itself eagerly and refuses bad input, so this
// layer maps and delegates rather than re-deriving rules that already exist in
// two places. A second copy of the fair-share arithmetic here is a second copy
// to get wrong.
type Config struct {
	// Listen is the ingestion address.
	Listen string `json:"listen"`

	// AdminListen serves /healthz and /readyz, on its own address and bound
	// to loopback by default.
	//
	// Separate from ingestion deliberately: the readiness reason names which
	// destinations are refusing calls, which is operational detail for whoever
	// runs crier, not for whoever sends it logs.
	AdminListen string `json:"adminListen"`

	// DrainTimeout bounds shutdown. Records still buffered when it expires
	// are lost, counted, and reported (ADR-0015).
	DrainTimeout Duration `json:"drainTimeout"`

	Buffer    BufferConfig     `json:"buffer"`
	Limits    LimitsConfig     `json:"limits"`
	Redaction RedactionConfig  `json:"redaction"`
	Filter    FilterConfig     `json:"filter"`
	Auth      AuthConfig       `json:"auth"`
	Exporters []ExporterConfig `json:"exporters"`
}

// BufferConfig sizes the buffer and its per-source shares.
type BufferConfig struct {
	Capacity     int            `json:"capacity"`
	BatchSize    int            `json:"batchSize"`
	BatchWindow  Duration       `json:"batchWindow"`
	Policy       string         `json:"policy"`
	Workers      int            `json:"workers"`
	Reservations map[string]int `json:"reservations"`
	// UnlistedPool is capacity held for sources without a reservation, shared
	// among all of them (ADR-0019).
	//
	// It is part of the capacity arithmetic, not an extra on top of it:
	// reservations + pool must fit inside the buffer. Validating only the
	// reservations lets a configuration start while over-committing, and the
	// symptom is not a crash — quota accounting believes there is room while
	// the store rejects.
	UnlistedPool int `json:"unlistedPool"`
}

// LimitsConfig caps record size (ADR-0010).
type LimitsConfig struct {
	MaxAttributes int `json:"maxAttributes"`
	MaxKeyBytes   int `json:"maxKeyBytes"`
	MaxValueBytes int `json:"maxValueBytes"`
	MaxBodyBytes  int `json:"maxBodyBytes"`
	// MaxDistinctValues bounds the cardinality guard. Zero disables it.
	MaxDistinctValues int `json:"maxDistinctValues"`
}

// RedactionConfig configures masking (ADR-0014).
type RedactionConfig struct {
	// Disabled turns redaction off entirely. It has to be typed: there is no
	// fail-open redactor, only no redactor, and that is a choice someone makes
	// deliberately rather than by leaving a field empty.
	Disabled      bool     `json:"disabled"`
	KeySubstrings []string `json:"keySubstrings"`
	KeyPatterns   []string `json:"keyPatterns"`
	BodyPatterns  []string `json:"bodyPatterns"`
	SkipBody      bool     `json:"skipBody"`
}

// FilterConfig configures the severity threshold and sampler.
type FilterConfig struct {
	MinSeverity int     `json:"minSeverity"`
	SampleRate  float64 `json:"sampleRate"`
}

// AuthConfig configures how a caller's identity is established (ADR-0008).
type AuthConfig struct {
	// Credentials map a source identifier to its secret. Read from the file
	// or, preferably, from the environment.
	Credentials map[string]string `json:"credentials"`

	// TrustedProxy derives identity from a reverse proxy's assertion instead.
	// Nil means direct authentication.
	TrustedProxy *TrustedProxyConfig `json:"trustedProxy"`
}

// TrustedProxyConfig mirrors the receiver's, kept separate so the daemon's
// configuration shape does not leak into the library's API.
type TrustedProxyConfig struct {
	TrustedCIDRs   []string `json:"trustedCIDRs"`
	IdentityHeader string   `json:"identityHeader"`
	// InsecureTrustEveryPeer accepts a trusted set covering the default route.
	InsecureTrustEveryPeer bool `json:"insecureTrustEveryPeer"`
}

// ExporterConfig is one destination.
type ExporterConfig struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	// Credential is the token. Prefer the environment over the file.
	Credential              string            `json:"credential"`
	CredentialHeader        string            `json:"credentialHeader"`
	CredentialScheme        string            `json:"credentialScheme"`
	Headers                 map[string]string `json:"headers"`
	Compression             string            `json:"compression"`
	AllowInsecureCredential bool              `json:"allowInsecureCredential"`

	// Filter additionally narrows what this destination receives, after
	// dequeue — on top of the top-level Filter, never instead of it
	// (ADR-0010, FR8, issue #45). The zero value keeps everything this
	// destination would otherwise get.
	Filter FilterConfig `json:"filter"`
}

// Duration is a time.Duration that unmarshals from a Go duration string, so a
// configuration file says "30s" rather than a count of nanoseconds nobody can
// read.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\"")
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("%q is not a duration", text)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// LoadConfig reads the file at path, if any, then applies environment
// overrides.
//
// Both are optional; the defaults are a working single-destination daemon
// except for the parts that cannot have a default — where to export, and who
// may send.
func LoadConfig(path string, getenv func(string) string) (Config, error) {
	cfg := Config{
		Listen:       defaultListen,
		AdminListen:  defaultAdminListen,
		DrainTimeout: Duration(defaultDrain),
	}

	if path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own argument
		if err != nil {
			return Config{}, fmt.Errorf("reading %s: %w", path, err)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		// Same posture as the wire format: a misspelled key that is silently
		// ignored is a setting the operator believes is in effect.
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("parsing %s: %w", path, err)
		}
	}

	if err := applyEnv(&cfg, getenv); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnv overlays CRIER_* variables.
//
// Credentials belong here rather than in the file: a file is committed by
// accident far more often than an environment is.
func applyEnv(cfg *Config, getenv func(string) string) error {
	if v := getenv("CRIER_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := getenv("CRIER_ADMIN_LISTEN"); v != "" {
		cfg.AdminListen = v
	}
	if v := getenv("CRIER_DRAIN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("CRIER_DRAIN_TIMEOUT=%q is not a duration", v)
		}
		cfg.DrainTimeout = Duration(d)
	}
	if v := getenv("CRIER_BUFFER_CAPACITY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CRIER_BUFFER_CAPACITY=%q is not a number", v)
		}
		cfg.Buffer.Capacity = n
	}
	if v := getenv("CRIER_UNLISTED_POOL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CRIER_UNLISTED_POOL=%q is not a number", v)
		}
		cfg.Buffer.UnlistedPool = n
	}

	// CRIER_SOURCES declares which source identifiers exist, so a credential
	// can live only in the environment and the file can hold none at all.
	//
	// It is a separate variable because envKey is lossy: "checkout-service"
	// and "checkout.service" both become CHECKOUT_SERVICE, so the environment
	// variable's name cannot be turned back into an identifier. The source
	// list carries the exact names; the per-source variables carry the
	// secrets, one each.
	for _, source := range splitList(getenv("CRIER_SOURCES")) {
		if cfg.Auth.Credentials == nil {
			cfg.Auth.Credentials = map[string]string{}
		}
		if _, declared := cfg.Auth.Credentials[source]; !declared {
			cfg.Auth.Credentials[source] = ""
		}
	}

	// CRIER_CREDENTIAL_<SOURCE> and CRIER_EXPORTER_CREDENTIAL_<NAME>, so a
	// secret never has to be written into the file at all.
	for source := range cfg.Auth.Credentials {
		if v := getenv("CRIER_CREDENTIAL_" + envKey(source)); v != "" {
			cfg.Auth.Credentials[source] = v
		}
	}
	for i, exp := range cfg.Exporters {
		if v := getenv("CRIER_EXPORTER_CREDENTIAL_" + envKey(exp.Name)); v != "" {
			cfg.Exporters[i].Credential = v
		}
	}
	return nil
}

// splitList parses a comma-separated list, ignoring blanks.
func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// envKey renders a source or exporter name as an environment-variable suffix.
func envKey(name string) string {
	upper := strings.ToUpper(name)
	return strings.NewReplacer("-", "_", ".", "_", "/", "_").Replace(upper)
}

// checkSelfIngestion refuses a configuration in which this instance exports to
// its own receiver (NFR11).
//
// That is not observability, it is a feedback loop: every exported batch
// produces operational logs, which are ingested, exported, and produce more.
// The check is on the address rather than trusted to documentation, because
// the mistake is easy to make behind a service name that resolves to self.
func checkSelfIngestion(listen string, exporters []ExporterConfig) error {
	listenHost, listenPort, err := net.SplitHostPort(listen)
	if err != nil {
		// An unparsable listen address is a real error, but it is the
		// listener's to report — failing here would blame the exporters for
		// it.
		//
		//nolint:nilerr // reported where it can say something useful
		return nil
	}

	for _, exp := range exporters {
		host, port, endpointErr := endpointHostPort(exp.Endpoint)
		if endpointErr != nil || port != listenPort {
			continue
		}
		if isSelf(host, listenHost) {
			return fmt.Errorf(
				"exporter %q points at this instance's own receiver (%s): crierd must never ingest its own operational logs (NFR11), "+
					"which is a feedback loop rather than observability", exp.Name, exp.Endpoint)
		}
	}
	return nil
}

func endpointHostPort(endpoint string) (host, port string, err error) {
	trimmed := endpoint
	for _, scheme := range []string{"http://", "https://"} {
		trimmed = strings.TrimPrefix(trimmed, scheme)
	}
	if i := strings.IndexAny(trimmed, "/?"); i >= 0 {
		trimmed = trimmed[:i]
	}
	return net.SplitHostPort(trimmed)
}

// isSelf reports whether an endpoint host is this instance.
func isSelf(endpointHost, listenHost string) bool {
	if endpointHost == listenHost {
		return true
	}
	loopback := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "[::1]": true}
	// A wildcard listener answers on every address the host has, loopback
	// included, so any loopback endpoint reaches it.
	if listenHost == "" || listenHost == "0.0.0.0" || listenHost == "::" {
		return loopback[endpointHost]
	}
	return loopback[endpointHost] && loopback[listenHost]
}

// secretsFrom converts configured credentials into masked values (NFR4, IR2).
func secretsFrom(credentials map[string]string) (map[string]secret.Value, error) {
	if len(credentials) == 0 {
		return nil, errors.New("no ingestion credentials configured; the receiver would reject every request")
	}
	out := make(map[string]secret.Value, len(credentials))
	for source, value := range credentials {
		if value == "" {
			return nil, fmt.Errorf("source %q has an empty credential", source)
		}
		out[source] = secret.New([]byte(value))
	}
	return out, nil
}

// dropPolicy maps the configured name.
func dropPolicy(name string) (core.DropPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "reject":
		return core.DropPolicyReject, nil
	case "block":
		return core.DropPolicyBlock, nil
	case "drop-oldest", "drop_oldest":
		return core.DropPolicyDropOldest, nil
	default:
		return 0, fmt.Errorf("drop policy %q is not one of reject, block, drop-oldest", name)
	}
}

// authenticator builds the receiver's authenticator from configuration.
func authenticator(cfg AuthConfig) (httpreceiver.Authenticator, error) {
	if cfg.TrustedProxy != nil {
		return httpreceiver.NewTrustedProxy(httpreceiver.TrustedProxyConfig{
			TrustedCIDRs:           cfg.TrustedProxy.TrustedCIDRs,
			IdentityHeader:         cfg.TrustedProxy.IdentityHeader,
			InsecureTrustEveryPeer: cfg.TrustedProxy.InsecureTrustEveryPeer,
		})
	}
	credentials, err := secretsFrom(cfg.Credentials)
	if err != nil {
		return nil, err
	}
	return httpreceiver.NewStaticCredentials(credentials)
}

// exporters builds every destination.
func exporters(configs []ExporterConfig) (map[string]core.Exporter, error) {
	if len(configs) == 0 {
		return nil, errors.New("no exporters configured; crier would accept records and discard them")
	}

	out := make(map[string]core.Exporter, len(configs))
	for _, cfg := range configs {
		if cfg.Name == "" {
			return nil, fmt.Errorf("exporter for %q has no name; names label every counter", cfg.Endpoint)
		}
		if _, duplicate := out[cfg.Name]; duplicate {
			return nil, fmt.Errorf("exporter %q is configured twice", cfg.Name)
		}

		var credential secret.Value
		if cfg.Credential != "" {
			credential = secret.New([]byte(cfg.Credential))
		}
		exporter, err := otlp.New(otlp.Config{
			Endpoint:                cfg.Endpoint,
			Headers:                 cfg.Headers,
			Credential:              credential,
			CredentialHeader:        cfg.CredentialHeader,
			CredentialScheme:        cfg.CredentialScheme,
			Compression:             otlp.Compression(cfg.Compression),
			AllowInsecureCredential: cfg.AllowInsecureCredential,
		})
		if err != nil {
			return nil, fmt.Errorf("exporter %q: %w", cfg.Name, err)
		}
		out[cfg.Name] = exporter
	}
	return out, nil
}
