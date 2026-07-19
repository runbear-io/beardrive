package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/agentskills"
)

// bdrive skill — install the `beardrive` skill into whatever agent platforms
// the user works with (Claude Code, Codex, Gemini CLI, Hermes), so the agent
// can do the setup itself: sign in, `bdrive init`, and — the part people miss
// when they copy commands by hand — `bdrive hooks install`.
func skillCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "skill",
		Short: "Show which AI agent platforms have the beardrive skill installed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(nil)
			if err != nil {
				return err
			}
			detected := map[string]bool{}
			for _, a := range agentskills.Detect(folder) {
				detected[a] = true
			}
			for _, a := range agentskills.Agents {
				state := "not detected"
				if detected[a] {
					state = "detected, skill not installed"
					if agentskills.Installed(a) {
						state = "skill installed"
					}
				}
				fmt.Printf("  %-8s %-32s %s\n", a, state, agentskills.Path(a))
			}
			fmt.Println("\ninstall with: bdrive skill install [--agent claude,codex,gemini,hermes]")
			return nil
		},
	}

	var agentsFlag string
	install := &cobra.Command{
		Use:   "install [folder]",
		Short: "Install the beardrive skill for detected agent platforms (or --agent list)",
		Long: "Writes the beardrive skill to each agent's user-level skills directory\n" +
			"(~/.codex/skills/beardrive/SKILL.md and friends), so the agent knows the\n" +
			"CLI everywhere — ask it to set up a folder and it runs login, init, and\n" +
			"`bdrive hooks install` for you. Re-running refreshes the copy shipped\n" +
			"with this binary.",
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
			results, err := agentskills.Install(folder, agents)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				fmt.Println("no agent platforms detected (looked for .claude/, .codex/, .gemini/, .hermes/ here or in ~)")
				fmt.Println("pick explicitly: bdrive skill install --agent claude,codex,gemini,hermes")
				return nil
			}
			for _, r := range results {
				state := "already current"
				if r.Changed {
					state = "installed"
				}
				fmt.Printf("  %-8s %s  →  %s\n", r.Agent, state, r.Path)
			}
			fmt.Println("\nnow ask your agent to set up the folder — it will run init and register sync hooks")
			return nil
		},
	}
	install.Flags().StringVar(&agentsFlag, "agent", "auto", "comma-separated platforms (claude,codex,gemini,hermes) or auto")
	c.AddCommand(install)
	return c
}
