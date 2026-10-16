/* Whether a server behind `apiBase` can serve a co-editing room at all.

   Rooms are per project: the hub names one from (project, path) and
   authorizes the connection against that project. A single-volume server
   (`/api/`) has no project, so its socket is refused every time — asking
   would only cost the editor the whole unreachable wait before it opened. */
export function hasCollabRoute(apiBase: string): boolean {
  return /\/p\/[^/]+\/$/.test(apiBase);
}
