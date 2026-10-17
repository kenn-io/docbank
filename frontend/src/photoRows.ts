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

/** Patch file values and authoritative asset revisions from an accepted receipt. */
export function patchPhotoRows(
  rows: PhotoBrowseRow[],
  receipt: PhotoAuthoredReceipt,
): PhotoBrowseRow[] {
  const files = new Map(receipt.after.map((file) => [file.file_id, file.values]));
  const choices = new Map(
    (receipt.representatives ?? []).map((change) => [change.asset_id, change]),
  );
  return rows.map((row) => {
    const values = files.get(row.display_file_id),
      choice = choices.get(row.asset_id);
    if (!values && !choice) return row;
    return {
      ...row,
      ...(values && { flag: values.flag, rating: values.rating, rotation: values.rotation }),
      ...(choice && { revision: choice.after_revision }),
    };
  });
}
