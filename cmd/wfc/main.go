// Command wfc is the workflow control-plane CLI client.
//
// It reaches the daemon only through the unix-socket API (decisions.md D3,
// D5); it never opens the database or touches the artifact store's disk
// layout. This is the Phase 1 skeleton: it prints its version and exits.
package main

import (
	"fmt"
	"os"
)

// version is the build identity reported by both binaries. Phase 2 replaces
// this with a real version/health endpoint.
const version = "0.0.0-dev"

func main() {
	fmt.Fprintf(os.Stdout, "wfc %s\n", version)
}
