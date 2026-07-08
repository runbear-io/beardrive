package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
)

// starterIgnore is seeded into new projects so build artifacts and
// dependency trees don't flood the sync. Users edit it freely; it syncs to
// every device like a normal file.
const starterIgnore = `# bdrive ignore rules (gitignore-style). This file syncs across devices.
node_modules/
dist/
build/
target/
out/
coverage/
__pycache__/
*.pyc
.venv/
venv/
.next/
.cache/
.DS_Store
*.log
.env
.env.*
`

// loginCmd points this device at a bdrive web server: verify it, remember it
// as the device default. Every later `bdrive init` uses it.
func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login [server-url]",
		Short: "Set this device's bdrive web server",
		Long: `Set the bdrive web server this device syncs through. The URL is verified
and remembered (settings.json under the bdrive home); every later
"bdrive init" uses it. With no argument, show the current server.`,
		Example: `  bdrive login https://drive.example.com:4173
  bdrive login                       # show the current server`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := config.LoadSettings()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				if settings.Server == "" {
					return fmt.Errorf("no server configured; run `bdrive login https://host:4173`")
				}
				fmt.Println(settings.Server)
				return nil
			}
			raw := args[0]
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("invalid server URL %q (want https://host:port)", raw)
			}
			server := strings.TrimSuffix(raw, "/")
			// Prove the URL actually points at a bdrive server before
			// remembering it.
			if err := checkServer(server); err != nil {
				return fmt.Errorf("cannot reach bdrive server at %s: %w", server, err)
			}
			settings.Server = server
			if err := config.SaveSettings(settings); err != nil {
				return err
			}
			fmt.Printf("logged in to %s (default server for `bdrive init`)\n", server)
			return nil
		},
	}
}

// initCmd is the one-command onboarding: join or create a project on the
// logged-in server and start syncing this folder. The client device is
// storage-blind — it learns everything from the server and never holds
// bucket URLs or cloud credentials.
func initCmd() *cobra.Command {
	var projectID, projectName string
	c := &cobra.Command{
		Use:   "init [folder]",
		Short: "Start syncing a project folder through your bdrive server",
		Long: `Set up a folder to sync through this device's bdrive web server (set once
with "bdrive login"), then mount it.

Project resolution on the server:

  - bare init          create-or-join a project named after the folder
  - --project <id>     join an existing project by id
  - --name <name>      create-or-join a project with an explicit name

init writes the folder's .bdrive (project id + server), seeds a starter
.bdriveignore if none exists, and mounts the folder — a background daemon
starts syncing immediately.`,
		Example: `  bdrive init                        # this folder, project named after it
  bdrive init ./notes --name shared-notes
  bdrive init --project p-7f3a2c91   # join an existing project`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			if projectID != "" && projectName != "" {
				return fmt.Errorf("--project and --name are mutually exclusive")
			}

			settings, err := config.LoadSettings()
			if err != nil {
				return err
			}
			server := settings.Server
			if server == "" {
				return fmt.Errorf("no server configured; run `bdrive login https://host:4173` first")
			}

			// Refuse to silently repoint a folder that already belongs
			// somewhere else.
			proj, hasProj, err := config.LoadProject(folder)
			if err != nil {
				return err
			}
			if hasProj && proj.Remote != "" && !strings.HasPrefix(proj.Remote, server+"/p/") {
				return fmt.Errorf("%s already syncs with %s; use `bdrive remote set` to repoint it",
					filepath.Join(folder, config.ProjectFile), proj.Remote)
			}

			// Resolve the project on the server.
			var p serverProject
			switch {
			case projectID != "":
				p, err = getProject(server, projectID)
			default:
				name := projectName
				if name == "" {
					name = filepath.Base(folder)
				}
				var created bool
				p, created, err = createProject(server, name)
				if err == nil && created {
					fmt.Printf("created project %q (%s) on %s\n", p.Name, p.ID, server)
				}
			}
			if err != nil {
				return fmt.Errorf("cannot set up project on %s: %w", server, err)
			}

			if err := os.MkdirAll(folder, 0o755); err != nil {
				return err
			}
			proj.Volume, proj.Remote = p.Name, server+"/p/"+p.ID
			if err := config.SaveProject(folder, proj); err != nil {
				return err
			}
			ignorePath := filepath.Join(folder, ".bdriveignore")
			if _, err := os.Stat(ignorePath); os.IsNotExist(err) {
				if err := os.WriteFile(ignorePath, []byte(starterIgnore), 0o644); err != nil {
					return err
				}
				fmt.Printf("seeded %s (edit freely; it syncs like a normal file)\n", ignorePath)
			}
			fmt.Printf("initialized %s\n  server:  %s\n  project: %s (%s)\n", folder, server, p.Name, p.ID)

			// Mount: initial cycle + background daemon. Same as `bdrive mnt`.
			return runMount(cmd.Context(), folder, "", "", false, 3*time.Second, 10*time.Second)
		},
	}
	c.Flags().StringVar(&projectID, "project", "", "join an existing project by id (p-xxxxxxxx)")
	c.Flags().StringVar(&projectName, "name", "", "project name to create or join (default: folder name)")
	return c
}

type serverProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var initClient = &http.Client{Timeout: 10 * time.Second}

// checkServer verifies the URL answers like a bdrive server.
func checkServer(server string) error {
	resp, err := initClient.Get(server + "/api/config")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpBodyError(resp)
	}
	var cfg struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil || cfg.Mode == "" {
		return fmt.Errorf("not a bdrive server (bad /api/config)")
	}
	if cfg.Mode != "hub" {
		fmt.Println("note: this server serves a single folder (no projects); `bdrive init` needs a hub (a server started on a storage root)")
	}
	return nil
}

func httpBodyError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
}

func getProject(server, id string) (serverProject, error) {
	var p serverProject
	resp, err := initClient.Get(server + "/api/projects/" + url.PathEscape(id))
	if err != nil {
		return p, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p, httpBodyError(resp)
	}
	err = json.NewDecoder(resp.Body).Decode(&p)
	return p, err
}

func createProject(server, name string) (serverProject, bool, error) {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return serverProject{}, false, err
	}
	resp, err := initClient.Post(server+"/api/projects", "application/json", bytes.NewReader(body))
	if err != nil {
		return serverProject{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return serverProject{}, false, httpBodyError(resp)
	}
	var out struct {
		Project serverProject `json:"project"`
		Created bool          `json:"created"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return serverProject{}, false, fmt.Errorf("not a bdrive server (bad response): %w", err)
	}
	return out.Project, out.Created, nil
}
