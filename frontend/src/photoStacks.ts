import type { PhotoBrowseRow } from "./generated/docbank.js";

export interface PhotoStack {
  asset_ids: string[];
  representative_id: string;
}

// BigInt preserves the capture clock's nanoseconds. Unzoned dates use the
// calendar clock and only compare to other unzoned dates.
function capture(row: PhotoBrowseRow): { nanos: bigint; zoned: boolean } | undefined {
  if (row.kind !== "photo" || !["second", "fraction"].includes(row.capture_time_precision ?? ""))
    return;
  const match =
    /^(\d{4})-(\d{2})-(\d{2})T(\d{1,2}):(\d{2}):(\d{2})(?:[.,](\d+))?(Z|[+-]\d{2}:\d{2})?$/.exec(
      row.capture_time ?? "",
    );
  if (!match) return;
  const [, year, month, day, hour, minute, second, fraction, zone] = match;
  const calendar = new Date(0);
  calendar.setUTCFullYear(Number(year), Number(month) - 1, Number(day));
  calendar.setUTCHours(Number(hour), Number(minute), Number(second), 0);
  if (
    calendar.getUTCFullYear() !== Number(year) ||
    calendar.getUTCMonth() !== Number(month) - 1 ||
    calendar.getUTCDate() !== Number(day) ||
    Number(hour) > 23 ||
    Number(minute) > 59 ||
    Number(second) > 59
  )
    return;
  let seconds = BigInt(calendar.getTime() / 1000);
  if (zone && zone !== "Z") {
    const hours = Number(zone.slice(1, 3)),
      minutes = Number(zone.slice(4, 6));
    if (hours > 24 || minutes > 60) return;
    seconds -= BigInt((hours * 3600 + minutes * 60) * (zone[0] === "+" ? 1 : -1));
  }
  return {
    nanos: seconds * BigInt(1_000_000_000) + BigInt((fraction ?? "").slice(0, 9).padEnd(9, "0")),
    zoned: !!zone,
  };
}

export function groupBursts(rows: readonly PhotoBrowseRow[], gap: number): PhotoStack[] {
  const clocks = rows.map(capture);
  const groups: PhotoStack[] = [];
  for (let start = 0; start < rows.length; ) {
    let end = start + 1;
    while (end < rows.length && end - start < 500) {
      const previous = rows[end - 1]!,
        next = rows[end]!,
        a = clocks[end - 1],
        b = clocks[end];
      if (
        !previous.source_folder_key ||
        previous.source_folder_key !== next.source_folder_key ||
        !a ||
        !b ||
        a.zoned !== b.zoned
      )
        break;
      const delta = a.nanos - b.nanos;
      if ((delta < BigInt(0) ? -delta : delta) > BigInt(gap) * BigInt(1_000_000_000)) break;
      end++;
    }
    const members = rows.slice(start, end),
      picks = members.filter((row) => row.picked);
    groups.push({
      asset_ids: members.map((row) => row.asset_id),
      representative_id: (picks.length === 1 ? picks[0]! : members[0]!).asset_id,
    });
    start = end;
  }
  return groups;
}
