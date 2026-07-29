package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// `bdrive hook-approve` answers a Claude Code PreToolUse hook: it auto-approves
// the handful of bdrive subcommands onboarding runs, so setting up a project is
// not a permission gauntlet. It exists because a plugin cannot ship permission
// rules — a PreToolUse hook returning permissionDecision is the supported way.
//
// The grant is deliberately narrow. Only the onboarding subcommands qualify,
// only when the command is a bare `bdrive …` invocation: anything with a shell
// operator, a redirect, or a substitution falls through to the normal
// permission prompt, so `bdrive sync && rm -rf /` can never ride along.
// Silence means "no opinion" — the user is asked as usual.
var approvedSubcommands = map[string]bool{
	"init": true, "login": true, "hooks": true,
	"status": true, "sync": true, "url": true,
}

// shellMeta are the characters that turn one command into several, or let it
// read/write somewhere unexpected. Their presence disqualifies the whole line.
const shellMeta = "&|;<>`$(){}\n\\"

func hookApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook-approve",
		Short:  "PreToolUse hook: auto-approve beardrive's own onboarding commands",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, _ := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
			var event struct {
				ToolName  string `json:"tool_name"`
				ToolInput struct {
					Command string `json:"command"`
				} `json:"tool_input"`
			}
			if err := json.Unmarshal(data, &event); err != nil {
				return nil // unparseable: no opinion
			}
			if event.ToolName != "Bash" || !approvedCommand(event.ToolInput.Command) {
				return nil
			}
			out, err := json.Marshal(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":            "PreToolUse",
					"permissionDecision":       "allow",
					"permissionDecisionReason": "beardrive setup command (allowed by the beardrive plugin)",
				},
			})
			if err != nil {
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(out))
			return nil
		},
	}
}

// approvedCommand reports whether a shell command is a bare invocation of one
// of beardrive's own onboarding subcommands.
func approvedCommand(command string) bool {
	if strings.ContainsAny(command, shellMeta) {
		return false
	}
	fields := strings.Fields(command)
	if len(fields) < 2 {
		return false
	}
	if bin := filepath.Base(fields[0]); bin != "bdrive" {
		return false
	}
	// Skip flags to find the subcommand: `bdrive --foo status` still counts.
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") {
			continue
		}
		return approvedSubcommands[f]
	}
	return false
}
