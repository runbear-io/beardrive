import { navigate } from "../nav";
import { Icon, ProjectIcon } from "./shell";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Project } from "../api/types";
import { closeSidebarOnMobile } from "./shell";

// Deterministic accent for a project's letter-mark, so each project keeps a
// stable color across reloads without any server state.
const PROJ_COLORS = ["#5b8def", "#f5a623", "#4cc38a", "#e0679b", "#8b7bf0", "#3ec8c8", "#e6934a"];
export function projColor(s: string): string {
  let h = 0;
  for (const c of s) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return PROJ_COLORS[h % PROJ_COLORS.length];
}

export interface ProjectMenu {
  active: "dashboard" | "install" | "history" | "settings" | null;
  onDashboard: () => void;
  onInstall: () => void;
  onHistory: () => void;
  onSettings: () => void;
}

export function ProjectNav({
  projects,
  currentId,
  menu,
  onNew,
}: {
  projects: Project[];
  currentId?: string;
  menu?: ProjectMenu;
  // Opening the create dialog belongs to HubApp: three things ask for it now
  // (this button, the empty state's button, and the auto-open when a signed-in
  // account has no projects at all), and one owner beats three copies.
  onNew: () => void;
}) {
  const current = projects.find((p) => p.id === currentId);

  return (
    <nav id="projects" aria-label="Projects">
      <div className="nav-head">
        <span>Projects</span>
        <button
          className="nav-add"
          title="New project"
          aria-label="New project"
          onClick={onNew}
        >
          +
        </button>
      </div>
      <div className="proj-row">
        <Select
          value={currentId || ""}
          onValueChange={(v) => {
            if (v && v !== currentId) {
              navigate("/" + v);
              closeSidebarOnMobile();
            }
          }}
        >
          <SelectTrigger
            id="project-select"
            aria-label={`Switch project — current: ${current?.name ?? "none"}`}
            title={current?.name}
            className="proj-trigger"
          >
            {current && (
              <span
                className="proj-mark"
                aria-hidden="true"
                style={{ background: projColor(current.name) }}
              >
                <ProjectIcon name={current.icon} />
              </span>
            )}
            {/* SelectValue mirrors the selected item's text into the trigger —
                which, now that every item carries its own mark, would draw a
                second one here. Render the name ourselves when we have it,
                keeping the [data-slot="select-value"] styling hook. */}
            {current ? (
              <span data-slot="select-value">{current.name}</span>
            ) : (
              <SelectValue placeholder="Select a project" />
            )}
          </SelectTrigger>
          <SelectContent className="proj-menu" position="popper" sideOffset={4}>
            {projects.map((p) => (
              <SelectItem key={p.id} value={p.id}>
                <span
                  className="proj-mark"
                  aria-hidden="true"
                  style={{ background: projColor(p.name) }}
                >
                  <ProjectIcon name={p.icon} />
                </span>
                {p.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {menu && (
        <ul className="nav-menu" aria-label="Project pages">
          {(
            [
              ["dashboard", "Dashboard", "dashboard", menu.onDashboard],
              ["install", "Installation", "terminal", menu.onInstall],
              ["history", "History", "hist", menu.onHistory],
              ["settings", "Settings", "gear", menu.onSettings],
            ] as const
          ).map(([key, label, icon, onClick]) => (
            <li key={key}>
              <div
                id={"nav-" + key}
                className={"row" + (menu.active === key ? " active" : "")}
                role="button"
                tabIndex={0}
                onClick={onClick}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    onClick();
                  }
                }}
              >
                <Icon name={icon} />
                <span className="label">{label}</span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </nav>
  );
}
