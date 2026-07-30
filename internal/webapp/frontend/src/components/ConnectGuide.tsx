import { useState } from "react";
import type { Project } from "../api/types";
import { copyText } from "../util";
import { ProjectIcon } from "./shell";
import { projColor } from "./ProjectNav";

/* ---- project home guide ----
   One paste sets up any coding agent: the prompt points at the canonical
   INSTALL_FOR_AGENTS.md with this hub's URL and this project's id filled
   in. The agent fetches the doc and handles every deviation — already
   installed, no Homebrew, sign-in, wrong folder — so the page itself
   stays to one line of prose; detail lives in the collapsed sections. */

export const INSTALL_DOC = "https://raw.githubusercontent.com/runbear-io/beardrive/main/INSTALL_FOR_AGENTS.md";

export function ConnectGuide({ project }: { project: Project }) {
  const origin = window.location.origin;
  const prompt =
    "Follow " +
    INSTALL_DOC +
    "\nto set up BearDrive project " +
    project.id +
    " on " +
    origin +
    ". Ask me which folder to sync.";
  const manual =
    "brew install runbear-io/tap/beardrive" +
    "\nbdrive login " +
    origin +
    "\nbdrive init --project " +
    project.id;

  return (
    <div className="guide">
      <h1 className="in-title gd-head">
        <span
          className="proj-mark"
          aria-hidden="true"
          style={{ background: projColor(project.name) }}
        >
          <ProjectIcon name={project.icon} />
        </span>
        {project.name}
      </h1>
      {project.description && <p className="in-desc">{project.description}</p>}
      <div className="gd-body">
        <p className="gd-desc">
          Paste into your coding agent — Claude Code, Cowork, Codex, Gemini CLI, Hermes — in the
          folder where you want the files:
        </p>
        <GuideCode code={prompt} />
        <p className="gd-desc">
          The agent installs the CLI, signs this machine in, and registers the sync hooks — asking
          before anything it changes.
        </p>
        <details className="gd-manual">
          <summary>What exactly happens</summary>
          <ul className="gd-desc gd-list">
            <li>
              Sign-in uses a device code you approve in this browser — the folder itself never
              holds credentials.
            </li>
            <li>
              Sync hooks pull the latest before every agent turn, push edits seconds after they
              happen, and stamp each change with the session that made it; agent reads feed
              Insights. They register once per machine in your agent's own config, so every
              session is covered and nothing is written into the synced folder.
            </li>
            <li>Codex hooks are off by default: set [features] codex_hooks = true in ~/.codex/config.toml.</li>
          </ul>
        </details>
        <details className="gd-manual">
          <summary>Or run it yourself</summary>
          <p className="gd-desc">
            Same result, in the folder you want the files. One command: init signs this device in,
            registers the sync hooks and starts syncing.
          </p>
          <GuideCode code={manual} />
          <p className="gd-desc">
            <a href="https://docs.beardrive.ai/manual/install/" target="_blank" rel="noreferrer">
              Full manual setup guide →
            </a>
          </p>
        </details>
      </div>
    </div>
  );
}

export function GuideCode({ code }: { code: string }) {
  const [label, setLabel] = useState("Copy");
  return (
    <pre className="gd-code">
      <code>{code}</code>
      <button
        className="gd-copy"
        onClick={async () => {
          setLabel((await copyText(code)) ? "Copied" : "Copy failed");
          setTimeout(() => setLabel("Copy"), 1400);
        }}
      >
        {label}
      </button>
    </pre>
  );
}
