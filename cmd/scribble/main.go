// Command scribble is the scribble service and its control CLI. All behaviour
// lives in internal/cli.
package main

import (
	"os"

	"github.com/kpenfound/scribble/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
