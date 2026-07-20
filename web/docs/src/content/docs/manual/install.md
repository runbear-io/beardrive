---
title: Install the CLI
description: Install the bdrive binary with Homebrew, go install, or a release tarball — for setting a folder up by hand, or for running a hub.
---

Most people never run this. [Setting up through your agent](/start/setup/)
installs the binary as its first step.

Install it yourself when you would rather drive the setup by hand, when you are
[running a hub](/self-hosting/run-a-hub/), or when a machine has no agent on it.

BearDrive ships one binary, `bdrive`. It is the CLI, the sync daemon, and the
web server. macOS and Linux.

## Homebrew

```sh
brew install runbear-io/tap/beardrive
```

Works on macOS and Linuxbrew.

## From source

```sh
go install github.com/runbear-io/beardrive/cmd/bdrive@latest
```

The frontend's built assets are committed to the repository, so building never
requires Node.

## Release binary

No Homebrew and no Go? Grab the tarball for your OS and architecture from
[the releases page](https://github.com/runbear-io/beardrive/releases).

## Verify

```sh
bdrive version
```

## Upgrading

```sh
brew upgrade beardrive
```

Clients and hub are the same binary — keep them roughly in step. The sync
protocol is append-only journals plus blobs, which old clients read forward.

After upgrading a client, re-run `bdrive hooks install` once per project to pick
up hook improvements, and `bdrive skill install` once per machine to refresh the
agent skill.

## Next

[Set up by hand](/manual/setup-by-hand/) — sign in and start syncing a folder,
command by command.
