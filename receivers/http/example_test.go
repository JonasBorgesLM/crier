package httpreceiver_test

import (
	"fmt"
	"net/http"

	"github.com/JonasBorgesLM/moat/secret"

	"github.com/JonasBorgesLM/crier/core"
	httpreceiver "github.com/JonasBorgesLM/crier/receivers/http"
)

// Example wires the ingestion endpoint: credentials, the pipeline it feeds,
// and the request-level guards around it.
func Example() {
	buffer, err := core.NewMemoryBuffer(core.MemoryBufferConfig{Capacity: 10_000})
	if err != nil {
		panic(err)
	}
	pipeline, err := core.NewPipeline(core.PipelineConfig{Buffer: buffer})
	if err != nil {
		panic(err)
	}

	// Held masked, never as a plain string (NFR4, IR2).
	auth, err := httpreceiver.NewStaticCredentials(map[string]secret.Value{
		"task-api": secret.New([]byte("ingest-token")),
	})
	if err != nil {
		panic(err)
	}

	receiver, err := httpreceiver.New(httpreceiver.Config{Pipeline: pipeline, Auth: auth})
	if err != nil {
		panic(err)
	}

	// Security headers outermost, so error responses carry them too.
	handler, err := receiver.Handler(httpreceiver.ChainConfig{})
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", handler)

	fmt.Println("serving", httpreceiver.V1.Path())
	fmt.Println("authenticated sources:", auth.Sources())

	// Output:
	// serving /v1/logs
	// authenticated sources: [task-api]
}

// ExampleNewTrustedProxy shows crier behind gateway-auth, where identity comes
// from the gateway's assertion rather than from a credential crier checks
// itself (IR7).
func ExampleNewTrustedProxy() {
	// Strictly opt-in, and the peers have to be named: a header is only
	// identity if the peer that set it is one crier was told to believe.
	proxy, err := httpreceiver.NewTrustedProxy(httpreceiver.TrustedProxyConfig{
		TrustedCIDRs: []string{"10.0.0.0/8"},
	})
	if err != nil {
		panic(err)
	}

	// A set covering the default route is refused, because it would make the
	// identity header forgeable by any client.
	_, err = httpreceiver.NewTrustedProxy(httpreceiver.TrustedProxyConfig{
		TrustedCIDRs: []string{"0.0.0.0/0"},
	})
	fmt.Println("default route accepted:", err == nil)
	fmt.Println("trusted prefixes:", proxy.TrustedPrefixes())

	// Output:
	// default route accepted: false
	// trusted prefixes: [10.0.0.0/8]
}
