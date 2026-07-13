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
