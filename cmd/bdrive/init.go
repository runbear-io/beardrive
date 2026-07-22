package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
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

// initCmd is the front door: sign in if needed, create or connect a project,
// choose what syncs, and start syncing — one command, interactive on a TTY,
// fully flag-driven for scripts and agents. Re-running it in an initialized
// folder just resumes syncing (which is also how a moved/renamed folder
// picks up where it left off).
func initCmd() *cobra.Command {
	var projectID, projectName, shared string
	var yes, foreground bool
	c := &cobra.Command{
		Use:   "init [folder]",
		Short: "Start syncing a project in this folder",
		Long: `Initiate a new project (or connect an existing one) in a folder and start
syncing it through your bdrive server.

On a terminal, init asks what you want: create a new project or connect an
existing one, and whether to sync the whole folder or only a shared
subfolder (e.g. ./shared). Flags answer those questions non-interactively;
without a TTY init never prompts (it creates-or-joins a project named after
the folder and syncs the whole folder).

If this device isn't signed in yet, init runs the login flow first
(default server: ` + config.DefaultServer + `; change it with bdrive login <url>).

Re-running init in an initialized folder resumes syncing — including after
the folder was renamed or moved.`,
		Example: `  bdrive init                        # interactive
  bdrive init ./notes --name shared-notes
  bdrive init --project p-7f3a2c91   # connect an existing project
  bdrive init --shared shared        # only ./shared syncs
  bdrive init --yes                  # accept all defaults (no prompts)`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			if projectID != "" && projectName != "" {
				return fmt.Errorf("--project and --name are mutually exclusive")
			}

			// Already initialized → resume (also self-heals after a move).
			if proj, ok, err := config.ResolveMount(folder); err != nil {
				return err
			} else if ok && proj.Remote != "" {
				fmt.Printf("resuming %s (project %s)\n", folder, proj.Volume)
				return startSync(cmd.Context(), folder, proj, foreground, 3*time.Second, 10*time.Second)
			}

			// Sign in first if this device has no (valid) session.
			settings, err := ensureLogin()
			if err != nil {
				return err
			}
			server := settings.Server

			interactive := stdinIsTTY() && !yes

			// Which project?
			var p serverProject
			switch {
			case projectID != "":
				p, err = getProject(server, settings.Token, projectID)
			case projectName != "":
				p, _, err = createProject(server, settings.Token, projectName)
			case interactive:
				p, err = chooseProject(server, settings.Token, filepath.Base(folder))
			default:
				p, _, err = createProject(server, settings.Token, filepath.Base(folder))
			}
			if err != nil {
				return fmt.Errorf("cannot set up project on %s: %w", server, err)
			}

			// What syncs?
			if shared == "" && interactive && !cmd.Flags().Changed("shared") {
				shared, err = chooseScope()
				if err != nil {
					return err
				}
			}
			var include []string
			if shared != "" {
				shared = strings.Trim(path.Clean(filepath.ToSlash(shared)), "/")
				if shared == "" || shared == "." || strings.HasPrefix(shared, "..") {
					return fmt.Errorf("invalid shared folder %q", shared)
				}
				include = []string{shared + "/"}
				if err := os.MkdirAll(filepath.Join(folder, filepath.FromSlash(shared)), 0o755); err != nil {
					return err
				}
			}

			if err := os.MkdirAll(folder, 0o755); err != nil {
				return err
			}
			proj := config.Project{
				Volume:  p.Name,
				Remote:  server + "/p/" + p.ID,
				Include: include,
			}
			proj, err = config.SaveProject(folder, proj)
			if err != nil {
				return err
			}
			ignorePath := filepath.Join(folder, ".bdriveignore")
			if _, err := os.Stat(ignorePath); os.IsNotExist(err) {
				if err := os.WriteFile(ignorePath, []byte(starterIgnore), 0o644); err != nil {
					return err
				}
			}
			fmt.Printf("initialized %s\n  server:  %s\n  project: %s (%s)\n", folder, server, p.Name, p.ID)
			if shared != "" {
				fmt.Printf("  syncing: ./%s only\n", shared)
			}
			if err := startSync(cmd.Context(), folder, proj, foreground, 3*time.Second, 10*time.Second); err != nil {
				return err
			}
			if foreground {
				return nil // daemon already ran and exited; "syncing automatically" would be false now
			}
			fmt.Printf(`
done — the daemon now keeps this folder in sync automatically.

next steps:
  connect another device or teammate:  bdrive init --project %s
  see who changed what:                bdrive log
  share a file by public URL:          bdrive share <file>
`, p.ID)
			return nil
		},
	}
	c.Flags().StringVar(&projectID, "project", "", "connect an existing project by id (p-xxxxxxxx)")
	c.Flags().StringVar(&projectName, "name", "", "project name to create or join (default: folder name)")
	c.Flags().StringVar(&shared, "shared", "", "sync only this subfolder (e.g. shared)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "accept defaults, never prompt")
	c.Flags().BoolVarP(&foreground, "foreground", "f", false, "run the sync daemon in the foreground")
	return c
}

// ensureLogin returns settings with a working session, running the login
// flow first when there is none (or the token went stale).
func ensureLogin() (config.Settings, error) {
	settings, err := config.LoadSettings()
	if err != nil {
		return settings, err
	}
	server := settings.Server
	if server == "" {
		server = config.DefaultServer
	}
	cfg, err := fetchServerConfig(server)
	if err != nil {
		return settings, fmt.Errorf("cannot reach bdrive server at %s: %w (set one with `bdrive login <url>`)", server, err)
	}
	if !cfg.Auth.Enabled {
		settings.Server = server
		return settings, config.SaveSettings(settings)
	}
	if settings.Token != "" && settings.Server == server {
		if _, err := whoAmIOnServer(server, settings.Token); err == nil {
			return settings, nil
		}
		fmt.Println("session expired — signing in again")
	}
	if err := runLogin(server, cfg, false); err != nil {
		return settings, err
	}
	return config.LoadSettings()
}

func chooseProject(server, token, defaultName string) (serverProject, error) {
	var mode string
	if err := survey.AskOne(&survey.Select{
		Message: "What would you like to do?",
		Options: []string{"Create a new project", "Connect an existing project"},
	}, &mode); err != nil {
		return serverProject{}, err
	}
	if mode == "Create a new project" {
		name := defaultName
		if err := survey.AskOne(&survey.Input{Message: "Project name:", Default: defaultName}, &name); err != nil {
			return serverProject{}, err
		}
		p, created, err := createProject(server, token, name)
		if err == nil && !created {
			fmt.Printf("project %q already exists — connecting to it\n", p.Name)
		}
		return p, err
	}
	projects, err := listProjects(server, token)
	if err != nil {
		return serverProject{}, err
	}
	if len(projects) == 0 {
		return serverProject{}, fmt.Errorf("the server has no projects yet; create one instead")
	}
	labels := make([]string, len(projects))
	for i, p := range projects {
		labels[i] = fmt.Sprintf("%s (%s)", p.Name, p.ID)
	}
	var idx int
	if err := survey.AskOne(&survey.Select{Message: "Connect to which project?", Options: labels}, &idx); err != nil {
		return serverProject{}, err
	}
	return projects[idx], nil
}

// chooseScope returns "" for whole-folder sync, or the shared subfolder.
func chooseScope() (string, error) {
	var mode string
	if err := survey.AskOne(&survey.Select{
		Message: "What should sync?",
		Options: []string{"The whole folder", "Only a shared subfolder"},
	}, &mode); err != nil {
		return "", err
	}
	if mode == "The whole folder" {
		return "", nil
	}
	dir := "shared"
	if err := survey.AskOne(&survey.Input{Message: "Shared subfolder:", Default: "shared"}, &dir); err != nil {
		return "", err
	}
	return dir, nil
}

type serverProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var initClient = &http.Client{Timeout: 10 * time.Second}

// serverDo sends an API request with this device's token attached, and
// turns a 401 into a run-bdrive-login hint.
func serverDo(method, url, token string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := initClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, fmt.Errorf("this server requires sign-in; run `bdrive login`")
	}
	return resp, nil
}

func httpBodyError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
}

func getProject(server, token, id string) (serverProject, error) {
	var p serverProject
	resp, err := serverDo(http.MethodGet, server+"/api/projects/"+url.PathEscape(id), token, nil)
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

func listProjects(server, token string) ([]serverProject, error) {
	resp, err := serverDo(http.MethodGet, server+"/api/projects", token, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpBodyError(resp)
	}
	var out struct {
		Projects []serverProject `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

func createProject(server, token, name string) (serverProject, bool, error) {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return serverProject{}, false, err
	}
	resp, err := serverDo(http.MethodPost, server+"/api/projects", token, body)
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
