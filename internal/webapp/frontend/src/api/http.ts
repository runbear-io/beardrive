// Fetch helpers shared by every API hook. All URLs are root-absolute: a
// deep path like /<project>/<dir>/<file> must never break relative
// resolution.

function toLogin(): never {
  // Auth required: sign in, then come back to the current route.
  location.href =
    "/auth/login?next=" + encodeURIComponent(location.pathname + location.search);
  throw new Error("signing in…");
}

export async function getJSON<T>(url: string): Promise<T> {
  const r = await fetch(url);
  if (r.status === 401) toLogin();
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

export async function postJSON<T>(url: string, body?: unknown): Promise<T> {
  const r = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  if (r.status === 401) toLogin();
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}
