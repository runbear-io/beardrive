export const MD_EXT = /\.(md|markdown)$/i;
export const IMG_EXT = /\.(png|jpe?g|gif|svg|webp|ico|bmp|avif)$/i;
export const HTML_EXT = /\.html?$/i;
export const PDF_EXT = /\.pdf$/i;
// Checked BEFORE TEXT_EXT (csv is in both): a parse failure falls back to
// the same plain-text view TEXT_EXT would have given. Delimiter comes from
// the extension, never from sniffing the bytes.
export const CSV_EXT = /\.(csv|tsv)$/i;
export const TEXT_EXT =
  /\.(txt|log|json|ya?ml|toml|csv|go|py|js|ts|jsx|tsx|sh|bash|zsh|rb|rs|c|h|cpp|java|kt|swift|sql|css|xml|ini|conf|env|mod|sum|jsonl)$/i;

/* ⌘ on Apple platforms, Ctrl everywhere else. Drives the keyboard-shortcut
   labels the UI shows (e.g. the sidebar toggle's ⌘B / Ctrl+B), so a Windows
   user is never told to press a key their keyboard does not have. The actual
   shortcut handlers accept metaKey OR ctrlKey, so this is a label concern
   only. `navigator.platform` is deprecated but still the most reliable signal
   and we fall back to the UA string. */
export const isMac =
  typeof navigator !== "undefined" &&
  /Mac|iPhone|iPad|iPod/i.test(navigator.platform || navigator.userAgent || "");

export function humanSize(n: number): string {
  if (n < 1024) return n + " B";
  const units = ["KB", "MB", "GB", "TB"];
  let i = -1;
  do {
    n /= 1024;
    i++;
  } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(1) + " " + units[i];
}

// Resolve a relative link against a directory, folding "." and "..".
export function joinPath(dir: string, rel: string): string {
  const parts = (dir ? dir.split("/") : []).concat(rel.split("/"));
  const out: string[] = [];
  for (const s of parts) {
    if (s === "" || s === ".") continue;
    if (s === "..") out.pop();
    else out.push(s);
  }
  return out.join("/");
}

/* A heading's id, as the hub renders it (markdown.go headingIDs): trimmed,
   NFC, ASCII letters/digits lowercased, ASCII space/-/_ each to "-", any other
   letter or number kept lowercased, everything else dropped, "heading" when
   nothing is left. The -N suffix for a repeated heading is per document and
   cannot be known from the text, so it is not applied here. */
export function headingSlug(text: string): string {
  let out = "";
  for (const ch of text.trim().normalize("NFC")) {
    if (/^[A-Za-z0-9]$/.test(ch)) out += ch.toLowerCase();
    else if (/^[ \t\n\r\f\v_-]$/.test(ch)) out += "-";
    else if (ch > "\x7f" && /^[\p{L}\p{N}]$/u.test(ch)) out += ch.toLowerCase();
  }
  return out || "heading";
}

/* Split an Obsidian wikilink target into the file part and the fragment:
   [[doc#Some Heading]] -> "doc", "#some-heading"; [[doc#^block]] -> "doc",
   "#block" (a block reference). A bare [[#Heading]] has an empty file part:
   the document it is written in. */
export function splitWikiTarget(target: string): { file: string; fragment: string } {
  const i = target.search(/[#^]/);
  if (i < 0) return { file: target.trim(), fragment: "" };
  const file = target.slice(0, i).trim();
  const rest = target.slice(i).replace(/^#?\^/, "").replace(/^#/, "").trim();
  if (!rest) return { file, fragment: "" };
  const block = target[i] === "^" || target.slice(i).startsWith("#^");
  return { file, fragment: "#" + (block ? rest : headingSlug(rest)) };
}

/* Obsidian-style wikilink target -> file. Exact path first, then basename;
   ".md" is optional on both; everything case-insensitive, and compared in
   NFC — a file synced from a Mac carries an NFD name, which the same visible
   target must still find. A #heading or ^block suffix is ignored here (see
   resolveWikiTarget). The rules are the product decision, so they live in one
   place and get tested without a browser (node --test has no DOM, so this
   cannot live in FileView). */
export function resolveWiki<F extends { path: string; name: string }>(target: string, files: F[]): F | undefined {
  const want = splitWikiTarget(target).file.normalize("NFC").toLowerCase();
  if (!want) return undefined;
  const fold = (s: string) => s.normalize("NFC").toLowerCase();
  return (
    files.find((f) => {
      const p = fold(f.path);
      return p === want || p === want + ".md";
    }) ||
    files.find((f) => {
      const n = fold(f.name);
      return n === want || n === want + ".md";
    })
  );
}

/* A wikilink target resolved to the file's ACTUAL path (its own Unicode
   form, which is what the tree and the URL use) plus any #fragment. `path`
   is "" for a fragment-only link into the current document; undefined is a
   miss. */
export function resolveWikiTarget(
  target: string,
  files: { path: string; name: string }[],
): { path: string; fragment: string } | undefined {
  const { file, fragment } = splitWikiTarget(target);
  if (!file) return fragment ? { path: "", fragment } : undefined;
  const hit = resolveWiki(target, files);
  return hit ? { path: hit.path, fragment } : undefined;
}

/* clipboard copy that never throws on a non-HTTPS origin (where
   navigator.clipboard is undefined). Returns true on success. */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    /* fall through */
  }
  return false;
}

/* Last project opened on this browser. localStorage throws in Safari's
   private mode and wherever storage is disabled, so both sides swallow — a
   preference is never worth a broken page. Origin-scoped already, so two
   hubs never share an answer. */
const LAST_PROJECT = "bdrive.lastProject";

export function lastProject(): string {
  try {
    return localStorage.getItem(LAST_PROJECT) || "";
  } catch {
    return "";
  }
}

export function rememberProject(id: string) {
  try {
    localStorage.setItem(LAST_PROJECT, id);
  } catch {
    /* preference only */
  }
}

// The width at which the file page's rail (FileRail.tsx) sits beside the
// prose instead of folding into a disclosure above it.
export const FM_RAIL = "(min-width: 1400px)"; // must match style.css

/* Who made a change, as history renders it everywhere: the account, with
   the display name in front when the server knows one, falling back to the
   git/OS identity of an offline device. */
export function whoChanged(e: { user?: string; user_name?: string; author?: string }): string {
  return e.user_name ? `${e.user_name} <${e.user}>` : e.user || e.author || "unknown";
}

/* The name a person reads as a person: the account's display name, else the
   local part of its email, else the device's git/OS identity. The full
   identity (whoChanged) belongs in hover text. */
export function displayName(e: { user?: string; user_name?: string; author?: string }): string {
  if (e.user_name) return e.user_name;
  if (e.user) return e.user.split("@")[0] || e.user;
  return e.author || "unknown";
}

/* "Sep 30, 2026 · 8:53 AM" in the viewer's locale and timezone. */
export function absTime(t: string | number | Date, locale?: string): string {
  const d = new Date(t);
  if (isNaN(d.getTime())) return "";
  const date = new Intl.DateTimeFormat(locale, { month: "short", day: "numeric", year: "numeric" }).format(d);
  const time = new Intl.DateTimeFormat(locale, { hour: "numeric", minute: "2-digit" }).format(d);
  return date + " · " + time;
}
