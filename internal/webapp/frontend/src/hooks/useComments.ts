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
  const refetch = () => void qc.invalidateQueries({ queryKey: ["comments", apiBase] });
  /* The hub answers a write with the thread as it now stands, so the cached
     list takes that answer at once and the refetch only confirms it in the
     background: whoever just posted sees it without a second round trip. */
  const place = (t: CommentThread | null, drop?: string) => {
    for (const [key, data] of qc.getQueriesData<{ comments: CommentThread[] }>({ queryKey: ["comments", apiBase] })) {
      if (!data || key.length !== 3) continue;
      const path = key[2] as string;
      let list = (data.comments || []).filter((c) => c.id !== drop);
      if (t) {
        const at = list.findIndex((c) => c.id === t.id);
        if (at >= 0) list = list.map((c, i) => (i === at ? t : c));
        else if (t.path === path) list = [...list, t];
      }
      qc.setQueryData(key, { ...data, comments: list });
    }
  };
  return {
    create: async (path: string, body: string, anchor?: CommentAnchor, sha?: string) => {
      const r = await postJSON<{ comment: CommentThread }>(apiBase + "comments", { path, body, anchor, sha });
      place(r.comment);
      refetch();
      return r.comment;
    },
    act: async (id: string, action: "reply" | "resolve" | "reopen", body = "") => {
      const r = await postJSON<{ comment?: CommentThread }>(apiBase + "comments/" + id, { action, body });
      if (r?.comment) place(r.comment);
      refetch();
    },
    // Gone from the list before the hub answers; a refusal is put back by the
    // refetch, which runs either way.
    remove: async (id: string) => {
      place(null, id);
      try {
        await api("DELETE", apiBase + "comments/" + id);
      } finally {
        refetch();
      }
    },
  };
}
