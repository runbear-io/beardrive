export const MD_EXT = /\.(md|markdown)$/i;
export const IMG_EXT = /\.(png|jpe?g|gif|svg|webp|ico|bmp|avif)$/i;
export const HTML_EXT = /\.html?$/i;
export const PDF_EXT = /\.pdf$/i;
export const TEXT_EXT =
  /\.(txt|log|json|ya?ml|toml|csv|go|py|js|ts|jsx|tsx|sh|bash|zsh|rb|rs|c|h|cpp|java|kt|swift|sql|css|xml|ini|conf|env|mod|sum|jsonl)$/i;

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

/* Who made a change, as history renders it everywhere: the account, with
   the display name in front when the server knows one, falling back to the
   git/OS identity of an offline device. */
export function whoChanged(e: { user?: string; user_name?: string; author?: string }): string {
  return e.user_name ? `${e.user_name} <${e.user}>` : e.user || e.author || "unknown";
}
