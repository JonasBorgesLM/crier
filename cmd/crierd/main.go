// Command crierd is crier's standalone log-shipping daemon.
//
// It is a thin shell over core (FR9): it assembles receivers, the pipeline,
// and exporters from configuration and owns process lifecycle. Anything worth
// testing belongs in core, not here.
//
// Implementation is tracked in milestone M4.
package main

import (
	"fmt"
	"log"
	"os"
)

// version is overwritten at build time via -ldflags.
var version = "dev"

func main() {
	log.SetFlags(0)
	log.SetPrefix("crierd: ")

	if _, err := fmt.Fprintf(os.Stdout, "crierd %s\n", version); err != nil {
		log.Fatalf("writing version: %v", err)
	}

	log.Fatal("not implemented yet — see milestone M4")
}
