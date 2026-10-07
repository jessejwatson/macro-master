// Command mm saves and runs macro commands.
package main

import (
	"os"

	"macro-master/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cli.NewApp(version).Run(os.Args[1:]))
}
