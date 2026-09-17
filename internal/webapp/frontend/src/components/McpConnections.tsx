import { useQueryClient, useQuery, useMutation } from "@tanstack/react-query";
import { api, getJSON } from "../api/http";
import type { McpGrant, Project } from "../api/types";
import { toast } from "../toast";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

// Connected agents: the browser surface for the MCP grants an account holds.
//
// Connecting happens on the server-rendered consent screen (/oauth/authorize),
// deliberately — it is where a human decides what an agent may touch, and it
// has to work before any grant exists. This page is the other half:
// seeing what is connected, and disconnecting it.
//
// Revocation is a security control, so it gets a real surface rather than a
// curl command. `bdrive mcp revoke` does the same thing from a terminal.

function ago(iso?: string): string {
  if (!iso || iso.startsWith("0001-")) return "never";
  const d = Date.now() - new Date(iso).getTime();
  const m = Math.floor(d / 60000);
  if (m < 1) return "just now";
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

export function McpConnections({ projects }: { projects: Project[] }) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["mcp", "grants"],
    queryFn: () => getJSON<{ grants: McpGrant[] }>("/api/mcp/grants"),
  });

  const revoke = useMutation({
    mutationFn: (id: string) => api("DELETE", `/api/mcp/grants/${encodeURIComponent(id)}`),
    onSuccess: () => {
      toast("Disconnected. The agent's access stopped immediately.");
      qc.invalidateQueries({ queryKey: ["mcp", "grants"] });
    },
    onError: (e) => toast((e as Error).message, true),
  });

  // A grant stores project IDs; show the names the user knows them by, and
  // fall back to the id for a project they can no longer see (the grant keeps
  // naming it, but it grants nothing — permission is resolved live).
  const nameOf = (id: string) => projects.find((p) => p.id === id)?.name || id;

  if (q.isLoading) return <div className="empty">Loading…</div>;
  if (q.error) {
    return (
      <div className="empty">
        <h3>Connections are unavailable</h3>
        <p>{(q.error as Error).message}</p>
      </div>
    );
  }

  const grants = q.data?.grants ?? [];

  return (
    <div className="project-settings" id="mcp-connections">
      <h2>Connected agents</h2>
      <p className="admin-sub">
        Agents you have connected to this hub over{" "}
        <a href="https://modelcontextprotocol.io" target="_blank" rel="noreferrer">
          MCP
        </a>
        . Each one reads and changes files in the projects you chose, acting as you — so its
        changes appear in History under your name, and it can never do more than you can.
      </p>

      {grants.length === 0 ? (
        <Card>
          <CardHeader>
            <CardTitle>No agents connected</CardTitle>
            <CardDescription>
              Point an MCP client (Claude, ChatGPT, Cursor, …) at{" "}
              <code>{window.location.origin}/mcp</code>. It will send you back here to pick which
              projects it may use.
            </CardDescription>
          </CardHeader>
        </Card>
      ) : (
        grants.map((g) => (
          <Card key={g.id} className="mcp-grant">
            <CardHeader>
              <CardTitle>{g.client_name || "Unnamed client"}</CardTitle>
              <CardDescription>
                Connected {ago(g.created)} · last used {ago(g.last_used)}
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div className="mcp-projects">
                {g.projects.map((id) => (
                  <span className="ps-chip" key={id}>
                    {nameOf(id)}
                  </span>
                ))}
              </div>
              <Button
                variant="destructive"
                disabled={revoke.isPending}
                onClick={() => {
                  // No confirm dialog: this is reversible in the only sense
                  // that matters — reconnecting is a click on the agent's
                  // side — and the destructive thing is leaving an agent
                  // connected that you no longer recognize.
                  revoke.mutate(g.id);
                }}
              >
                Disconnect
              </Button>
            </CardContent>
          </Card>
        ))
      )}
    </div>
  );
}
