// Command renderplist prints the launchd plist / systemd unit that
// `pilothouse service install` would write, without touching any service
// manager. Usage: go run ./tests/e2e/reliability/renderplist /abs/path/to/pilothouse
package main

import (
	"fmt"
	"os"

	"github.com/xasdta/pilothouse/internal/service"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: renderplist /abs/path/to/pilothouse")
		os.Exit(2)
	}
	m := service.New(os.Args[1])
	b, err := m.Render()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "would install at:", m.Path())
	os.Stdout.Write(b)
}
