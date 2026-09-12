import type { Node } from "./api.js";

export type VisibleCSVRow = {
  node: Node;
  path: string;
  match?: "name" | "content";
};

const columns = [
  "node_id",
  "content_version_id",
  "revision",
  "path",
  "name",
  "mime_type",
  "size_bytes",
  "created_at",
  "modified_at",
  "sha256",
  "md5",
  "match",
] as const;

const spreadsheetFormula = /^[\p{White_Space}\p{Cc}]*[=+\-@]/u;

function textCell(value: string | undefined): string {
  const safe = value && spreadsheetFormula.test(value) ? `'${value}` : (value ?? "");
  return `"${safe.replaceAll('"', '""')}"`;
}

function record(cells: readonly (string | number | undefined)[]): string {
  return cells
    .map((cell) => (typeof cell === "number" ? String(cell) : textCell(cell)))
    .join(",");
}

export function selectedVisibleCSVRows(
  displayedRows: readonly VisibleCSVRow[],
  selectedIDs: ReadonlySet<number>,
): VisibleCSVRow[] {
  return displayedRows.filter(
    (row) => row.node.kind === "file" && selectedIDs.has(row.node.id),
  );
}

export function buildVisiblePageCSV(rows: readonly VisibleCSVRow[]): string {
  const records = [
    record(columns),
    ...rows.map(({ node, path, match }) =>
      record([
        node.id,
        node.current_version_id,
        node.revision,
        path,
        node.name,
        node.mime_type,
        node.size,
        node.created_at,
        node.modified_at,
        node.blob_hash,
        node.md5,
        match,
      ]),
    ),
  ];
  return `${records.join("\r\n")}\r\n`;
}

export function visiblePageCSVFilename(now = new Date()): string {
  return `docbank-visible-page-${now.toISOString().slice(0, 10)}.csv`;
}

export function downloadVisiblePageCSV(
  rows: readonly VisibleCSVRow[],
  now = new Date(),
): void {
  const url = URL.createObjectURL(
    new Blob(["\uFEFF", buildVisiblePageCSV(rows)], {
      type: "text/csv;charset=utf-8",
    }),
  );
  try {
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = visiblePageCSVFilename(now);
    anchor.click();
  } finally {
    URL.revokeObjectURL(url);
  }
}
