import type { Node } from "./generated/docbank.js";
import { restoreNode, trashNode } from "./receipts.js";

export function parentPath(path: string): string {
  const split = path.lastIndexOf("/");
  return split <= 0 ? "/" : path.slice(0, split);
}

export function trashedMessage(node: Pick<Node, "name">): string {
  return `Moved “${node.name}” to trash`;
}

// The restore response carries where the item landed; the message reports that.
export function restoredMessage(
  trashed: Pick<Node, "photo_asset_id" | "photo_file_count">,
  restored: Pick<Node, "path">,
): string {
  const files = trashed.photo_asset_id ? (trashed.photo_file_count ?? 0) : 0;
  if (files > 1) return `Restored ${files} files`;
  return `Restored to ${parentPath(restored.path ?? "/")}`;
}

export async function moveToTrash(
  session: string,
  node: Node,
): Promise<{ receipt: Node; message: string }> {
  const receipt = await trashNode(session, node.id, node.revision);
  return { receipt, message: trashedMessage(node) };
}

// Undo goes through the same restore call as the Trash drawer.
export async function undoTrash(
  session: string,
  receipt: Node,
): Promise<{ receipt: Node; notice: string }> {
  const restored = await restoreNode(session, receipt.id, receipt.revision);
  return { receipt: restored, notice: restoredMessage(receipt, restored) };
}
