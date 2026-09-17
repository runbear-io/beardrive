package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
)

// mcpCmd manages the agents connected to your hub over MCP.
//
// Connecting happens in a browser — the consent screen is where a human picks
// which projects an agent may touch, and that is deliberately not scriptable.
// Disconnecting is the opposite: it is the thing you want to do quickly, from
// wherever you are, possibly because something is wrong. A revocation that
// needs a browser session and a hand-written curl is a security control
// without a surface.
func mcpCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Manage agents connected to your hub over MCP",
		Long: `List and revoke the MCP connections on your account.

An MCP connection lets an agent — Claude, ChatGPT, Cursor, anything that
speaks MCP — read and write the projects you granted it, acting as you.
Connections are created in a browser (the hub's consent screen is where you
tick which projects to share); this command is how you see and remove them.`,
		Example: `  bdrive mcp list
  bdrive mcp revoke mcpg_1a2b3c4d`,
	}
	c.AddCommand(mcpListCmd(), mcpRevokeCmd())
	return c
}

func mcpListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "Show the agents connected to your account",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := config.LoadSettings()
			if err != nil {
				return err
			}
			grants, err := fetchGrants(settings)
			if err != nil {
				return err
			}
			if len(grants) == 0 {
				fmt.Println("no agents are connected to your account")
				return nil
			}
			for _, g := range grants {
				name := g.ClientName
				if name == "" {
					name = "(unnamed client)"
				}
				fmt.Printf("%s  %s\n", g.ID, name)
				fmt.Printf("    projects: %s\n", strings.Join(g.Projects, ", "))
				fmt.Printf("    connected %s", g.Created.Local().Format("2006-01-02 15:04"))
				if !g.LastUsed.IsZero() {
					fmt.Printf(", last used %s", humanAgo(g.LastUsed))
				}
				fmt.Println()
			}
			return nil
		},
	}
}

func mcpRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <connection-id>",
		Short: "Disconnect an agent immediately",
		Long: `Revoke one MCP connection. The agent's current token stops working on its
next call — not at some later expiry — and its refresh token dies with it.

Run 'bdrive mcp list' to see connection ids.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := config.LoadSettings()
			if err != nil {
				return err
			}
			server, err := hubServer(settings)
			if err != nil {
				return err
			}
			resp, err := serverDo(http.MethodDelete,
				server+"/api/mcp/grants/"+url.PathEscape(args[0]), settings.Token, nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return fmt.Errorf("no connection %q on your account (run `bdrive mcp list`)", args[0])
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return httpBodyError(resp)
			}
			fmt.Printf("revoked %s\n", args[0])
			return nil
		},
	}
}

// mcpGrant mirrors the hub's grant JSON. Digests are never sent, so there is
// nothing here to keep secret.
type mcpGrant struct {
	ID         string    `json:"id"`
	ClientName string    `json:"client_name"`
	Projects   []string  `json:"projects"`
	Created    time.Time `json:"created"`
	LastUsed   time.Time `json:"last_used"`
}

func fetchGrants(settings config.Settings) ([]mcpGrant, error) {
	server, err := hubServer(settings)
	if err != nil {
		return nil, err
	}
	resp, err := serverDo(http.MethodGet, server+"/api/mcp/grants", settings.Token, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s does not serve MCP (the hub's config has no `mcp` block)", server)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, httpBodyError(resp)
	}
	var out struct {
		Grants []mcpGrant `json:"grants"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Grants, nil
}

// hubServer is the signed-in hub, with the error a signed-out user needs.
func hubServer(settings config.Settings) (string, error) {
	if settings.Token == "" || settings.Server == "" {
		return "", fmt.Errorf("not signed in — run `bdrive login` first")
	}
	return strings.TrimSuffix(settings.Server, "/"), nil
}

func humanAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
