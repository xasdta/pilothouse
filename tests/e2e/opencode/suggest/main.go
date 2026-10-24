// Command suggest applies the OpenCode settings suggestion that
// `pilothouse setup` offers (permission bash/edit = "ask"), without the
// rest of setup (which would also offer a login service). The e2e suite
// uses it to check that OpenCode accepts the edited config and asks.
//
// It refuses to run without PILOTHOUSE_USER_HOME, so it can only edit a
// test home.
package main

import (
	"fmt"
	"os"

	"github.com/xasdta/pilothouse/internal/adapters"
	"github.com/xasdta/pilothouse/internal/adapters/opencode"
)

func main() {
	if os.Getenv("PILOTHOUSE_USER_HOME") == "" {
		fmt.Fprintln(os.Stderr, "suggest: set PILOTHOUSE_USER_HOME to a test home")
		os.Exit(2)
	}
	sg := opencode.New().(adapters.Suggester)
	ch, _, _, err := sg.Suggest(adapters.InstallOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "suggest:", err)
		os.Exit(1)
	}
	fmt.Printf("%s (changed=%v)\n%s", ch.Summary, ch.Changed, ch.After)
}
