# Quickstart→sync demo (GIF + MP4)

The launch demo per the BEA-4 content-calendar rev 3 spec: agent-first
(`/beardrive:install` → agent writes the plan → teammate's agent reads it
fresh → share link), no bare CLI on screen.

- `demo-quickstart.gif` — embed target for PH / blog / social / README.
- `demo-quickstart.mp4` — same cut, for platforms that prefer video.

Provenance: every output line replays a real output from the fresh-machine
quickstart run verified in BEA-33 against v0.10.0 (install, login, two-device
sync, `bdrive share`); timing is condensed to demo pace and the hub URL is
prettified from the test hub's localhost address.

Re-render after changes: `brew install vhs ttyd ffmpeg && vhs demo.tape`
(edit the storyboard in `demo-script.sh`).
