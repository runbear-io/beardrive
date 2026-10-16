import type { Person } from "../hooks/usePresence";
import { personHue } from "../lib/meeting";

/* Who else is here, as initials.

   Deliberately small and unclickable: it answers "am I about to collide with
   someone" and nothing else. Names are display names the hub already shows
   every project member in History — the roster carries no account key. */

// initials takes the first letter of the first two words, so "Ada Lovelace"
// is AL and "ada@x.io" is A. Never more than two: the circle is 24px.
function initials(name: string): string {
  const words = name.trim().split(/[\s@._-]+/).filter(Boolean);
  if (!words.length) return "?";
  return (words[0][0] + (words[1]?.[0] ?? "")).toUpperCase();
}

// A stable colour per person, so the same teammate is the same colour across
// reloads and across everyone's screen. Hash, not an index: an index would
// re-colour everybody whenever one person left. The editor's carets and
// section avatars use the same hash (lib/meeting.ts personHue).
const hue = personHue;

// One person's initials in their colour. `.pavatar` in style.css draws the
// same circle for widgets CodeMirror builds outside React.
export function Avatar({ name, title }: { name: string; title?: string }) {
  return (
    <span className="pavatar" title={title ?? name} style={{ ["--ph" as string]: hue(name) }}>
      {initials(name)}
    </span>
  );
}

export function PresenceBar({ people, path }: { people: Person[]; path?: string }) {
  if (!people.length) return null;
  // Whoever is on the file you are on comes first — that is the collision the
  // bar exists to warn about.
  const sorted = [...people].sort((a, b) => {
    const ax = path && a.path === path ? 0 : 1;
    const bx = path && b.path === path ? 0 : 1;
    return ax - bx;
  });
  const shown = sorted.slice(0, 5);
  const here = path ? people.filter((p) => p.path === path).length : 0;
  const rest = sorted.length - shown.length;
  return (
    <div id="presence" className="flex items-center gap-1" aria-label="People viewing this project">
      {shown.map((p, i) => (
        <span
          key={p.name + i}
          title={p.path ? `${p.name} — ${p.path}` : p.name}
          className="inline-flex h-6 w-6 items-center justify-center rounded-full text-[10px] font-medium ring-1 ring-black/10"
          style={{
            backgroundColor: `hsl(${hue(p.name)} 70% 88%)`,
            color: `hsl(${hue(p.name)} 60% 28%)`,
            // Same file as the reader: ring it so the overlap is visible
            // without reading any names.
            outline: path && p.path === path ? "2px solid hsl(var(--ring))" : undefined,
            outlineOffset: "1px",
          }}
        >
          {initials(p.name)}
        </span>
      ))}
      {rest > 0 && <span className="text-xs text-muted-foreground">+{rest}</span>}
      {here > 0 && (
        <span className="presence-count" title={`${here} ${here === 1 ? "person is" : "people are"} on this file`}>
          {here} viewing
        </span>
      )}
    </div>
  );
}
