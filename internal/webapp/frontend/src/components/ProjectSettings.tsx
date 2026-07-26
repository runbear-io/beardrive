import { api } from "../api/http";
import { modalPrompt } from "../modal";
import { toast } from "../toast";
import { Button } from "@/components/ui/button";
import type { Org, Project } from "../api/types";

// Settings for the open project (sidebar menu). Today: identity facts and
// the delete danger zone; per-project knobs land here as they grow.
// Install/connect lives on the Installation page.
export function ProjectSettings({
  project,
  org,
  onDeleted,
}: {
  project: Project;
  org: Org | null;
  onDeleted: () => Promise<void>;
}) {
  return (
    <div className="project-settings">
      <h2>{project.name}</h2>
      <dl className="ps-facts">
        <dt>Project id</dt>
        <dd>
          <code>{project.id}</code>
        </dd>
        {org && (
          <>
            <dt>Workspace</dt>
            <dd>{org.name}</dd>
          </>
        )}
        {project.created && (
          <>
            <dt>Created</dt>
            <dd>{new Date(project.created).toLocaleDateString()}</dd>
          </>
        )}
      </dl>

      {/* Owner-only, and only as UX: handleProjectDelete enforces it too. */}
      {org?.role === "owner" && (
        <section className="ps-danger">
          <h3>Danger zone</h3>
          <p>
            Deleting removes the project from this hub. Its files stay in storage.
            This can't be undone.
          </p>
          <Button
            variant="danger"
            onClick={async () => {
              const typed = await modalPrompt(
                `Delete “${project.name}”?`,
                "This can't be undone. Type the project name to confirm:",
                "",
                "Delete project",
                { match: project.name, danger: true },
              );
              if (typed === null) return;
              try {
                await api("DELETE", "/api/projects/" + project.id);
                toast(`Deleted “${project.name}”.`);
                await onDeleted();
              } catch (e) {
                toast((e as Error).message, true);
              }
            }}
          >
            Delete project
          </Button>
        </section>
      )}
    </div>
  );
}
