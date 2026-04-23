// Command satellites is the satellites-v4 server binary. On boot it prints a
// single identifying line so operators can confirm which binary, version,
// build, and git commit they are running; later stories replace this stub
// with state, MCP, and cron initialisation.
package main

import (
	"fmt"

	"github.com/bobmcallan/satellites/internal/config"
)

func main() {
	fmt.Printf("satellites-server %s\n", config.GetFullVersion())
}
