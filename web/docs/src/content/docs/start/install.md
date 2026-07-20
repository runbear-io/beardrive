---
title: Install
description: Install the bdrive CLI with Homebrew, go install, or a release binary.
---

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

[Quickstart](/start/quickstart/) — sign in and start syncing a folder.
