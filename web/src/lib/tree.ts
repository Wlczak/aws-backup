// Build a nested folder tree from flat file rows. The Files page uses
// this to render a file-system-like view with expandable folders and
// folder-level checkbox selection.
//
// Paths are split on both `/` and `\` so a mixed set of Unix + Windows
// paths still groups consistently. Leading slashes are stripped so
// `/foo/bar.txt` and `foo/bar.txt` share the same tree.
import type { FileRow } from './api';

export interface TreeNode {
  name: string;         // last path segment; '' for root
  path: string;         // full path (folder) or file.path (file)
  isFolder: boolean;
  children: TreeNode[]; // sorted: folders first, then files, each alphabetically
  file?: FileRow;       // set only on leaves
  fileCount: number;    // leaves in subtree
  totalSize: number;    // sum of file sizes in subtree
}

function splitPath(p: string): string[] {
  const parts = p.split(/[\\/]+/).filter((s) => s.length > 0);
  return parts;
}

export function buildTree(files: FileRow[]): TreeNode {
  const root: TreeNode = {
    name: '',
    path: '',
    isFolder: true,
    children: [],
    fileCount: 0,
    totalSize: 0,
  };
  const byPath = new Map<string, TreeNode>();
  byPath.set('', root);

  for (const f of files) {
    const parts = splitPath(f.path);
    if (parts.length === 0) continue;
    let parent = root;
    let cur = '';
    for (let i = 0; i < parts.length - 1; i++) {
      cur = cur === '' ? parts[i] : `${cur}/${parts[i]}`;
      let node = byPath.get(cur);
      if (!node) {
        node = {
          name: parts[i],
          path: cur,
          isFolder: true,
          children: [],
          fileCount: 0,
          totalSize: 0,
        };
        byPath.set(cur, node);
        parent.children.push(node);
      }
      parent = node;
    }
    const leaf: TreeNode = {
      name: parts[parts.length - 1],
      path: f.path,
      isFolder: false,
      children: [],
      file: f,
      fileCount: 1,
      totalSize: f.size,
    };
    parent.children.push(leaf);
  }

  // Roll up counts + sort.
  rollup(root);
  sortTree(root);
  return root;
}

function rollup(node: TreeNode): { count: number; size: number } {
  if (!node.isFolder) return { count: 1, size: node.totalSize };
  let count = 0;
  let size = 0;
  for (const c of node.children) {
    const r = rollup(c);
    count += r.count;
    size += r.size;
  }
  node.fileCount = count;
  node.totalSize = size;
  return { count, size };
}

function sortTree(node: TreeNode) {
  if (!node.isFolder) return;
  node.children.sort((a, b) => {
    if (a.isFolder !== b.isFolder) return a.isFolder ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
  for (const c of node.children) sortTree(c);
}

// collectFiles returns every file row under the given node (itself if it is a file).
export function collectFiles(node: TreeNode): FileRow[] {
  if (!node.isFolder) return node.file ? [node.file] : [];
  const out: FileRow[] = [];
  const stack: TreeNode[] = [...node.children];
  while (stack.length) {
    const n = stack.pop()!;
    if (n.isFolder) stack.push(...n.children);
    else if (n.file) out.push(n.file);
  }
  return out;
}
