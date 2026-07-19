import type { Org, Project } from "../api/types";

// Settings for the open project (sidebar menu). Today: identity facts;
// per-project knobs land here as they grow. Install/connect lives on the
// Installation page.
export function ProjectSettings({ project, org }: { project: Project; org: Org | null }) {
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
    </div>
  );
}
