import { useEffect } from "react";
import type { HeatMap, Node } from "../api/types";
import { heatFor, heatLevel, heatText, useFolderHistory } from "../hooks/useBrowse";
import { humanSize } from "../util";
import { Icon } from "./shell";
import { HistoryRow } from "./HistoryRow";

export function FolderListing(props: {
  node: Node;
  heatMap: HeatMap | null;
  hub: boolean; // hub feeds exist; a plain-folder viewer has no journals
  apiBase: string;
  onOpen: (path: string) => void;
  onFullHistory: (prefix: string) => void;
  onRendered?: () => void; // scroll restoration: content height just grew
}) {
  const { node, heatMap, onOpen } = props;
  const kids = (node.children || [])
    .slice()
    .sort((a, b) => Number(b.dir || false) - Number(a.dir || false) || a.name.localeCompare(b.name));
  const dirs = kids.filter((c) => c.dir).length;
  const files = kids.length - dirs;
  const counts: string[] = [];
  if (dirs) counts.push(dirs + (dirs === 1 ? " folder" : " folders"));
  if (files) counts.push(files + (files === 1 ? " file" : " files"));
  const folderHeat = heatFor(heatMap, node.path, true);
  if (folderHeat) counts.push(heatText(folderHeat) + " in 30 days");

  return (
    <div className="dirlist">
      <h1 className="dl-title">
        <span className="dl-title-icon">
          <Icon name="folder" />
        </span>
        <span>{node.name}</span>
      </h1>
      <p className="dl-sub">{counts.join(" · ") || "Empty folder"}</p>
      {kids.length === 0 ? (
        <div className="dl-empty">Nothing in this folder yet.</div>
      ) : (
        <div className="dl-items">
          {kids.map((c) => {
            let meta = "";
            if (c.dir) {
              const n = (c.children || []).length;
              meta = n + (n === 1 ? " item" : " items");
            } else {
              meta = [c.size ? humanSize(c.size) : "", c.time ? new Date(c.time).toLocaleDateString() : ""]
                .filter(Boolean)
                .join(" · ");
            }
            const he = heatFor(heatMap, c.path, !!c.dir);
            if (he) meta = heatText(he) + (meta ? " · " + meta : "");
            return (
              <div
                key={c.path}
                className="dl-row"
                tabIndex={0}
                role="button"
                title={c.path}
                onClick={() => onOpen(c.path)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    onOpen(c.path);
                  }
                }}
              >
                <span className="ticon">
                  <Icon name={c.dir ? "folder" : "doc"} />
                </span>
                <span className="dl-name">{c.name}</span>
                {he && <span className={"heatdot lvl" + heatLevel(he)} title={heatText(he) + " in 30 days"} />}
                <span className="dl-meta">{meta}</span>
              </div>
            );
          })}
        </div>
      )}
      {props.hub && (
        <FolderHistory
          apiBase={props.apiBase}
          prefix={node.path + "/"}
          onOpen={onOpen}
          onFullHistory={() => props.onFullHistory(node.path + "/")}
          onRendered={props.onRendered}
        />
      )}
    </div>
  );
}

/* The folder's change feed, straight from the journals: files added,
   edited, and deleted anywhere under it, newest first. */
function FolderHistory(props: {
  apiBase: string;
  prefix: string;
  onOpen: (path: string) => void;
  onFullHistory: () => void;
  onRendered?: () => void;
}) {
  const entries = useFolderHistory(props.apiBase, props.prefix, true);
  const { onRendered } = props;
  useEffect(() => {
    // The feed adds height after the listing rendered; a restored scroll
    // position (back/forward) may only fit now.
    if (entries && entries.length && onRendered) onRendered();
  }, [entries, onRendered]);
  if (!entries || entries.length === 0) return null;
  return (
    <div className="dl-history">
      <h3 className="dl-h3">Recent changes</h3>
      <div className="history dl-hlist">
        {entries.map((e, i) => (
          <HistoryRow key={i} entry={e} onOpen={props.onOpen} />
        ))}
      </div>
      <button className="ai-btn dl-more" onClick={props.onFullHistory}>
        Full history
      </button>
    </div>
  );
}
