import type { Org, Project } from "../api/types";
import { ConnectGuide } from "./ConnectGuide";

// Settings for the open project (the header gear). Today: identity facts
// plus the connect guide; per-project knobs land here as they grow.
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
      <h3>Connect a device</h3>
      <ConnectGuide project={project} />
    </div>
  );
}
