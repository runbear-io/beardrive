package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
)

// The browser login flow: the CLI listens on a random loopback port, opens
// the server's sign-in page with ?redirect= pointing back at that port, and
// the page bounces a one-time code to us once the user is signed in (they
// can sign up right there). We exchange the code for a long-lived device
// token. Headless machines use the code flow instead: show a short code,
// the user approves it at /auth/device from any browser, we poll.

// loginCmd signs this device in to a bdrive web server and remembers it.
// Bare `bdrive login` uses the remembered server, or beardrive.ai.
func loginCmd() *cobra.Command {
	var useDevice, status bool
	c := &cobra.Command{
		Use:   "login [server-url]",
		Short: "Sign this device in to a bdrive web server",
		Long: `Sign this device in to a bdrive web server and remember it (settings.json
under the bdrive home); every later "bdrive init" uses it.

If the server requires accounts, login opens its sign-in page in a browser
(sign up right there if you don't have an account); once you sign in, the
terminal finishes automatically. On headless machines, or with --device,
you are shown a short code to approve from any browser instead.

With no argument the remembered server is used, or ` + config.DefaultServer + `.`,
		Example: `  bdrive login                       # remembered server, or beardrive.ai
  bdrive login https://drive.example.com:4173
  bdrive login --device              # no local browser (SSH)
  bdrive login --status              # show server + signed-in account`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			settings, err := config.LoadSettings()
			if err != nil {
				return err
			}
			if status {
				if settings.Server == "" {
					return fmt.Errorf("no server configured; run `bdrive login`")
				}
				fmt.Println(settings.Server)
				if settings.Token != "" {
					if u, err := whoAmIOnServer(settings.Server, settings.Token); err == nil {
						fmt.Printf("signed in as %s <%s>\n", u.Name, u.Email)
					} else {
						fmt.Println("token no longer valid — run `bdrive login` again")
					}
				}
				return nil
			}
			server := settings.Server
			if len(args) > 0 {
				server = strings.TrimSuffix(args[0], "/")
			}
			if server == "" {
				server = config.DefaultServer
			}
			u, err := url.Parse(server)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("invalid server URL %q (want https://host:port)", server)
			}
			cfg, err := fetchServerConfig(server)
			if err != nil {
				return fmt.Errorf("cannot reach bdrive server at %s: %w", server, err)
			}
			if cfg.Mode != "hub" {
				fmt.Println("note: this server serves a single folder (no projects); `bdrive init` needs a hub (a server started on a storage root)")
			}
			if !cfg.Auth.Enabled {
				settings = config.Settings{Server: server}
				if err := config.SaveSettings(settings); err != nil {
					return err
				}
				fmt.Printf("logged in to %s (no sign-in required by this server)\n", server)
				return nil
			}
			if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
				fmt.Println("warning: signing in over plain http — credentials travel unencrypted; prefer https (reverse proxy or tailscale)")
			}
			return runLogin(server, cfg, useDevice)
		},
	}
	c.Flags().BoolVar(&useDevice, "device", false, "use the code flow instead of a local browser (SSH/headless)")
	c.Flags().BoolVar(&status, "status", false, "show the current server and signed-in account")
	return c
}

// runLogin executes the sign-in flow against a server known to require auth
// and persists server + token + account to settings.
func runLogin(server string, cfg serverConfig, useDevice bool) error {
	loginPath := cfg.Auth.CLILogin
	if loginPath == "" {
		loginPath = "/auth/cli"
	}
	var token string
	var user serverUser
	var err error
	if useDevice {
		token, user, err = deviceCodeLogin(server)
	} else {
		token, user, err = browserLogin(server, loginPath)
	}
	if err != nil {
		return err
	}
	settings := config.Settings{Server: server, Token: token, Email: user.Email, Name: user.Name}
	if err := config.SaveSettings(settings); err != nil {
		return err
	}
	fmt.Printf("logged in to %s as %s <%s>\n", server, user.Name, user.Email)
	return nil
}

type serverUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type serverConfig struct {
	Mode string `json:"mode"`
	Auth struct {
		Enabled  bool   `json:"enabled"`
		CLILogin string `json:"cli_login"`
	} `json:"auth"`
}

func fetchServerConfig(server string) (serverConfig, error) {
	var cfg serverConfig
	resp, err := initClient.Get(server + "/api/config")
	if err != nil {
		return cfg, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cfg, httpBodyError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil || cfg.Mode == "" {
		return cfg, fmt.Errorf("not a bdrive server (bad /api/config)")
	}
	return cfg, nil
}

func whoAmIOnServer(server, token string) (serverUser, error) {
	var u serverUser
	req, err := http.NewRequest(http.MethodGet, server+"/api/auth/me", nil)
	if err != nil {
		return u, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := initClient.Do(req)
	if err != nil {
		return u, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return u, httpBodyError(resp)
	}
	err = json.NewDecoder(resp.Body).Decode(&u)
	return u, err
}

func deviceName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "cli"
}

// browserLogin runs the loopback-callback flow.
func browserLogin(server, loginPath string) (string, serverUser, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", serverUser{}, err
	}
	defer ln.Close()

	var stateBuf [16]byte
	rand.Read(stateBuf[:])
	state := hex.EncodeToString(stateBuf[:])
	redirect := fmt.Sprintf("http://%s/callback", ln.Addr().String())
	loginURL := fmt.Sprintf("%s%s?redirect=%s&state=%s", server, loginPath, redirect, state)

	codeCh := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" || r.URL.Query().Get("state") != state {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><body style="font-family:sans-serif;background:#1e1e24;color:#ddd;text-align:center;padding-top:20vh">
<h2>Signed in ✓</h2><p>You can close this tab and return to the terminal.</p></body>`)
		select {
		case codeCh <- r.URL.Query().Get("code"):
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	fmt.Println("opening your browser to sign in (sign up there if you don't have an account):")
	fmt.Println("  " + loginURL)
	if err := openBrowser(loginURL); err != nil {
		fmt.Println("could not open a browser — open the URL above manually, or rerun with --device")
	}

	select {
	case code := <-codeCh:
		if code == "" {
			return "", serverUser{}, fmt.Errorf("sign-in was rejected")
		}
		return exchangeCode(server, code)
	case <-time.After(5 * time.Minute):
		return "", serverUser{}, errors.New("timed out waiting for the browser sign-in (try `bdrive login --device`)")
	}
}

func exchangeCode(server, code string) (string, serverUser, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "device": deviceName()})
	resp, err := initClient.Post(server+"/api/auth/exchange", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", serverUser{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", serverUser{}, httpBodyError(resp)
	}
	var out struct {
		Token string     `json:"token"`
		User  serverUser `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", serverUser{}, err
	}
	return out.Token, out.User, nil
}

// deviceCodeLogin runs the headless flow: show a code, poll until the user
// approves it from any browser.
func deviceCodeLogin(server string) (string, serverUser, error) {
	body, _ := json.Marshal(map[string]string{"device": deviceName()})
	resp, err := initClient.Post(server+"/api/auth/device/start", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", serverUser{}, err
	}
	var start struct {
		Code      string `json:"code"`
		VerifyURL string `json:"verify_url"`
		Interval  int    `json:"interval"`
	}
	err = json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	if err != nil || start.Code == "" {
		return "", serverUser{}, fmt.Errorf("server did not start a device login: %v", err)
	}
	if start.Interval <= 0 {
		start.Interval = 2
	}
	fmt.Printf("on any signed-in browser, open:\n  %s\nand approve code: %s\n", start.VerifyURL, start.Code)

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(start.Interval) * time.Second)
		body, _ := json.Marshal(map[string]string{"code": start.Code, "device": deviceName()})
		resp, err := initClient.Post(server+"/api/auth/device/poll", "application/json", bytes.NewReader(body))
		if err != nil {
			continue // transient; keep polling
		}
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			return "", serverUser{}, errors.New("the code expired before it was approved")
		}
		var out struct {
			Pending bool       `json:"pending"`
			Token   string     `json:"token"`
			User    serverUser `json:"user"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil || out.Pending {
			continue
		}
		if out.Token != "" {
			return out.Token, out.User, nil
		}
	}
	return "", serverUser{}, errors.New("timed out waiting for approval")
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
