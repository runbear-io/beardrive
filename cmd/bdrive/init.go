package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
	"github.com/runbear-io/beardrive/internal/autostart"
	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/templates"
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
	var projectID, projectName, serverURL, template string
	var only []string
	var yes, foreground, noHooks, noAutostart bool
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
  bdrive init --project 7f3a2c91-4d5e-4b8a-9c17-2ad0f6b3e9c4   # connect an existing project
  bdrive init --project 7f3a2c91-4d5e-4b8a-9c17-2ad0f6b3e9c4 --server https://hub.example.com
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
			// Both refusals land before any network call or file write: a
			// scope that excludes a template's top level would hide it from
			// the whole team (scope rules live in the synced .bdriveignore),
			// and an unknown name should cost nothing.
			var tpl templates.Template
			if template != "" {
				if len(only) > 0 {
					return fmt.Errorf("--template and --only are mutually exclusive: " +
						"scope rules live in the synced .bdriveignore, so a scope that leaves out " +
						"the template's folders would hide them for everyone")
				}
				if tpl, err = templates.Get(template); err != nil {
					return err
				}
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
				// --template in an already-initialized folder is the agent's
				// path: init pulled the project, the folder turned out to be
				// empty, so the structure is written here and the usual cycle
				// pushes it. Existing paths are never overwritten, which is
				// what makes re-running this safe.
				if tpl.Name != "" {
					if err := seedLocally(folder, tpl); err != nil {
						return err
					}
				}
				if !noHooks {
					installAgentHooks(folder)
				}
				if !noAutostart {
					installAutostart()
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

			// Which project — and, when creating one, what does it start from?
			var p serverProject
			var created bool
			switch {
			case projectID != "":
				p, err = getProject(server, settings.Token, projectID)
			case projectName != "":
				p, created, err = createProject(server, settings.Token, projectName, template)
			case interactive:
				p, created, err = chooseProject(server, settings.Token, filepath.Base(folder), &template, &tpl)
			default:
				p, created, err = createProject(server, settings.Token, filepath.Base(folder), template)
			}
			if err != nil {
				return fmt.Errorf("cannot set up project on %s: %w", server, err)
			}
			// A template is applied when a project is created, and only then:
			// joining one that already exists must never restructure it.
			if tpl.Name != "" && !created && p.Template != tpl.Name {
				from := "an empty project"
				if p.Template != "" {
					from = "the " + p.Template + " template"
				}
				return fmt.Errorf("project %q already exists and was created from %s\n"+
					"a template only applies to a new project; connect to this one without --template", p.Name, from)
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
			if tpl.Name != "" {
				switch {
				case p.Template == tpl.Name:
					// The hub seeded it at creation; the initial cycle below
					// is blocking and pulls, so the files land on disk before
					// this command returns.
					fmt.Printf("  start:   %s template (seeded on the hub)\n", tpl.Name)
				default:
					// An older hub silently ignored the template field, which
					// would make --template a quiet no-op. Seed from here and
					// let the first cycle push it.
					if err := seedLocally(folder, tpl); err != nil {
						return err
					}
				}
			}
			if !noHooks {
				installAgentHooks(folder)
			}
			if !noAutostart {
				installAutostart()
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
			// The one and only place the CLI asks for a star. A folder was just
			// set up successfully — about once per project per machine — which
			// is the single moment that earns the ask. Never from a command that
			// repeats (sync, status, the daemon), and never without a TTY: a
			// star plea in a CI log or in output a script parses is exactly what
			// got postinstall ads banned from npm.
			if stdinIsTTY() {
				fmt.Printf("\nif this is useful, a star helps other teams find it: %s\n", repoURL)
			}
			return nil
		},
	}
	c.Flags().StringVar(&serverURL, "server", "", "hub to connect to (default: the remembered one); signs in there if this device has no session")
	c.Flags().StringVar(&projectID, "project", "", "connect an existing project by id (p-xxxxxxxx)")
	c.Flags().StringVar(&projectName, "name", "", "project name to create or join (default: folder name)")
	c.Flags().StringVar(&template, "template", "", "start from a structure ("+strings.Join(templates.Names(), ", ")+"); default: an empty project")
	c.Flags().StringSliceVar(&only, "only", nil, "sync only these subfolders of the mount (comma-separated, e.g. wiki,docs) — written as .bdriveignore rules")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "accept defaults, never prompt")
	c.Flags().BoolVarP(&foreground, "foreground", "f", false, "run the sync daemon in the foreground")
	c.Flags().BoolVar(&noHooks, "no-hooks", false, "skip registering agent sync hooks")
	c.Flags().BoolVar(&noAutostart, "no-autostart", false, "skip registering sync to restart at login")
	return c
}

// seedLocally writes a template into the folder and says what it wrote.
// Paths that already exist are never touched, so seeding twice — an agent
// re-running init, a hub that already seeded — is a no-op rather than a
// conflict.
func seedLocally(folder string, tpl templates.Template) error {
	wrote, err := tpl.WriteTo(folder)
	if err != nil {
		return fmt.Errorf("seed the %s template: %w", tpl.Name, err)
	}
	if len(wrote) == 0 {
		fmt.Printf("  start:   %s template (already present)\n", tpl.Name)
		return nil
	}
	fmt.Printf("  start:   %s template (%d files — read AGENTS.md)\n", tpl.Name, len(wrote))
	return nil
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

// installAutostart registers the login unit so a reboot doesn't quietly stop
// sync. Best effort and one line of output: a platform without one (a BSD, or
// Linux without systemd) or an unwritable config dir is not a reason to fail
// an init that otherwise worked — the folder syncs, it just won't come back by
// itself.
func installAutostart() {
	res, err := autostart.Install()
	if err != nil {
		if !errors.Is(err, autostart.ErrUnsupported) {
			fmt.Printf("  login:   autostart not registered (%v) — run `bdrive resume` after a reboot\n", err)
		}
		return
	}
	state := "autostart registered"
	if !res.Changed {
		state = "autostart already registered"
	}
	fmt.Printf("  login:   %s  →  %s\n", state, res.Path)
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

// chooseProject asks what to do and does it. The starting point is asked
// only on the create-a-new-project branch — connecting to an existing
// project never restructures it — and the picked template is handed back
// through name/tpl so the caller's refusals and summary see it too.
func chooseProject(server, token, defaultName string, name *string, tpl *templates.Template) (serverProject, bool, error) {
	var mode string
	if err := survey.AskOne(&survey.Select{
		Message: "What would you like to do?",
		Options: []string{"Create a new project", "Connect an existing project"},
	}, &mode); err != nil {
		return serverProject{}, false, err
	}
	if mode == "Create a new project" {
		projName := defaultName
		if err := survey.AskOne(&survey.Input{Message: "Project name:", Default: defaultName}, &projName); err != nil {
			return serverProject{}, false, err
		}
		if *name == "" {
			picked, err := chooseTemplate()
			if err != nil {
				return serverProject{}, false, err
			}
			if picked != "" {
				t, err := templates.Get(picked)
				if err != nil {
					return serverProject{}, false, err
				}
				*name, *tpl = picked, t
			}
		}
		p, created, err := createProject(server, token, projName, *name)
		if err == nil && !created {
			fmt.Printf("project %q already exists — connecting to it\n", p.Name)
		}
		return p, created, err
	}
	projects, err := listProjects(server, token)
	if err != nil {
		return serverProject{}, false, err
	}
	if len(projects) == 0 {
		return serverProject{}, false, fmt.Errorf("the server has no projects yet; create one instead")
	}
	labels := make([]string, len(projects))
	for i, p := range projects {
		labels[i] = fmt.Sprintf("%s (%s)", p.Name, p.ID)
	}
	var idx int
	if err := survey.AskOne(&survey.Select{Message: "Connect to which project?", Options: labels}, &idx); err != nil {
		return serverProject{}, false, err
	}
	return projects[idx], false, nil
}

// chooseTemplate offers the three starting points in the same words the web
// dialog uses, recommended first and "empty" as a real option rather than a
// footnote. Returns "" for an empty project.
func chooseTemplate() (string, error) {
	list := templates.List()
	options := make([]string, 0, len(list)+1)
	for i, t := range list {
		label := fmt.Sprintf("%s — %s", t.Title, t.Blurb)
		if i == 0 {
			label += "  (recommended)"
		}
		options = append(options, label)
	}
	options = append(options, "Empty project — just the folder")

	var idx int
	if err := survey.AskOne(&survey.Select{
		Message: "Start from a structure?",
		Options: options,
		Default: options[len(options)-1],
	}, &idx); err != nil {
		return "", err
	}
	if idx == len(list) {
		return "", nil
	}
	return list[idx].Name, nil
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
	// Template is the structure the hub seeded the project from, "" for an
	// empty project — and also for a hub too old to know the field, which is
	// why init falls back to seeding locally rather than trusting it blindly.
	Template string `json:"template"`
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

func createProject(server, token, name, template string) (serverProject, bool, error) {
	body, err := json.Marshal(map[string]string{"name": name, "template": template})
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
