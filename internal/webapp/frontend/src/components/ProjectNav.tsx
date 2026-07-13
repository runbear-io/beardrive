import { useNavigate } from "react-router-dom";
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

export function ProjectNav({ projects, currentId }: { projects: Project[]; currentId?: string }) {
  const navigate = useNavigate();
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
      <ul>
        {projects.map((p) => (
          <li key={p.id}>
            <div
              className={"row" + (currentId === p.id ? " active" : "")}
              title={p.name}
              tabIndex={0}
              role="button"
              onClick={() => {
                navigate("/" + p.id);
                closeSidebarOnMobile();
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  (e.currentTarget as HTMLElement).click();
                }
              }}
            >
              <span className="proj-mark" style={{ background: projColor(p.name) }}>
                {p.name.trim()[0] || "?"}
              </span>
              <span className="label">{p.name}</span>
            </div>
          </li>
        ))}
      </ul>
    </nav>
  );
}
