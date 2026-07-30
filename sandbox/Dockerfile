# A disposable Linux machine to run a scenario in when it needs one.
#
# This is an ENVIRONMENT, not a test suite. Scenarios live where they belong:
# deterministic ones in internal/webapp/cli_e2e_test.go and internal/syncer,
# the conversational one in the onboarding-e2e skill. Reach for the sandbox
# only when a scenario needs something a Go test cannot give it:
#
#   - a fresh $HOME, so `bdrive init` writes its device identity and its agent
#     hooks (~/.claude/settings.json) somewhere thrown away instead of yours
#   - a real `claude` session, with the real permission classifier
#   - Linux, for the systemd user unit
#   - a reboot, simulated by killing processes while the filesystem survives
#
# It provides a hub, a seeded account, Claude Code, browserless sign-in
# (bdrive-signin / bdrive-approve) and the binary under test. The two scripts
# it ships are the scenarios that cannot live anywhere else.
#
# The bdrive binary is bind-mounted at run time rather than built in, so
# testing a code change rebuilds the binary, not the image. Use run.sh, which
# is the intended entry point.
#
# No `# syntax=` directive on purpose: it makes every build resolve the
# Dockerfile frontend from the registry, which turns a cold or slow network
# into a build that hangs with no output. That also rules out RUN heredocs,
# hence boot.sh being its own file.
FROM node:22-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git less \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @anthropic-ai/claude-code \
 && npm cache clean --force

ENV HOME=/home/tester \
    BDRIVE_HOME=/home/tester/.bdrive \
    HUB=http://localhost:8080 \
    HUB_EMAIL=me@example.com \
    HUB_PASSWORD=hunter2hunter2
WORKDIR /work

COPY boot.sh /usr/local/bin/boot
COPY onboarding.sh /usr/local/bin/onboarding
COPY daemon-linux.sh /usr/local/bin/daemon-linux
RUN chmod +x /usr/local/bin/boot /usr/local/bin/onboarding /usr/local/bin/daemon-linux

ENTRYPOINT ["/usr/local/bin/boot"]
CMD ["bash"]
