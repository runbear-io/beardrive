import { useState } from "react";
import type { Project } from "../api/types";
import { copyText } from "../util";

/* ---- project home guide ----
   How to mount the project as a local folder and connect a coding agent,
   with real, copyable commands: this hub's URL and this project's id
   filled in. */

interface GuideAgent {
  key: string;
  label: string;
  hook?: string;
  note?: string;
  extra?: string;
}

const GUIDE_AGENTS: GuideAgent[] = [
  { key: "claude", label: "Claude Code & Cowork" },
  {
    key: "hermes",
    label: "Hermes",
    hook: "hermes",
    note:
      "Registers BearDrive's hooks in Hermes's config: pull before every turn, push after edits " +
      "with a session note, and report file reads to Insights.",
  },
  {
    key: "codex",
    label: "Codex",
    hook: "codex",
    note: "Registers hooks in .codex/hooks.json.",
    extra:
      "Run /hooks inside Codex once to trust the project's .codex layer — after that every turn " +
      "pulls, edits push automatically, and reads are reported to Insights.",
  },
];

interface Step {
  title: string;
  desc?: string;
  code?: string;
  extra?: string;
}

function guideSteps(agent: GuideAgent, project: Project): Step[] {
  const origin = window.location.origin;
  const pid = project.id;

  // Claude Code and Claude Cowork share one plugin: install it once, then
  // /beardrive:install does the whole setup conversationally.
  if (agent.key === "claude") {
    return [
      {
        title: "Add the BearDrive plugin",
        desc:
          "One time, in any Claude Code session. The plugin ships the beardrive skill, the /beardrive " +
          "commands, and turn-boundary sync hooks — and Claude Cowork shares the same plugins, so " +
          "installing it once covers both.",
        code: "/plugin marketplace add runbear-io/beardrive\n/plugin install beardrive@beardrive",
      },
      {
        title: "Set up this project conversationally",
        desc: "In a Claude Code or Cowork session in the folder where you want the files, run:",
        code: "/beardrive:install connect to " + origin + ", project " + pid,
        extra:
          "Claude installs the CLI, signs this machine in, mounts the project, and registers the " +
          "sync hooks — pull the latest before every turn, push after edits (stamped with the session " +
          "that made them), and report file reads to Insights. It asks before anything it changes.",
      },
    ];
  }

  const slug =
    (project.name || "project").toLowerCase().replace(/[^a-z0-9._-]+/g, "-") || "project";
  return [
    {
      title: "Install the BearDrive CLI",
      desc: "One static binary. Homebrew on macOS and Linux; releases and `go install` also work.",
      code: "brew install runbear-io/tap/beardrive",
    },
    {
      title: "Sign in to this hub",
      desc:
        "Opens the browser once and stores a device token on this machine — the synced folder itself never holds credentials.",
      code: "bdrive login " + origin,
    },
    {
      title: "Mount the project into a local folder",
      desc:
        "Run it where you want the files. An existing folder works too — contents merge, and re-running init later (or after moving the folder) just resumes.",
      code: "mkdir -p ~/" + slug + " && cd ~/" + slug + "\nbdrive init --project " + pid,
    },
    {
      title: "Connect " + agent.label,
      desc: agent.note,
      code: "bdrive hooks install --agent " + agent.hook,
      extra: agent.extra,
    },
  ];
}

function savedAgent(): string {
  try {
    return localStorage.getItem("bdrive-guide-agent") || "claude";
  } catch {
    return "claude"; // private mode
  }
}

export function ConnectGuide({ project }: { project: Project }) {
  const [agentKey, setAgentKey] = useState(savedAgent);
  // A stale saved key (e.g. a tab that no longer exists) falls back to the
  // first tab rather than rendering nothing.
  const agent = GUIDE_AGENTS.find((a) => a.key === agentKey) || GUIDE_AGENTS[0];

  return (
    <div className="guide">
      <h1 className="in-title">{project.name}</h1>
      <p className="dl-sub">
        Mount this project as a folder on any machine and connect your coding agent: files sync both
        ways in the background, every change is journaled with who made it, and agent reads feed
        Insights.
      </p>
      <div className="gd-tabs">
        {GUIDE_AGENTS.map((a) => (
          <button
            key={a.key}
            className={"gd-tab" + (a.key === agent.key ? " active" : "")}
            data-key={a.key}
            onClick={() => {
              setAgentKey(a.key);
              try {
                localStorage.setItem("bdrive-guide-agent", a.key);
              } catch {
                /* private mode */
              }
            }}
          >
            {a.label}
          </button>
        ))}
      </div>
      <div className="gd-body">
        {guideSteps(agent, project).map((s, i) => (
          <div className="gd-step" key={i}>
            <div className="gd-step-head">
              <span className="gd-num">{i + 1}</span>
              <span className="gd-step-title">{s.title}</span>
            </div>
            {s.desc && <p className="gd-desc">{s.desc}</p>}
            {s.code && <GuideCode code={s.code} />}
            {s.extra && <p className="gd-desc gd-extra">{s.extra}</p>}
          </div>
        ))}
        <p className="gd-done">
          That's it — the folder now syncs on its own. Every agent turn starts from the latest
          state, edits appear here (and on every teammate's mount) within seconds, and what your
          agents read shows up in Insights.
        </p>
      </div>
    </div>
  );
}

function GuideCode({ code }: { code: string }) {
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
