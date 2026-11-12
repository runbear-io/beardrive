import type { ChangeFileDelta, Node } from "../api/types";

const cloneTreeNode = (n: Node): Node => ({
  ...n,
  children: n.children?.map(cloneTreeNode),
});

const sortTreeNodes = (n: Node): void => {
  if (!n.children) return;
  n.children.sort((a, b) => {
    if (!!a.dir !== !!b.dir) return a.dir ? -1 : 1;
    const an = a.name.toLowerCase();
    const bn = b.name.toLowerCase();
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
  for (const c of n.children) if (c.dir) sortTreeNodes(c);
};

const refreshDirectoryTimes = (n: Node): string | undefined => {
  if (!n.dir) return n.time;
  let newest = "";
  for (const c of n.children || []) {
    const time = refreshDirectoryTimes(c);
    if (!time) continue;
    if (!newest || (Date.parse(time) || 0) > (Date.parse(newest) || 0)) newest = time;
  }
  n.time = newest || undefined;
  return n.time;
};

/** Apply a complete small SSE change frame without fetching /tree. */
export function applyTreeDeltas(root: Node, deltas: ChangeFileDelta[]): Node {
  const next = cloneTreeNode(root);
  if (!next.children) next.children = [];

  for (const delta of deltas) {
    const parts = delta.path.split("/").filter(Boolean);
    if (!parts.length) continue;
    const leaf = parts[parts.length - 1];
    if (delta.op === "put") {
      let parent = next;
      for (let i = 0; i < parts.length - 1; i++) {
        const name = parts[i];
        const path = parts.slice(0, i + 1).join("/");
        let child = parent.children!.find((c) => c.name === name);
        if (!child || !child.dir) {
          child = { name, path, dir: true, children: [] };
          parent.children = parent.children!.filter((c) => c.name !== name);
          parent.children!.push(child);
        } else if (!child.children) {
          child.children = [];
        }
        parent = child;
      }
      const file: Node = {
        name: leaf,
        path: delta.path,
        size: delta.size,
        time: delta.time,
        user: delta.user,
        user_name: delta.user_name,
        author: delta.author,
        device: delta.device,
        link: delta.link,
      };
      parent.children = (parent.children || []).filter((c) => c.name !== leaf);
      parent.children.push(file);
    } else if (delta.op === "delete") {
      const remove = (dir: Node, at: number): boolean => {
        if (!dir.children) return false;
        const name = parts[at];
        if (at === parts.length - 1) {
          const before = dir.children.length;
          dir.children = dir.children.filter((c) => c.name !== name || c.dir);
          return before !== dir.children.length && dir.children.length === 0;
        }
        const child = dir.children.find((c) => c.name === name && c.dir);
        if (!child || !remove(child, at + 1)) return false;
        dir.children = dir.children.filter((c) => c !== child);
        return dir.children.length === 0;
      };
      remove(next, 0);
    }
    sortTreeNodes(next);
    refreshDirectoryTimes(next);
  }
  return next;
}
