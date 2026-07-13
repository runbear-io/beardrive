// Hand-written TypeScript shapes for the Go API responses. Derived from the
// handler structs in internal/webapp — keep in sync when handlers change.
// The API is deliberately storage-blind: nothing here ever names a bucket,
// remote URL, or credential, and heat responses carry no actor identities.

// GET /api/config (handleConfig, server.go)
export interface ServerConfig {
  mode: "volume" | "hub";
  volume: string;
  brand: string;
  upload: { enabled: boolean };
  auth: {
    enabled: boolean;
    cli_login?: string;
    allow_signup?: boolean;
    admin?: boolean;
  };
  reads: { enabled: boolean };
  me?: { email: string; name: string };
}

// GET /api/projects (handleProjectList → Project, projects.go)
export interface Project {
  id: string;
  name: string;
  org?: string;
  created?: string;
}

export interface ProjectList {
  projects: Project[];
}

// POST /api/projects (handleProjectCreate) — create-or-join by name.
export interface ProjectCreated {
  project: Project;
  created: boolean;
}

// GET /api/orgs (handleOrgList, orgs.go)
export interface OrgMember {
  email: string;
  role: string; // "owner" | "member"
}

export interface Org {
  id: string;
  name: string;
  role: string; // the signed-in account's role in this org
  members: OrgMember[];
  created?: string;
}

export interface OrgList {
  orgs: Org[];
}

// POST /api/invites/{token} (handleInviteAccept, orgs.go)
export interface InviteAccepted {
  ok: boolean;
  org: { id: string; name: string };
}

// GET /api/admin/pending (handleAdminPending, admin.go)
export interface PendingList {
  pending: Array<{ id: string; email: string; name: string }>;
}
