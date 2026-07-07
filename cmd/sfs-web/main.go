// sfs-web serves a read-only website for a folder or an sfs remote: browse
// folders and files, read rendered markdown (Obsidian-style, including
// [[wikilinks]]), and download any file.
//
// Two sources:
//
//   - a local folder, served straight from disk (the default — on an sfs
//     mount the daemon keeps it fresh, so this is the simplest deployment);
//   - an sfs remote, read directly from the object store with per-file
//     provenance from the journals — no mount, daemon, or local state needed.
//
// Examples:
//
//	sfs-web                          # serve the current directory
//	sfs-web ./notes                  # serve a folder
//	sfs-web s3://bucket/prefix       # serve an sfs remote
//	sfs-web --remote gs://bucket/prefix --addr :8080
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/runbear-io/sfs/internal/remote"
	"github.com/runbear-io/sfs/internal/webapp"
)

func main() {
	remoteURL := flag.String("remote", "", "sfs remote to serve (s3://bucket/prefix, gs://bucket/prefix, file:///path)")
	dir := flag.String("dir", "", "local folder to serve (default: current directory)")
	addr := flag.String("addr", ":4173", "address to listen on")
	volume := flag.String("volume", "", "volume display name (default: folder or remote basename)")
	refresh := flag.Duration("refresh", 10*time.Second, "how long to cache the file listing")
	flag.Parse()

	// Positional argument: a URL selects remote mode, anything else is a
	// folder. With nothing specified at all, serve the current directory.
	if *remoteURL == "" && *dir == "" && flag.NArg() > 0 {
		if arg := flag.Arg(0); strings.Contains(arg, "://") {
			*remoteURL = arg
		} else {
			*dir = arg
		}
	}
	if *remoteURL != "" && *dir != "" {
		fmt.Fprintln(os.Stderr, "usage: sfs-web [folder | remote-url]  [--addr :4173]  (--remote and --dir are mutually exclusive)")
		os.Exit(2)
	}
	if *remoteURL == "" && *dir == "" {
		*dir = "."
	}

	ctx := context.Background()
	var src webapp.Source
	var display, name string
	if *dir != "" {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			fmt.Fprintf(os.Stderr, "error: %s is not a directory\n", abs)
			os.Exit(1)
		}
		src = &webapp.DirSource{Root: abs}
		display, name = abs, filepath.Base(abs)
	} else {
		be, err := remote.Open(ctx, *remoteURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		defer be.Close()
		src = &webapp.RemoteSource{Backend: be}
		display, name = *remoteURL, volumeName(*remoteURL)
	}
	if *volume != "" {
		name = *volume
	}
	srv := &webapp.Server{
		Source:  src,
		Remote:  display,
		Volume:  name,
		Refresh: *refresh,
	}

	shown := *addr
	if strings.HasPrefix(shown, ":") {
		shown = "localhost" + shown
	}
	fmt.Printf("sfs-web serving %s\n  volume: %s\n  url:    http://%s\n", display, name, shown)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
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
	return "sfs"
}
