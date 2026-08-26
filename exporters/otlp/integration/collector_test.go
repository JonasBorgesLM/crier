//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// collectorImage is pinned. An integration suite that follows a moving tag
// fails on someone else's release schedule, and the failure looks like a bug
// in crier.
const collectorImage = "otel/opentelemetry-collector:0.159.0"

// collectorConfig is the smallest pipeline that proves a payload arrived: the
// OTLP HTTP receiver, and the debug exporter writing what it received to
// stdout, which is the container's log.
const collectorConfig = `
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318

exporters:
  debug:
    verbosity: detailed

service:
  telemetry:
    logs:
      level: info
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [debug]
`

// collector is a running OpenTelemetry Collector.
type collector struct {
	t         *testing.T
	container testcontainers.Container
	endpoint  string
}

// startCollector brings up a collector and returns it, skipping the test when
// there is no container runtime to run it on.
func startCollector(t *testing.T) *collector {
	t.Helper()
	return newCollector(t, 0)
}

// startRestartableCollector binds the collector to a stable host port, so it
// can be stopped and started again without the endpoint moving underneath an
// exporter that was configured with it — which is what a destination going
// away and coming back actually looks like.
func startRestartableCollector(t *testing.T) *collector {
	t.Helper()
	return newCollector(t, freePort(t))
}

// newCollector starts the container. hostPort of zero lets Docker choose.
func newCollector(t *testing.T, hostPort int) *collector {
	t.Helper()
	requireDocker(t)

	req := testcontainers.ContainerRequest{
		Image:        collectorImage,
		ExposedPorts: []string{"4318/tcp"},
		Files: []testcontainers.ContainerFile{{
			Reader:            strings.NewReader(collectorConfig),
			ContainerFilePath: "/etc/otelcol/config.yaml",
			FileMode:          0o644,
		}},
		WaitingFor: wait.ForAll(
			wait.ForLog("Everything is ready. Begin running and processing data."),
			wait.ForListeningPort("4318/tcp"),
		).WithDeadline(2 * time.Minute),
	}
	if hostPort != 0 {
		req.HostConfigModifier = func(cfg *dockercontainer.HostConfig) {
			cfg.PortBindings = nat.PortMap{
				"4318/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(hostPort)}},
			}
		}
	}

	ctx := context.Background()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("starting the collector: %v", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("collector host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "4318/tcp")
	if err != nil {
		t.Fatalf("collector port: %v", err)
	}

	c := &collector{
		t:         t,
		container: ctr,
		endpoint:  fmt.Sprintf("http://%s:%s", host, port.Port()),
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := ctr.Terminate(ctx); err != nil {
			t.Logf("terminating the collector: %v", err)
		}
	})
	return c
}

// freePort reserves a port by binding it and letting it go, which is the
// closest thing to an atomic answer the operating system offers.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address %T", l.Addr())
	}
	return addr.Port
}

// requireDocker skips rather than fails when there is no container runtime.
//
// A developer without Docker should see the suite step aside, not a wall of
// red that says nothing about their change. CI has a runtime and runs it for
// real.
func requireDocker(t *testing.T) {
	t.Helper()
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	defer func() { _ = provider.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := provider.Health(ctx); err != nil {
		t.Skipf("container runtime is not healthy: %v", err)
	}
}

// logs returns everything the collector has written so far.
func (c *collector) logs() string {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reader, err := c.container.Logs(ctx)
	if err != nil {
		c.t.Fatalf("reading collector logs: %v", err)
	}
	defer func() { _ = reader.Close() }()

	out, err := io.ReadAll(reader)
	if err != nil {
		c.t.Fatalf("reading collector logs: %v", err)
	}
	return string(out)
}

// waitForLog polls the collector's output until it contains want.
//
// Acceptance is asynchronous — the collector answers 200 and then writes what
// it received — so the assertion has to be "it arrived", not "it arrived
// before this line of the test ran".
func (c *collector) waitForLog(want string) {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(c.logs(), want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("the collector never logged %q", want)
}

// stop kills the collector, so the next export finds nothing listening.
func (c *collector) stop() {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.container.Stop(ctx, nil); err != nil {
		c.t.Fatalf("stopping the collector: %v", err)
	}
}

// start brings a stopped collector back on the same endpoint.
func (c *collector) start() {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.container.Start(ctx); err != nil {
		c.t.Fatalf("restarting the collector: %v", err)
	}
}
