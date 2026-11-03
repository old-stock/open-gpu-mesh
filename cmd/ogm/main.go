// Command ogm is the single binary for open-gpu-mesh: server, agent and CLI.
package main

import (
	"os"

	"github.com/old-stock/open-gpu-mesh/internal/cli"
)

func main() { cli.Main(os.Args[1:]) }
