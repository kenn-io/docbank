import type {
  PhotoBrowseRow as WirePhotoBrowseRow,
  PhotoStack,
  PhotoAuthoredReceipt,
} from "./generated/docbank.js";

export type { PhotoStack };
export type PhotoBrowseRow = WirePhotoBrowseRow & { stack?: PhotoStack };

/** Attach each server-described burst to its member rows; other rows stay single. */
export function photoRows(page: {
  items: WirePhotoBrowseRow[];
  stacks?: PhotoStack[];
}): PhotoBrowseRow[] {
  const stacks = new Map<string, PhotoStack>();
  for (const stack of page.stacks ?? []) for (const id of stack.asset_ids) stacks.set(id, stack);
  return page.items.map((row) => ({ ...row, stack: stacks.get(row.asset_id) }));
}

/** The latest explicit choice, else the only picked frame, else the first frame. */
function representative(stack: Omit<PhotoStack, "representative_id">) {
  const ranks = Object.entries(stack.representative_ranks ?? {});
  if (ranks.length) return ranks.reduce((best, entry) => (entry[1] > best[1] ? entry : best))[0];
  return stack.picked_ids.length === 1 ? stack.picked_ids[0]! : stack.asset_ids[0]!;
}

/**
 * Patch loaded rows from an accepted receipt. `picked` reports the asset's
 * current pick state when the caller knows all of its files.
 */
export function patchPhotoRows(
  rows: PhotoBrowseRow[],
  receipt: PhotoAuthoredReceipt,
  picked: (assetID: string) => boolean | undefined,
): PhotoBrowseRow[] {
  const files = new Map(receipt.after.map((file) => [file.file_id, file.values]));
  const choices = new Map(
    (receipt.representatives ?? []).map((change) => [change.asset_id, change]),
  );
  const stacks = new Map<PhotoStack, PhotoStack>();
  for (const row of rows) {
    const choice = choices.get(row.asset_id),
      pick = picked(row.asset_id);
    if (!row.stack || (pick === undefined && !choice)) continue;
    let next = stacks.get(row.stack);
    if (!next) stacks.set(row.stack, (next = { ...row.stack }));
    if (pick !== undefined)
      next.picked_ids = [
        ...next.picked_ids.filter((id) => id !== row.asset_id),
        ...(pick ? [row.asset_id] : []),
      ];
    if (choice) {
      const ranks = { ...next.representative_ranks };
      if (choice.after) ranks[row.asset_id] = choice.after;
      else delete ranks[row.asset_id];
      next.representative_ranks = ranks;
    }
  }
  const patched = rows.map((row) => {
    const values = files.get(row.display_file_id),
      choice = choices.get(row.asset_id),
      stack = row.stack && (stacks.get(row.stack) ?? row.stack);
    if (!values && !choice && stack === row.stack) return row;
    return {
      ...row,
      ...(values && { flag: values.flag, rating: values.rating, rotation: values.rotation }),
      ...(choice && { revision: choice.after_revision }),
      stack,
    };
  });
  for (const stack of stacks.values()) stack.representative_id = representative(stack);
  return patched;
}
