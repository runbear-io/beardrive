// Conflict copies: the one guarantee the shared-folder promise rests on.
// When two devices edit the same file between syncs, the loser is preserved
// beside the winner as `<name>.bdrive-conflict-<device>-<utc>` — see
// conflictName in internal/syncer/syncer.go. That name is a pure function of
// the path, so the hub can recognise a conflict copy, and recover the device
// and the moment, from the string alone: no server route, no journal field.
// Unit-tested in conflict.test.ts (`npm test`).

export type Conflict = {
  original: string; // the path the winning version lives at
  device: string; // the device whose edit was preserved
  when: Date; // when that edit was made (UTC in the name)
};

// Anchored at the end — a path that merely CONTAINS the string somewhere in
// the middle (a folder named after a conflict copy) is an ordinary file.
// The character class mirrors syncer.go's sanitize (everything outside
// [A-Za-z0-9_-] becomes '-') and the 32 mirrors its clip.
const RE = /\.bdrive-conflict-([A-Za-z0-9_-]{0,32})-(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/;

/* The conflict copy behind a path, or null for an ordinary file. Anything
   malformed — a truncated suffix, a device name too long, a date that isn't
   one — is "not a conflict copy", never a throw. */
export function parseConflict(path: string): Conflict | null {
  const m = RE.exec(path);
  if (!m) return null;
  const [, device, y, mo, d, h, mi, s] = m;
  // 20260814T060945Z is not ISO-8601, and new Date() on it is
  // implementation-defined — build it by field instead, then round-trip to
  // reject the impossible (month 13, February 30, hour 25).
  const when = new Date(Date.UTC(+y, +mo - 1, +d, +h, +mi, +s));
  if (
    when.getUTCFullYear() !== +y ||
    when.getUTCMonth() !== +mo - 1 ||
    when.getUTCDate() !== +d ||
    when.getUTCHours() !== +h ||
    when.getUTCMinutes() !== +mi ||
    when.getUTCSeconds() !== +s
  ) {
    return null;
  }
  return { original: path.slice(0, m.index), device, when };
}

/* The inverse: the name a losing version is preserved under.

   Must agree with conflictName in internal/syncer/syncer.go character for
   character — a browser and a syncing device produce copies that sit in the
   same folder and are read by the same parser above, so a second format would
   be a second thing to explain to the same reader. Round-tripped through
   parseConflict in conflict.test.ts.

   Both variable parts are bounded the way the Go does it: the device is
   sanitized (everything outside [A-Za-z0-9_-] becomes '-') and then clipped
   to 32, and the base name is clipped so the whole thing stays under 255. */
export function conflictName(path: string, device: string, when: Date): string {
  const p2 = (n: number) => String(n).padStart(2, "0");
  const stamp =
    String(when.getUTCFullYear()) +
    p2(when.getUTCMonth() + 1) +
    p2(when.getUTCDate()) +
    "T" +
    p2(when.getUTCHours()) +
    p2(when.getUTCMinutes()) +
    p2(when.getUTCSeconds()) +
    "Z";
  const suffix =
    ".bdrive-conflict-" +
    device.replace(/[^A-Za-z0-9_-]/g, "-").slice(0, 32) +
    "-" +
    stamp;
  const cut = path.lastIndexOf("/");
  const dir = cut < 0 ? "" : path.slice(0, cut + 1);
  const base = cut < 0 ? path : path.slice(cut + 1);
  return dir + base.slice(0, Math.max(0, 255 - suffix.length)) + suffix;
}
