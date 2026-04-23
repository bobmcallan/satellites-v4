// Command satellites-agent is the satellites-v4 worker binary that will pull
// tasks from the queue. On boot it prints a single identifying line; later
// stories replace this stub with the task loop.
package main

import (
	"fmt"

	"github.com/bobmcallan/satellites/internal/config"
)

func main() {
	fmt.Printf("satellites-agent %s\n", config.GetFullVersion())
}
