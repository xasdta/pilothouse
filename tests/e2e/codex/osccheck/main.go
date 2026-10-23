// osccheck feeds the exact OSC 7501 sequences Codex 0.164 emits through
// Pilothouse's wrap.OSCScanner and prints what it accepts.
// Run: go run ./tests/e2e/codex/osccheck
package main

import (
	"fmt"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/wrap"
)

func main() {
	seqs := []string{
		"\x1b]7501;state=idle:app=codex\x1b\\",
		"\x1b]7501;state=working:app=codex\x1b\\",
		"\x1b]7501;state=blocked:app=codex:kind=permission\x1b\\",
		"\x1b]7501;state=done:app=codex\x1b\\",
		"\x1b]7501;state=clear\x1b\\",
		"\x1b]7501;state=working;app=claude\x07", // control: ';' form
	}
	for _, s := range seqs {
		var sc wrap.OSCScanner
		var got []api.OSCBody
		sc.Feed([]byte(s), func(b api.OSCBody) { got = append(got, b) })
		fmt.Printf("%-55q -> %+v\n", s, got)
	}
}
