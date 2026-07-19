import { navigate } from "../nav";
import { Icon } from "./shell";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { postJSON } from "../api/http";
import type { Project, ProjectCreated } from "../api/types";
import { modalPrompt } from "../modal";
import { toast } from "../toast";
import { useHubRefresh } from "../hooks/useHub";
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
}: {
  projects: Project[];
  currentId?: string;
  menu?: ProjectMenu;
}) {
  const refresh = useHubRefresh();

  const create = async () => {
    const name = await modalPrompt("New project", "Project name", "", "Create");
    if (!name) return;
    try {
      const out = await postJSON<ProjectCreated>("/api/projects", { name });
      await refresh();
      navigate("/" + out.project.id);
      toast(`Created “${out.project.name}”.`);
    } catch (e) {
      toast("Could not create the project: " + (e as Error).message, true);
    }
  };

  return (
    <nav id="projects" aria-label="Projects">
      <div className="nav-head">
        <span>Projects</span>
        <button className="nav-add" title="New project" onClick={create}>
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
          <SelectTrigger id="project-select" aria-label="Switch project" className="proj-trigger">
            {currentId && (
              <span
                className="proj-mark"
                aria-hidden="true"
                style={{ background: projColor(projects.find((p) => p.id === currentId)?.name || "") }}
              />
            )}
            <SelectValue placeholder="Select a project" />
          </SelectTrigger>
          <SelectContent className="proj-menu" position="popper" sideOffset={4}>
            {projects.map((p) => (
              <SelectItem key={p.id} value={p.id}>
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
