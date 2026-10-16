import type { PhotoBrowseRow as WirePhotoBrowseRow } from "./generated/docbank.js";
import { groupBursts, type PhotoStack } from "./photoStacks.js";

export type PhotoBrowseRow = WirePhotoBrowseRow & { stack?: PhotoStack };

export function photoRows(
  page: { items: WirePhotoBrowseRow[]; burst_gap_seconds?: number },
  grouped = false,
): PhotoBrowseRow[] {
  const stacks = new Map<string, PhotoStack>();
  if (grouped)
    for (const stack of groupBursts(page.items, page.burst_gap_seconds ?? 2))
      if (stack.asset_ids.length > 1) for (const id of stack.asset_ids) stacks.set(id, stack);
  return page.items.map((row) => ({ ...row, stack: stacks.get(row.asset_id) }));
}
