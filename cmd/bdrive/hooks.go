package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/agenthooks"
)

// bdrive hooks — register BearDrive's turn-boundary sync hooks with whatever
// AI agent platforms the user works with (Claude Code, Codex, Gemini CLI,
// Hermes). One command instead of hand-editing four config formats; the
// beardrive skill runs it right after `bdrive init`.
func hooksCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hooks",
		Short: "Show which AI agent platforms have beardrive sync hooks registered",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(nil)
			if err != nil {
				return err
			}
			detected := map[string]bool{}
			for _, a := range agenthooks.Detect(folder) {
				detected[a] = true
			}
			for _, a := range agenthooks.Agents {
				state := "not detected"
				if detected[a] {
					state = "detected, hooks not registered"
					if agenthooks.Registered(folder, a) {
						state = "hooks registered"
					}
				}
				fmt.Printf("  %-8s %-32s %s\n", a, state, agenthooks.ConfigPath(folder, a))
			}
			fmt.Println("\nhooks are registered once per machine, in each platform's own user config —")
			fmt.Println("they cover every session in every folder, and no project file is written.")
			if detected["codex"] {
				fmt.Println("\ncodex: hooks are experimental and off by default — enable them with")
				fmt.Println("  [features] codex_hooks = true   in ~/.codex/config.toml")
			}
			fmt.Println("\nregister with:   bdrive hooks install [--agent claude,codex,gemini,hermes]")
			fmt.Println("remove with:     bdrive hooks uninstall")
			return nil
		},
	}

	var agentsFlag string
	install := &cobra.Command{
		Use:   "install [folder]",
		Short: "Register sync hooks for detected agent platforms (or --agent list)",
		Long: "Registers beardrive's sync hooks with each agent platform's own hook\n" +
			"config: files pull before every turn and push after edits, and changes\n" +
			"are stamped with the agent session that made them (`bdrive sync --note`).\n" +
			"Merging is idempotent and preserves hooks you already have.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			var agents []string
			if agentsFlag != "" && agentsFlag != "auto" {
				agents = strings.Split(agentsFlag, ",")
			}
			results, err := agenthooks.Install(folder, agents)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				fmt.Println("no agent platforms detected (looked for .claude/, .codex/, .gemini/ here or in ~; ~/.hermes/)")
				fmt.Println("pick explicitly: bdrive hooks install --agent claude,codex,gemini,hermes")
				return nil
			}
			for _, r := range results {
				state := "already registered"
				if r.Changed {
					state = "registered"
				}
				fmt.Printf("  %-8s %s  →  %s\n", r.Agent, state, r.Path)
				if r.Migrated != "" {
					fmt.Printf("           moved out of %s (project hooks are no longer used)\n", r.Migrated)
				}
				if r.Note != "" {
					fmt.Printf("           note: %s\n", r.Note)
				}
			}
			return nil
		},
	}
	install.Flags().StringVar(&agentsFlag, "agent", "auto", "comma-separated platforms (claude,codex,gemini,hermes) or auto")

	var uninstallAgents string
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove beardrive's sync hooks from every agent platform",
		Long: "Removes only beardrive's own hook entries from each platform's user\n" +
			"config, leaving any other hooks in those files untouched. Syncing itself\n" +
			"is unaffected — the daemon keeps running; only turn-boundary sync stops.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var agents []string
			if uninstallAgents != "" && uninstallAgents != "auto" {
				agents = strings.Split(uninstallAgents, ",")
			}
			results, err := agenthooks.Uninstall(agents)
			if err != nil {
				return err
			}
			removed := 0
			for _, r := range results {
				if r.Changed {
					removed++
					fmt.Printf("  %-8s removed  →  %s\n", r.Agent, r.Path)
				}
			}
			if removed == 0 {
				fmt.Println("no beardrive hooks were registered")
			}
			return nil
		},
	}
	uninstall.Flags().StringVar(&uninstallAgents, "agent", "auto", "comma-separated platforms (claude,codex,gemini,hermes) or auto")
	c.AddCommand(install, uninstall)
	return c
}
