// Fetch helpers shared by every API hook. All URLs are root-absolute: a
// deep path like /<project>/<dir>/<file> must never break relative
// resolution.

function toLogin(): never {
  // Auth required: sign in, then come back to the current route.
  location.href =
    "/auth/login?next=" + encodeURIComponent(location.pathname + location.search);
  throw new Error("signing in…");
}

// Server messages are written for operators and CLIs — lowercase, unpunctuated,
// occasionally naming internals ("forbidden: seat limit reached for plan free").
// They surface verbatim in toasts, so the ones we can predict get product copy
// and everything else falls back to the server's own words, which is still
// better than a generic apology when the cause is specific.
function errorFor(status: number, body: string): string {
  const raw = body.trim();
  switch (status) {
    case 403:
      if (raw.includes("seat")) return "This plan is out of seats. Upgrade to add more people.";
      if (raw.includes("owner")) return "Only owners can do that.";
      return "You don't have access to that.";
    case 409:
      // The 409 body carries the URL that owns the thing, which is the whole
      // point of the message — keep it, but capitalize like the rest.
      return raw ? raw[0].toUpperCase() + raw.slice(1) : "That is managed outside this hub.";
    case 404:
      return "That is gone — it may have been removed already.";
    case 429:
      return "Too many requests. Give it a moment.";
    default:
      if (status >= 500) return "The server had a problem. Try again.";
      return raw ? raw[0].toUpperCase() + raw.slice(1) : "Something went wrong.";
  }
}

async function fail(r: Response): Promise<never> {
  throw new Error(errorFor(r.status, await r.text()));
}

export async function getJSON<T>(url: string): Promise<T> {
  const r = await fetch(url);
  if (r.status === 401) toLogin();
  if (!r.ok) await fail(r);
  return r.json();
}

/* fetch wrapper for methods without a body-returning helper */
export async function api<T = unknown>(method: string, url: string, body?: unknown): Promise<T> {
  const opt: RequestInit = { method };
  if (body !== undefined) {
    opt.headers = { "Content-Type": "application/json" };
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(url, opt);
  if (!r.ok) await fail(r);
  return r.status === 204 ? ({} as T) : r.json();
}

export async function postJSON<T>(url: string, body?: unknown): Promise<T> {
  const r = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  if (r.status === 401) toLogin();
  if (!r.ok) await fail(r);
  return r.json();
}
