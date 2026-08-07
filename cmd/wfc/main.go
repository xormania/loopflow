// Command wfc is the workflow control-plane command line.
//
// It opens the SQLite state directly — there is no server to start first.
package main

import (
	"context"
	"os"

	"github.com/xormania/wfc/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
