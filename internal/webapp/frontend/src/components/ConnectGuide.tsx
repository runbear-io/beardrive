import { useState } from "react";
import type { Project } from "../api/types";
import { copyText } from "../util";
import { ProjectIcon } from "./shell";
import { projColor } from "./ProjectNav";

/* ---- project home guide ----
   How to mount the project as a local folder and connect a coding agent,
   with real, copyable commands: this hub's URL and this project's id
   filled in. */

interface GuideAgent {
  key: string;
  label: string;
  agent?: string; // --agent value for `bdrive skill install`
  skillDir?: string; // where that agent reads the skill from
  extra?: string; // platform-specific caveat after the last step
}

const GUIDE_AGENTS: GuideAgent[] = [
  { key: "claude", label: "Claude Code & Cowork" },
  {
    key: "hermes",
    label: "Hermes",
    agent: "hermes",
    skillDir: "~/.hermes/skills/beardrive/",
  },
  {
    key: "codex",
    label: "Codex",
    agent: "codex",
    skillDir: "~/.codex/skills/beardrive/",
    extra:
      "Codex asks once to trust the project's .codex hooks layer — answer yes (or run /hooks) and " +
      "from then on every turn pulls, edits push automatically, and reads are reported to Insights.",
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

  // Every other agent: one paste, no terminal — the same shape as the Claude
  // tab. The commands ride INSIDE the prompt because these agents ship no
  // BearDrive knowledge (Claude's tab can be terse only because the plugin
  // carries it); the user still copies one thing, and the agent handles every
  // deviation — already installed, no Homebrew, sign-in, wrong folder. Step 2
  // of the prompt installs the skill, so every later session is conversational.
  return [
    {
      title: "Paste this into " + agent.label,
      desc:
        "Start " +
        agent.label +
        " in the folder where you want the files (an existing folder works too — contents merge), " +
        "then paste:",
      code: setupPrompt(agent, project),
      extra:
        "Approve the shell commands when it asks. It installs the CLI, signs this machine in (it " +
        "hands you a code and a URL — the folder itself never holds credentials), mounts the " +
        "project, and registers the sync hooks: pull before every turn, push after edits stamped " +
        "with the session that made them, file reads into Insights. It also keeps the beardrive " +
        "skill in " +
        agent.skillDir +
        ", so from here on you can just ask." + (agent.extra ? " " + agent.extra : ""),
    },
  ];
}

// The prompt the user pastes. It points at the canonical agent instructions
// (repo-root INSTALL_FOR_AGENTS.md) instead of inlining commands, so the
// steps never go stale in someone's copy — the agent fetches the doc and
// works through install, skill install, device-code login, init, and hooks.
function setupPrompt(_agent: GuideAgent, project: Project): string {
  return [
    "Follow https://raw.githubusercontent.com/runbear-io/beardrive/main/INSTALL_FOR_AGENTS.md",
    "to connect this folder to BearDrive project " + project.id + " on " + window.location.origin + ".",
  ].join("\n");
}

// For anyone who would rather not hand the setup to an agent. Skipping
// `hooks install` is the one thing that silently costs you turn-boundary
// syncing, so it is spelled out here.
function manualCommands(agent: GuideAgent, project: Project): string {
  return (
    "brew install runbear-io/tap/beardrive" +
    "\nbdrive skill install --agent " +
    agent.agent +
    "\nbdrive login " +
    window.location.origin +
    "\nbdrive init --project " +
    project.id +
    "\nbdrive hooks install --agent " +
    agent.agent
  );
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
  const steps = guideSteps(agent, project);

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
        {steps.map((s, i) => (
          <div className={"gd-step" + (steps.length > 1 ? "" : " gd-solo")} key={i}>
            <div className="gd-step-head">
              {/* A lone step is not a sequence — no "1." badge for it. */}
              {steps.length > 1 && <span className="gd-num">{i + 1}</span>}
              <span className="gd-step-title">{s.title}</span>
            </div>
            {s.desc && <p className="gd-desc">{s.desc}</p>}
            {s.code && <GuideCode code={s.code} />}
            {s.extra && <p className="gd-desc gd-extra">{s.extra}</p>}
          </div>
        ))}
        {agent.agent && (
          <details className="gd-manual">
            <summary>Or run it yourself</summary>
            <p className="gd-desc">
              Same result, in the folder you want the files. Don't skip the last line — the hooks
              are what keep every turn starting from the latest state.
            </p>
            <GuideCode code={manualCommands(agent, project)} />
          </details>
        )}
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
