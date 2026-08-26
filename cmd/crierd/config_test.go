package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crier.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}
	return path
}

func TestDefaultsApplyWithoutAFile(t *testing.T) {
	cfg, err := LoadConfig("", noEnv)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != defaultListen {
		t.Errorf("Listen = %q, want %q", cfg.Listen, defaultListen)
	}
	// Loopback by default: the readiness reason names which destinations are
	// refusing calls, which is not for whoever sends logs.
	if !strings.HasPrefix(cfg.AdminListen, "127.0.0.1") {
		t.Errorf("AdminListen = %q, want a loopback default", cfg.AdminListen)
	}
}

// Same posture as the wire format: a misspelled key silently ignored is a
// setting the operator believes is in effect.
func TestUnknownConfigKeyIsRejectedAndNamed(t *testing.T) {
	path := writeConfig(t, `{"lisen": ":4318"}`)

	_, err := LoadConfig(path, noEnv)
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "lisen") {
		t.Errorf("error = %q, want it to name the offending key", err)
	}
}

func TestDurationsReadAsStrings(t *testing.T) {
	path := writeConfig(t, `{"drainTimeout": "45s"}`)

	cfg, err := LoadConfig(path, noEnv)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.DrainTimeout.String(); got != "45s" {
		t.Errorf("DrainTimeout = %s, want 45s", got)
	}

	bad := writeConfig(t, `{"drainTimeout": "eventually"}`)
	if _, err := LoadConfig(bad, noEnv); err == nil {
		t.Error("an unparsable duration was accepted")
	}
}

// A file is committed by accident far more often than an environment is.
func TestCredentialsComeFromTheEnvironment(t *testing.T) {
	path := writeConfig(t, `{
		"auth": {"credentials": {"task-api": "placeholder"}},
		"exporters": [{"name": "primary", "endpoint": "https://collector:4318", "credential": "placeholder"}]
	}`)
	env := map[string]string{
		"CRIER_CREDENTIAL_TASK_API":         "the-real-ingest-token",
		"CRIER_EXPORTER_CREDENTIAL_PRIMARY": "the-real-export-token",
	}

	cfg, err := LoadConfig(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Auth.Credentials["task-api"]; got != "the-real-ingest-token" {
		t.Errorf("ingest credential = %q, want the environment's", got)
	}
	if got := cfg.Exporters[0].Credential; got != "the-real-export-token" {
		t.Errorf("export credential = %q, want the environment's", got)
	}
}

func TestEnvironmentOverridesAreValidated(t *testing.T) {
	for _, tc := range []struct{ key, value, want string }{
		{"CRIER_DRAIN_TIMEOUT", "soon", "not a duration"},
		{"CRIER_BUFFER_CAPACITY", "lots", "not a number"},
		{"CRIER_UNLISTED_POOL", "some", "not a number"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			_, err := LoadConfig("", func(k string) string {
				if k == tc.key {
					return tc.value
				}
				return ""
			})
			if err == nil {
				t.Fatalf("%s=%q was accepted", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// NFR11. Every exported batch produces operational logs, which are ingested,
// exported, and produce more.
func TestExportingToOwnReceiverIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		listen   string
		endpoint string
		wantErr  bool
	}{
		{"loopback endpoint, wildcard listener", ":4318", "http://localhost:4318", true},
		{"explicit loopback both sides", "127.0.0.1:4318", "http://127.0.0.1:4318/v1/logs", true},
		{"same explicit address", "10.0.0.5:4318", "https://10.0.0.5:4318", true},
		{"different port", ":4318", "http://localhost:9999", false},
		{"a real collector elsewhere", ":4318", "https://collector.example.com:4318", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSelfIngestion(tc.listen, []ExporterConfig{{Name: "primary", Endpoint: tc.endpoint}})

			if tc.wantErr && err == nil {
				t.Fatal("the configuration was accepted; crierd would ingest its own logs")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a legitimate destination was refused: %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "NFR11") {
				t.Errorf("error = %q, want it to name the requirement", err)
			}
		})
	}
}

func TestDropPolicyNames(t *testing.T) {
	for _, name := range []string{"", "reject", "block", "drop-oldest", "DROP_OLDEST"} {
		if _, err := dropPolicy(name); err != nil {
			t.Errorf("dropPolicy(%q): %v", name, err)
		}
	}
	if _, err := dropPolicy("discard-everything"); err == nil {
		t.Error("an unknown drop policy was accepted")
	}
}

// A small buffer plus no batch size is a configuration that reads as
// reasonable and would not start, because the library default is larger than
// the buffer. The daemon resolves it; an explicit value is left alone.
func TestBatchSizeIsKeptInsideASmallBuffer(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  BufferConfig
		want int
	}{
		{"small buffer, unset batch", BufferConfig{Capacity: 100}, 100},
		{"large buffer, unset batch", BufferConfig{Capacity: 100_000}, 0},
		{"unset buffer, unset batch", BufferConfig{}, 0},
		{"explicit batch is honoured", BufferConfig{Capacity: 100, BatchSize: 16}, 16},
		{"explicit oversized batch is left for core to refuse", BufferConfig{Capacity: 10, BatchSize: 999}, 999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := batchSize(tc.cfg); got != tc.want {
				t.Errorf("batchSize(%+v) = %d, want %d", tc.cfg, got, tc.want)
			}
		})
	}
}
