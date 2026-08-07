// Command flowd is the workflow control-plane server.
//
// It is the sole owner of the control-plane SQLite database (decisions.md D2)
// and serves HTTP/1.1 + JSON over a unix domain socket (D3). Neither the
// listener nor the state root exists yet: this is the Phase 1 skeleton, whose
// only job is to compile and exit cleanly.
package main

import (
	"fmt"
	"os"
)

// version is the build identity reported by both binaries. Phase 2 replaces
// this with a real version/health endpoint.
const version = "0.0.0-dev"

func main() {
	// Phase 1 wires this to store.Open + migrate; Phase 2 adds the socket
	// listener, single-instance lock, and graceful shutdown.
	fmt.Fprintf(os.Stdout, "flowd %s (skeleton: no listener, no store)\n", version)
}
