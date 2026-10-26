import * as generated from "./generated/docbank.js";
import type { Node } from "./generated/docbank.js";
import { restoreNode, trashNode } from "./receipts.js";

export function parentPath(path: string): string {
  const split = path.lastIndexOf("/");
  return split <= 0 ? "/" : path.slice(0, split);
}

export function trashedMessage(node: Pick<Node, "name">, items?: number): string {
  const count = items ? ` (${items} item${items === 1 ? "" : "s"})` : "";
  return `Moved “${node.name}”${count} to trash`;
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
): Promise<{ receipt: Node; message: string }> {
  const items = await itemCount(session, node);
  const receipt = await trashNode(session, node.id, node.revision);
  return { receipt, message: trashedMessage(node, items) };
}

// Undo goes through the same restore call as the Trash drawer.
export async function undoTrash(
  session: string,
  receipt: Node,
): Promise<{ receipt: Node; notice: string }> {
  const restored = await restoreNode(session, receipt.id, receipt.revision);
  return { receipt: restored, notice: restoredMessage(receipt, restored) };
}

export async function restoreFromTrash(
  session: string,
  node: Node,
): Promise<{ receipt: Node; message: string }> {
  const receipt = await restoreNode(session, node.id, node.revision);
  return { receipt, message: restoredMessage(node, receipt) };
}
