// Command voxen is the user-facing CLI.
//
//	voxen init        create identity and configuration
//	voxen start       start the background daemon
//	voxen status      inspect daemon state
//
// The CLI is a thin client: it never opens audio devices or reads private keys
// while the daemon is running.
package main

import (
	"os"

	"github.com/s4chdev/voxen/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr, os.Stdin))
}
