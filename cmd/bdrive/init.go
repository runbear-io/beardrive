package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/agenthooks"
	"github.com/runbear-io/beardrive/internal/agentskills"
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
	var projectID, projectName, serverURL string
	var only []string
	var yes, foreground, noHooks bool
	c := &cobra.Command{
		Use:   "init [folder]",
		Short: "Start syncing a project in this folder",
		Long: `Initiate a new project (or connect an existing one) in a folder and start
syncing it through your bdrive server.

The mount is always exactly the folder you name: ` + "`bdrive init wiki`" + ` syncs
./wiki and nothing else, and that folder's contents are the project's
contents. To sync a folder but hold back parts of it, use --only (or
` + "`bdrive scope`" + ` later), which writes ordinary .bdriveignore rules — there is
no separate scope setting.

On a terminal, init asks what you want: create a new project or connect an
existing one, and whether to sync the whole folder or only some subfolders.
Flags answer those questions non-interactively; without a TTY init never
prompts (it creates-or-joins a project named after the folder and syncs the
whole folder).

If this device isn't signed in yet, init runs the login flow first
(default server: ` + config.DefaultServer + `; change it with bdrive login <url>).

Re-running init in an initialized folder resumes syncing — including after
the folder was renamed or moved.`,
		Example: `  bdrive init                        # interactive
  bdrive init wiki                   # ./wiki is the project
  bdrive init ./notes --name shared-notes
  bdrive init --project p-7f3a2c91   # connect an existing project
  bdrive init --project p-7f3a2c91 --server https://hub.example.com
  bdrive init . --only wiki,docs     # this folder, but only ./wiki and ./docs sync
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
				// --only on an existing mount narrows it in place: the scope is
				// just .bdriveignore rules, so re-running init is a legitimate
				// way to set them (and is what `bdrive scope` points at).
				if cmd.Flags().Changed("only") {
					scope, err := cleanScopeDirs(only)
					if err != nil {
						return err
					}
					for _, dir := range scope {
						if err := os.MkdirAll(filepath.Join(folder, filepath.FromSlash(dir)), 0o755); err != nil {
							return err
						}
					}
					if err := writeScopeDirs(folder, scope); err != nil {
						return err
					}
					if len(scope) == 0 {
						fmt.Println("  syncing: the whole folder (scope rules removed)")
					} else {
						fmt.Printf("  syncing: ./%s only (rules written to .bdriveignore)\n", strings.Join(scope, ", ./"))
					}
				}
				installSkill(folder)
				if !noHooks {
					installAgentHooks(folder)
				}
				return startSync(cmd.Context(), folder, proj, foreground, 3*time.Second, 10*time.Second)
			}

			// Sign in first if this device has no (valid) session.
			settings, err := ensureLogin(serverURL)
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
			if err := checkNotAlreadyMounted(server+"/p/"+p.ID, folder, p.Name); err != nil {
				return err
			}

			// All of this folder, or only some of it?
			if len(only) == 0 && interactive && !cmd.Flags().Changed("only") {
				only, err = chooseScope()
				if err != nil {
					return err
				}
			}
			scope, err := cleanScopeDirs(only)
			if err != nil {
				return err
			}
			for _, dir := range scope {
				if err := os.MkdirAll(filepath.Join(folder, filepath.FromSlash(dir)), 0o755); err != nil {
					return err
				}
			}

			if err := os.MkdirAll(folder, 0o755); err != nil {
				return err
			}
			proj := config.Project{
				Volume: p.Name,
				Remote: server + "/p/" + p.ID,
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
			if len(scope) > 0 {
				if err := writeScopeDirs(folder, scope); err != nil {
					return err
				}
			}
			fmt.Printf("initialized %s\n  server:  %s\n  project: %s (%s)\n", folder, server, p.Name, p.ID)
			installSkill(folder)
			if !noHooks {
				installAgentHooks(folder)
			}
			if len(scope) > 0 {
				dirs := make([]string, len(scope))
				for i, d := range scope {
					dirs[i] = "./" + d
				}
				fmt.Printf("  syncing: %s only (rules written to .bdriveignore)\n", strings.Join(dirs, ", "))
			}
			if err := startSync(cmd.Context(), folder, proj, foreground, 3*time.Second, 10*time.Second); err != nil {
				return err
			}
			if foreground {
				return nil // daemon already ran and exited; "syncing automatically" would be false now
			}
			fmt.Printf(`
done — the daemon now keeps this folder in sync automatically.

  your project:  %s/%s   (sign-in required; `+"`bdrive url <file>`"+` links any file)

next steps:
  connect another device or teammate:  bdrive init --project %s
  see who changed what:                bdrive log
  share a file by public URL:          bdrive share <file>
`, server, p.ID, p.ID)
			return nil
		},
	}
	c.Flags().StringVar(&serverURL, "server", "", "hub to connect to (default: the remembered one); signs in there if this device has no session")
	c.Flags().StringVar(&projectID, "project", "", "connect an existing project by id (p-xxxxxxxx)")
	c.Flags().StringVar(&projectName, "name", "", "project name to create or join (default: folder name)")
	c.Flags().StringSliceVar(&only, "only", nil, "sync only these subfolders of the mount (comma-separated, e.g. wiki,docs) — written as .bdriveignore rules")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "accept defaults, never prompt")
	c.Flags().BoolVarP(&foreground, "foreground", "f", false, "run the sync daemon in the foreground")
	c.Flags().BoolVar(&noHooks, "no-hooks", false, "skip registering agent sync hooks")
	return c
}

// installAgentHooks registers turn-boundary sync hooks as part of init, so
// the one command a user (or their agent) already ran covers hooks too — a
// separate `bdrive hooks install` is one more permission prompt, and it is
// exactly the command agent permission layers flag.
//
// The hooks go in each platform's USER config, once per machine: platforms
// read hook config only from the directory a session starts in, so a
// per-project file would cover only the sessions that happen to start there
// (and, living inside a mount, would sync to the whole team).
// Idempotent; failure never fails init (sync is already up).
func installAgentHooks(folder string) {
	results, err := agenthooks.Install(folder, nil)
	if err != nil {
		fmt.Printf("  hooks:   %v — run `bdrive hooks install` to retry\n", err)
		return
	}
	for _, r := range results {
		state := "hooks registered"
		if !r.Changed {
			state = "hooks already registered"
		}
		fmt.Printf("  %-8s %s  →  %s\n", r.Agent, state, r.Path)
		if r.Migrated != "" {
			fmt.Printf("           moved out of %s (project hooks are no longer used)\n", r.Migrated)
		}
		if r.Note != "" {
			fmt.Printf("           note: %s\n", r.Note)
		}
	}
}

// installSkill teaches every detected platform the bdrive CLI, so later
// sessions are conversational. Folded into init deliberately: a separate
// `bdrive skill install` is another approval prompt for no decision.
func installSkill(folder string) {
	results, err := agentskills.Install(folder, nil)
	if err != nil {
		return // best effort: the skill is a convenience, not a requirement
	}
	var installed []string
	for _, r := range results {
		if r.Changed {
			installed = append(installed, r.Agent)
		}
	}
	if len(installed) > 0 {
		fmt.Printf("  skill:   installed for %s\n", strings.Join(installed, ", "))
	}
}

// checkNotAlreadyMounted refuses a second folder for a project this device
// already syncs. Each device writes one journal per project on the remote, so
// two local mounts of one project are two writers of the same journal: the
// second one's push overwrites the first one's ops and those files disappear
// from the hub. Stale registry entries (folder gone) are ignored.
func checkNotAlreadyMounted(remote, folder, name string) error {
	mounts, err := config.LoadMounts()
	if err != nil {
		return nil // registry unreadable: don't block setup over it
	}
	for _, mi := range mounts {
		if mi.Remote != remote || mi.Path == folder || mi.Path == "" {
			continue
		}
		if !config.IsMount(mi.Path) {
			continue // moved or deleted; the registry entry is stale
		}
		return fmt.Errorf("this device already syncs project %q at %s\n"+
			"one folder per project per device: a second mount would overwrite that folder's history on the hub\n"+
			"use that folder, or release it first with `bdrive stop --forget %s`", name, mi.Path, mi.Path)
	}
	return nil
}

func installHooksIn(folder string) {
	results, err := agenthooks.Install(folder, nil)
	if err != nil {
		fmt.Printf("  hooks:   %v — run `bdrive hooks install` to retry\n", err)
		return
	}
	if len(results) == 0 {
		return
	}
	for _, r := range results {
		state := "hooks registered"
		if !r.Changed {
			state = "hooks already registered"
		}
		fmt.Printf("  %-8s %s  →  %s\n", r.Agent, state, r.Path)
		if r.Note != "" {
			fmt.Printf("           note: %s\n", r.Note)
		}
	}
}

// ensureLogin returns settings with a working session, running the login
// flow first when there is none (or the token went stale). A non-empty
// wantServer targets that hub — the reason `bdrive init --server <url>`
// exists at all: without it, connecting to a hub this device has never seen
// takes a separate `bdrive login <url>` first, and every extra command is
// another permission prompt for whoever is driving.
func ensureLogin(wantServer string) (config.Settings, error) {
	settings, err := config.LoadSettings()
	if err != nil {
		return settings, err
	}
	server := settings.Server
	if wantServer != "" {
		server = normalizeServer(wantServer)
	}
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
	prev := settings.Server
	settings.Server = server
	switch {
	case settings.Token != "" && prev == server:
		if _, err := whoAmIOnServer(server, settings.Token); err == nil {
			return settings, nil
		}
		fmt.Println("session expired — signing in again")
	case prev != "" && prev != server:
		fmt.Printf("signing in to %s (this device was signed in to %s)\n", server, prev)
	}
	if err := runLogin(server, cfg, false); err != nil {
		return settings, err
	}
	return config.LoadSettings()
}

// normalizeServer accepts what people (and agents) actually type: a bare
// host, with or without a port, keeps working instead of failing and being
// retried. Anything already carrying a scheme is left alone.
func normalizeServer(raw string) string {
	raw = strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if strings.Contains(raw, "://") {
		return raw
	}
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	// Local hubs are plain http; anything else on the internet is https.
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]" {
		return "http://" + raw
	}
	return "https://" + raw
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

// chooseScope returns nil for whole-folder sync, or the subfolders to narrow
// to (written as .bdriveignore rules, not a separate setting).
func chooseScope() ([]string, error) {
	var mode string
	if err := survey.AskOne(&survey.Select{
		Message: "What should sync?",
		Options: []string{"The whole folder", "Only some subfolders"},
	}, &mode); err != nil {
		return nil, err
	}
	if mode == "The whole folder" {
		return nil, nil
	}
	dirs := "shared"
	if err := survey.AskOne(&survey.Input{Message: "Subfolder(s) to sync, space- or comma-separated:", Default: "shared"}, &dirs); err != nil {
		return nil, err
	}
	return strings.Fields(strings.ReplaceAll(dirs, ",", " ")), nil
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
