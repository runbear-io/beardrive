import { navigate } from "../nav";
import { Icon } from "./shell";
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

export function ProjectNav({
  projects,
  currentId,
  onOpenSettings,
}: {
  projects: Project[];
  currentId?: string;
  onOpenSettings?: () => void;
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
        <span className="proj-select-wrap">
          {currentId && (
            <span
              className="proj-mark"
              aria-hidden="true"
              style={{ background: projColor(projects.find((p) => p.id === currentId)?.name || "") }}
            />
          )}
          <select
            id="project-select"
            aria-label="Switch project"
            value={currentId || ""}
            onChange={(e) => {
              if (e.target.value) {
                navigate("/" + e.target.value);
                closeSidebarOnMobile();
              }
            }}
          >
            {!currentId && <option value="" disabled />}
            {projects.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </select>
          <Icon name="chevd" />
        </span>
        {onOpenSettings && (
          <button
            id="project-settings-btn"
            className="icon-btn2"
            title="Project settings"
            aria-label="Project settings"
            onClick={onOpenSettings}
          >
            <Icon name="gear" />
          </button>
        )}
      </div>
    </nav>
  );
}
