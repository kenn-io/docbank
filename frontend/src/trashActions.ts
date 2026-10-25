import * as generated from "./generated/docbank.js";
import type { Node } from "./generated/docbank.js";
import { restoreNode, trashNode } from "./receipts.js";

// The daemon restores to a node's original folder when it still exists and to
// the top folder otherwise, and it does not say which. The browser remembers
// where it trashed things from so the restore message can.
const origins = new Map<number, string>();

export function parentPath(path: string): string {
  const split = path.lastIndexOf("/");
  return split <= 0 ? "/" : path.slice(0, split);
}

export function trashedMessage(node: Pick<Node, "name">, items?: number): string {
  const count = items ? ` (${items} item${items === 1 ? "" : "s"})` : "";
  return `Moved “${node.name}”${count} to trash`;
}

export function restoredMessage(
  trashed: Pick<Node, "id" | "photo_asset_id" | "photo_file_count">,
  restored: Pick<Node, "path">,
  origin = origins.get(trashed.id),
): string {
  const files = trashed.photo_asset_id ? (trashed.photo_file_count ?? 0) : 0;
  if (files > 1) return `Restored ${files} files`;
  const destination = parentPath(restored.path ?? "/");
  return origin && origin !== destination
    ? `Restored to ${destination} — ${origin} no longer exists`
    : `Restored to ${destination}`;
}

// A folder's direct item count, as the browser shows it. Missing counts leave
// the message plain rather than blocking the move.
async function itemCount(session: string, node: Node): Promise<number | undefined> {
  if (node.kind !== "dir") return undefined;
  try {
    return (await generated.listChildren(node.id, { limit: 1, offset: 0 }, { session })).total;
  } catch {
    return undefined;
  }
}

export async function moveToTrash(
  session: string,
  node: Node,
  path: string,
): Promise<{ receipt: Node; message: string }> {
  const items = await itemCount(session, node);
  const receipt = await trashNode(session, node.id, node.revision);
  origins.set(node.id, parentPath(path));
  return { receipt, message: trashedMessage(node, items) };
}

// Undo goes through the same restore call as the Trash drawer. It speaks up
// only when the folder it came from is gone.
export async function undoTrash(
  session: string,
  receipt: Node,
): Promise<{ receipt: Node; notice?: string }> {
  const restored = await restoreNode(session, receipt.id, receipt.revision);
  const origin = origins.get(receipt.id);
  origins.delete(receipt.id);
  const moved = !!origin && origin !== parentPath(restored.path ?? "/");
  return {
    receipt: restored,
    notice: moved ? restoredMessage(receipt, restored, origin) : undefined,
  };
}

export async function restoreFromTrash(
  session: string,
  node: Node,
): Promise<{ receipt: Node; message: string }> {
  const receipt = await restoreNode(session, node.id, node.revision);
  const message = restoredMessage(node, receipt);
  origins.delete(node.id);
  return { receipt, message };
}
