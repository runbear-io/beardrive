package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/webapp"
)

// webConfig mirrors the web command's flags so a server can be configured
// from a file (bdrive web -c config.json). Explicitly-passed flags win over
// file values.
type webConfig struct {
	Remote     string `json:"remote,omitempty"`
	Dir        string `json:"dir,omitempty"`
	Addr       string `json:"addr,omitempty"`
	Volume     string `json:"volume,omitempty"`
	Refresh    string `json:"refresh,omitempty"` // duration, e.g. "10s"
	Upload     *bool  `json:"upload,omitempty"`
	UploadTTL  string `json:"upload_ttl,omitempty"`  // duration, e.g. "15m"
	ProjectsDB string `json:"projects_db,omitempty"` // hub project registry path
}

func loadWebConfig(path string) (webConfig, error) {
	var c webConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

func webCmd() *cobra.Command {
	var remoteURL, dir, volume string
	var addr, configPath, projectsDB string
	var refresh, uploadTTL time.Duration
	var upload bool
	c := &cobra.Command{
		Use:   "web [folder | storage-root-url]",
		Short: "Serve the bdrive web server: viewer, uploads, and sync hub",
		Long: `Serve the bdrive web server: browse folders and files, read rendered
markdown (Obsidian-style, including [[wikilinks]]), and download any file.

Two modes:

  - a local folder, served straight from disk (the default — on a mounted
    folder the daemon keeps it fresh, so this is the simplest viewer);
  - a hub: point it at an object-storage root and it hosts many projects,
    each stored under its own prefix, managed by a file-backed project
    registry. Client devices run "bdrive login <this server>" once, then
    "bdrive init" per project, and sync whole folders through it without
    ever seeing the storage location or holding cloud credentials.

The server is read-only unless --upload is set. With uploads on, content
travels directly between clients and the object store through short-lived
presigned URLs when the backend supports it (S3, GCS with signing
credentials); otherwise it is relayed through this server.`,
		Example: `  bdrive web                          # serve the current directory
  bdrive web ./notes                  # serve a folder
  bdrive web -c config.json           # everything from a config file
  bdrive web s3://bucket/root --upload  # multi-project sync hub`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Config file first; flags that were explicitly passed override
			// its values.
			if configPath != "" {
				c, err := loadWebConfig(configPath)
				if err != nil {
					return err
				}
				set := cmd.Flags().Changed
				if c.Remote != "" && !set("remote") {
					remoteURL = c.Remote
				}
				if c.Dir != "" && !set("dir") {
					dir = c.Dir
				}
				if c.Addr != "" && !set("addr") {
					addr = c.Addr
				}
				if c.Volume != "" && !set("volume") {
					volume = c.Volume
				}
				if c.Upload != nil && !set("upload") {
					upload = *c.Upload
				}
				if c.Refresh != "" && !set("refresh") {
					d, err := time.ParseDuration(c.Refresh)
					if err != nil {
						return fmt.Errorf("config refresh: %w", err)
					}
					refresh = d
				}
				if c.UploadTTL != "" && !set("upload-ttl") {
					d, err := time.ParseDuration(c.UploadTTL)
					if err != nil {
						return fmt.Errorf("config upload_ttl: %w", err)
					}
					uploadTTL = d
				}
				if c.ProjectsDB != "" && !set("projects-db") {
					projectsDB = c.ProjectsDB
				}
			}
			// Positional argument: a URL selects remote mode, anything else
			// is a folder. With nothing specified, serve the current dir.
			if remoteURL == "" && dir == "" && len(args) > 0 {
				if strings.Contains(args[0], "://") {
					remoteURL = args[0]
				} else {
					dir = args[0]
				}
			}
			if remoteURL != "" && dir != "" {
				return fmt.Errorf("--remote and --dir are mutually exclusive")
			}
			if remoteURL == "" && dir == "" {
				dir = "."
			}

			srv := &webapp.Server{
				Refresh: refresh,
				Upload:  webapp.UploadConfig{Enabled: upload, TTL: uploadTTL},
			}
			var display string
			if dir != "" {
				// Single-volume viewer over a plain folder.
				abs, err := filepath.Abs(dir)
				if err != nil {
					return err
				}
				if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
					return fmt.Errorf("%s is not a directory", abs)
				}
				srv.Source = &webapp.DirSource{Root: abs}
				srv.Volume = filepath.Base(abs)
				display = abs
			} else {
				// Hub: many projects on one storage root.
				be, err := remote.Open(cmd.Context(), remoteURL)
				if err != nil {
					return err
				}
				defer be.Close()
				if projectsDB == "" {
					home, err := config.Home()
					if err != nil {
						return err
					}
					projectsDB = filepath.Join(home, "projects.json")
				}
				db, err := webapp.OpenProjectDB(projectsDB)
				if err != nil {
					return fmt.Errorf("open project registry: %w", err)
				}
				dev, err := config.LoadDevice()
				if err != nil {
					return fmt.Errorf("load device identity: %w", err)
				}
				srv.Root = be
				srv.Projects = db
				srv.Device = webapp.Identity{ID: dev.ID, Name: dev.Name, Author: dev.Author}
				srv.Volume = volumeName(remoteURL)
				display = remoteURL + " (projects: " + projectsDB + ")"
			}
			if volume != "" {
				srv.Volume = volume
			}

			shown := addr
			if strings.HasPrefix(shown, ":") {
				shown = "localhost" + shown
			}
			fmt.Printf("serving %s\n  volume: %s\n  url:    http://%s\n", display, srv.Volume, shown)
			return http.ListenAndServe(addr, srv.Handler())
		},
	}
	c.Flags().StringVarP(&remoteURL, "remote", "r", "", "remote to serve (s3://bucket/prefix, gs://bucket/prefix, file:///path)")
	c.Flags().StringVar(&dir, "dir", "", "local folder to serve (default: current directory)")
	c.Flags().StringVar(&addr, "addr", ":4173", "address to listen on")
	c.Flags().StringVarP(&volume, "volume", "v", "", "volume display name (default: folder or remote basename)")
	c.Flags().DurationVar(&refresh, "refresh", 10*time.Second, "how long to cache the file listing")
	c.Flags().BoolVar(&upload, "upload", false, "allow clients to upload files")
	c.Flags().DurationVar(&uploadTTL, "upload-ttl", webapp.DefaultUploadTTL, "lifetime of presigned direct-upload URLs")
	c.Flags().StringVarP(&configPath, "config", "c", "", "JSON config file; explicit flags override its values")
	c.Flags().StringVar(&projectsDB, "projects-db", "", "hub project registry file (default: $BDRIVE_HOME/projects.json)")
	return c
}

func volumeName(remoteURL string) string {
	if u, err := url.Parse(remoteURL); err == nil {
		if base := path.Base(strings.Trim(u.Path, "/")); base != "" && base != "." {
			return base
		}
		if u.Host != "" {
			return u.Host
		}
	}
	return "beardrive"
}
