import { Icon, RailButton } from "./shell";

// The reader's pinned paths (lib/visits), each with what needs attention on
// it: open review threads and conflict copies. A pin whose path is gone from
// the tree stays listed but dimmed, so a rename does not silently drop it.

export interface PinBadges {
  comments: Map<string, number>;
  conflicts: Map<string, number>;
}

const plural = (n: number, w: string) => n + " " + w + (n === 1 ? "" : "s");

export function Pinned(props: {
  pins: string[];
  badges: PinBadges;
  exists: (path: string) => boolean;
  currentPath: string;
  onOpen: (path: string) => void;
  onUnpin: (path: string) => void;
}) {
  const { pins, badges, exists, currentPath, onOpen, onUnpin } = props;
  if (!pins.length) return null;
  return (
    <section id="pinned" aria-label="Pinned">
      <div className="sb-section">Pinned</div>
      {pins.map((p) => {
        const name = p.slice(p.lastIndexOf("/") + 1);
        const c = badges.comments.get(p) || 0;
        const k = badges.conflicts.get(p) || 0;
        const gone = !exists(p);
        return (
          <div
            key={p}
            className={"row pin" + (p === currentPath ? " active" : "") + (gone ? " gone" : "")}
            data-pin={p}
            role="button"
            tabIndex={0}
            title={gone ? p + " — no longer in this project" : p}
            onClick={() => !gone && onOpen(p)}
            onKeyDown={(e) => {
              if ((e.key === "Enter" || e.key === " ") && !gone) {
                e.preventDefault();
                onOpen(p);
              }
            }}
          >
            <Icon name="star" />
            <span className="label">{name}</span>
            {c > 0 && <span className="pin-badge">{plural(c, "comment")}</span>}
            {k > 0 && <span className="pin-badge danger">{plural(k, "conflict")}</span>}
            <button
              className="tact pin-x"
              aria-label={"Unpin " + name}
              title="Unpin"
              tabIndex={-1}
              onClick={(e) => {
                e.stopPropagation();
                onUnpin(p);
              }}
            >
              <Icon name="x" />
            </button>
          </div>
        );
      })}
    </section>
  );
}

export function PinnedRail(props: { pins: string[]; badges: PinBadges; onOpen: (path: string) => void }) {
  const { pins, badges, onOpen } = props;
  if (!pins.length) return null;
  return (
    <>
      <span className="rail-sep" />
      {pins.map((p) => {
        const name = p.slice(p.lastIndexOf("/") + 1);
        const c = badges.comments.get(p) || 0;
        const k = badges.conflicts.get(p) || 0;
        const hint = [c && plural(c, "comment"), k && plural(k, "conflict")].filter(Boolean).join(" · ");
        return (
          <RailButton
            key={p}
            icon="doc"
            label={name}
            hint={hint || undefined}
            badge={k || c || undefined}
            danger={k > 0}
            onClick={() => onOpen(p)}
          />
        );
      })}
    </>
  );
}
