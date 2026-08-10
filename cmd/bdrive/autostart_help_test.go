package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Windows autostart code exists but cannot run — internal/store and
// internal/daemon do not build for Windows — so no help text may promise it.
// BEA-77: three surfaces gave three different answers to "can my Windows
// teammate join?".
func TestAutostartHelpNamesNoWindows(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range []string{c.Short, c.Long} {
			if strings.Contains(s, "Windows") {
				t.Errorf("%q help mentions Windows: %s", c.Name(), s)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(autostartCmd())
}
