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
  // Where this org is administered: the hub's own page on a self-hosted
  // install, an external directory's page on a managed one. Follow it; do
  // not branch on it.
  manage_url: string;
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

// GET .../tree (handleTree → Node, server.go)
export interface Node {
  name: string;
  path: string;
  dir?: boolean;
  size?: number;
  time?: string;
  author?: string;
  device?: string;
  children?: Node[];
}

// GET .../render (handleRender, server.go)
export interface RenderDoc {
  path: string;
  html: string;
  size: number;
  time?: string;
  author?: string;
  device?: string;
}

// GET .../heat (handleHeat, reads.go) — counts only, never who.
export interface HeatEntry {
  human?: number;
  agent?: number;
  share?: number;
  readers?: number;
  last?: string;
}
export type HeatMap = Record<string, HeatEntry>;

// GET .../history (HistoryEntry, history.go)
export interface DeviceInfo {
  id?: string;
  name?: string;
  os?: string;
  ip?: string;
}
export interface HistoryEntry {
  time: string;
  kind: string; // add | edit | delete (older servers: raw "put")
  path: string;
  size?: number;
  blob?: string;
  user?: string;
  user_name?: string;
  author?: string;
  device: DeviceInfo;
  note?: string;
}

// POST .../shares (handleShareCreate, shares.go)
export interface ShareCreated {
  token: string;
  url: string;
}

// GET /api/orgs/{org}/invites (handleInviteList, orgs.go)
export interface OrgInviteInfo {
  token: string;
  url: string;
  creator?: string;
  created?: string;
  expires: string;
  uses: number;
}

// GET /api/orgs/{org}/shares (handleOrgShares, admin.go)
export interface OrgShareInfo {
  token: string;
  url: string;
  path: string;
  project_name?: string;
  creator?: string;
  created?: string;
}

// GET/POST /api/admin/policy (handleAdminPolicy, admin.go)
export interface AdminPolicy {
  require_verification: boolean;
  require_approval: boolean;
  allow_signup: boolean;
  allowed_domains?: string[]; // read-only (server config)
  admins?: string[]; // read-only (server config)
  mailer: boolean;
}

// POST .../upload/init (handleUploadInit, upload.go)
export interface UploadPlan {
  mode: "direct" | "server";
  exists?: boolean;
  url?: string;
  method?: string;
  headers?: Record<string, string>;
}
