import type { Node } from "../api/types";
import { Icon, closeSidebarOnMobile } from "./shell";

// The sidebar file tree. The chevron only folds; the row selects (opens a
// folder's listing / a file). Clicking the folder whose listing is already
// showing folds/unfolds it, like a plain tree.
export function FileTree(props: {
  root: Node | undefined;
  expanded: Set<string>;
  onToggle: (path: string) => void;
  currentPath: string;
  listingShowing: boolean; // current view is a folder listing
  onOpen: (path: string) => void;
}) {
  return (
    <nav id="tree" aria-label="Files">
      {props.root && <TreeChildren nodes={props.root.children || []} {...props} />}
    </nav>
  );
}

type RowProps = Omit<Parameters<typeof FileTree>[0], "root">;

function TreeChildren({ nodes, ...rest }: RowProps & { nodes: Node[] }) {
  return (
    <ul>
      {nodes.map((n) => (
        <TreeNode key={n.path} node={n} {...rest} />
      ))}
    </ul>
  );
}

function TreeNode({ node: n, ...rest }: RowProps & { node: Node }) {
  const { expanded, onToggle, currentPath, listingShowing, onOpen } = rest;
  const open = n.dir ? expanded.has(n.path) : false;
  const click = () => {
    if (n.dir) {
      // Folding beats re-opening when this folder's listing is already up.
      if (currentPath === n.path && listingShowing) {
        onToggle(n.path);
        return;
      }
    }
    onOpen(n.path);
    if (!n.dir) closeSidebarOnMobile();
  };
  return (
    <li className={(n.dir ? "dir" : "file") + (n.dir && !open ? " collapsed" : "")}>
      <div
        className={"row" + (currentPath === n.path ? " active" : "")}
        data-path={n.path}
        tabIndex={0}
        role="button"
        title={n.name}
        aria-expanded={n.dir ? open : undefined}
        onClick={click}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            click();
          }
        }}
      >
        <span
          className="chev"
          onClick={(e) => {
            if (!n.dir) return;
            e.stopPropagation();
            onToggle(n.path);
          }}
        >
          <Icon name="chevd" />
        </span>
        <span className="ticon">
          <Icon name={n.dir ? "folder" : "doc"} />
        </span>
        <span className="label">{n.name}</span>
      </div>
      {n.dir && <TreeChildren nodes={n.children || []} {...rest} />}
    </li>
  );
}

/* Every ancestor folder of a path (for unfolding the way to it). */
export function ancestorsOf(filePath: string): string[] {
  const parts = filePath.split("/");
  const out: string[] = [];
  let acc = "";
  for (let i = 0; i < parts.length - 1; i++) {
    acc = acc ? acc + "/" + parts[i] : parts[i];
    out.push(acc);
  }
  return out;
}
