// Command axon builds, deploys and operates app clusters from one descriptor.
//
// Author: Arshpreet Singh Bhatia.
package main

import (
	"os"

	"axon/internal/commands"
)

func main() {
	os.Exit(commands.Registry().Run(os.Args[1:]))
}
