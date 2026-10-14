import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, getJSON, postJSON } from "../api/http";
import type { CommentAnchor, CommentThread } from "../api/types";

// Every thread on one file, open and resolved: the panel filters, and the
// count on the toolbar button is the open ones. The change stream refetches
// it when anyone comments (useProjectEvents); the poll is the floor under
// that, as it is for the tree.
export function useComments(apiBase: string, path: string, enabled: boolean) {
  return useQuery({
    queryKey: ["comments", apiBase, path],
    queryFn: () =>
      getJSON<{ comments: CommentThread[] }>(
        apiBase + "comments?status=all&path=" + encodeURIComponent(path),
      ),
    enabled: enabled && !!path,
    select: (d) => d.comments || [],
    refetchInterval: 30_000,
  });
}

export function useCommentActions(apiBase: string) {
  const qc = useQueryClient();
  const done = () => qc.invalidateQueries({ queryKey: ["comments", apiBase] });
  return {
    create: async (path: string, body: string, anchor?: CommentAnchor, sha?: string) => {
      const r = await postJSON<{ comment: CommentThread }>(apiBase + "comments", { path, body, anchor, sha });
      await done();
      return r.comment;
    },
    act: async (id: string, action: "reply" | "resolve" | "reopen", body = "") => {
      await postJSON(apiBase + "comments/" + id, { action, body });
      await done();
    },
    remove: async (id: string) => {
      await api("DELETE", apiBase + "comments/" + id);
      await done();
    },
  };
}
