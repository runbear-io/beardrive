package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/webapp"
)

func webCmd() *cobra.Command {
	var remoteURL, dir, volume string
	var addr string
	var refresh time.Duration
	c := &cobra.Command{
		Use:   "web [folder | remote-url]",
		Short: "Serve a read-only web viewer for a folder or a remote",
		Long: `Serve a read-only website: browse folders and files, read rendered
markdown (Obsidian-style, including [[wikilinks]]), and download any file.

Two sources:

  - a local folder, served straight from disk (the default — on a mounted
    folder the daemon keeps it fresh, so this is the simplest deployment);
  - a BearDrive remote, read directly from the object store with per-file
    provenance from the journals — no mount, daemon, or local state needed.`,
		Example: `  bdrive web                          # serve the current directory
  bdrive web ./notes                  # serve a folder
  bdrive web s3://bucket/prefix       # serve a remote
  bdrive web --remote gs://bucket/prefix --addr :8080`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			var src webapp.Source
			var display, name string
			if dir != "" {
				abs, err := filepath.Abs(dir)
				if err != nil {
					return err
				}
				if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
					return fmt.Errorf("%s is not a directory", abs)
				}
				src = &webapp.DirSource{Root: abs}
				display, name = abs, filepath.Base(abs)
			} else {
				be, err := remote.Open(cmd.Context(), remoteURL)
				if err != nil {
					return err
				}
				defer be.Close()
				src = &webapp.RemoteSource{Backend: be}
				display, name = remoteURL, volumeName(remoteURL)
			}
			if volume != "" {
				name = volume
			}
			srv := &webapp.Server{
				Source:  src,
				Remote:  display,
				Volume:  name,
				Refresh: refresh,
			}

			shown := addr
			if strings.HasPrefix(shown, ":") {
				shown = "localhost" + shown
			}
			fmt.Printf("serving %s\n  volume: %s\n  url:    http://%s\n", display, name, shown)
			return http.ListenAndServe(addr, srv.Handler())
		},
	}
	c.Flags().StringVarP(&remoteURL, "remote", "r", "", "remote to serve (s3://bucket/prefix, gs://bucket/prefix, file:///path)")
	c.Flags().StringVar(&dir, "dir", "", "local folder to serve (default: current directory)")
	c.Flags().StringVar(&addr, "addr", ":4173", "address to listen on")
	c.Flags().StringVarP(&volume, "volume", "v", "", "volume display name (default: folder or remote basename)")
	c.Flags().DurationVar(&refresh, "refresh", 10*time.Second, "how long to cache the file listing")
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
