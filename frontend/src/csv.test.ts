import { describe, expect, it } from "vitest";
import { parseCSV } from "../test-support/csv.js";
import type { Node } from "./api.js";
import {
  buildVisiblePageCSV,
  selectedVisibleCSVRows,
  type VisibleCSVRow,
} from "./csv.js";

const headers = [
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
];

function node(id: number, overrides: Partial<Node> = {}): Node {
  return {
    id,
    name: `document-${id}.txt`,
    kind: "file",
    current_version_id: `version-${id}`,
    blob_hash: `sha256-${id}`,
    size: id * 10,
    mime_type: "text/plain",
    revision: id + 20,
    created_at: "2026-09-01T10:11:12Z",
    modified_at: "2026-09-02T13:14:15Z",
    ...overrides,
  };
}

describe("visible-page CSV", () => {
  it("accepts one file-leading BOM without changing later cell content", () => {
    expect(parseCSV('\uFEFF"heading"\r\n"kept\uFEFFvalue"\r\n')).toEqual([
      ["heading"],
      ["kept\uFEFFvalue"],
    ]);
  });

  it("reports an unterminated quoted field", () => {
    expect(() => parseCSV('"unterminated')).toThrowError(
      "CSV has an unterminated quoted field",
    );
  });

  it("reports data after a closing quote", () => {
    expect(() => parseCSV('"closed"x\r\n')).toThrowError(
      "CSV has an unexpected character after a closing quote",
    );
  });

  it("exports stable columns with exact visible node authority and blank absent metadata", () => {
    const rows: VisibleCSVRow[] = [
      {
        node: node(41, {
          current_version_id: "76d80170-1cd3-4f35-9580-e6bfc1c4f9e1",
          blob_hash: "a".repeat(64),
          md5: "f6fdffe48c908deb0f4c3bd36c032e72",
          revision: 117,
          size: 987_654,
        }),
        path: "/Cases/alpha.txt",
        match: "content",
      },
      {
        node: node(42, {
          current_version_id: undefined,
          blob_hash: undefined,
          mime_type: undefined,
        }),
        path: "/Cases/document-42.txt",
      },
    ];

    const parsed = parseCSV(buildVisiblePageCSV(rows));

    expect(parsed).toHaveLength(3);
    expect(parsed.every((record) => record.length === headers.length)).toBe(true);
    expect(parsed[0]).toEqual(headers);
    expect(parsed[1]).toEqual([
      "41",
      "76d80170-1cd3-4f35-9580-e6bfc1c4f9e1",
      "117",
      "/Cases/alpha.txt",
      "document-41.txt",
      "text/plain",
      "987654",
      "2026-09-01T10:11:12Z",
      "2026-09-02T13:14:15Z",
      "a".repeat(64),
      "f6fdffe48c908deb0f4c3bd36c032e72",
      "content",
    ]);
    expect(parsed[2]).toEqual([
      "42",
      "",
      "62",
      "/Cases/document-42.txt",
      "document-42.txt",
      "",
      "420",
      "2026-09-01T10:11:12Z",
      "2026-09-02T13:14:15Z",
      "",
      "",
      "",
    ]);
  });

  it("preserves RFC4180 comma, quote, newline, and Unicode values", () => {
    const csv = buildVisiblePageCSV([
      {
        node: node(7, { name: 'Budget, "final"\n日本語.txt' }),
        path: '/Reports, 2026/Budget, "final"\n日本語.txt',
        match: "name",
      },
    ]);

    expect(csv.endsWith("\r\n")).toBe(true);
    expect(csv).toContain('"Budget, ""final""\n日本語.txt"');
    const parsed = parseCSV(csv);
    expect(parsed[1]?.[3]).toBe('/Reports, 2026/Budget, "final"\n日本語.txt');
    expect(parsed[1]?.[4]).toBe('Budget, "final"\n日本語.txt');
  });

  it.each(["=SUM(A1:A2)", "+cmd", "-2+3", "@lookup", " \t=hidden", "\u0001@hidden"])(
    "neutralizes formula-leading textual cell %j",
    (hostile) => {
      const parsed = parseCSV(
        buildVisiblePageCSV([
          {
            node: node(9, { name: hostile }),
            path: hostile,
            match: "name",
          },
        ]),
      );

      expect(parsed[1]?.[3]).toBe(`'${hostile}`);
      expect(parsed[1]?.[4]).toBe(`'${hostile}`);
      expect(parsed[1]?.[6]).toBe("90");
    },
  );

  it("keeps selected visible files in displayed order", () => {
    const displayed = [
      { node: node(3), path: "/zeta.txt", match: "name" as const },
      { node: node(1), path: "/alpha.txt", match: "content" as const },
      {
        node: node(2, { kind: "dir" }),
        path: "/folder",
      },
      { node: node(4), path: "/unselected.txt" },
    ];

    expect(
      selectedVisibleCSVRows(displayed, new Set([1, 99, 3, 2])).map(
        (row) => row.node.id,
      ),
    ).toEqual([3, 1]);
  });

  it("serializes 1000 synthetic visible rows", () => {
    const rows = Array.from({ length: 1_000 }, (_, index) => ({
      node: node(index + 1),
      path: `/Synthetic/document-${index + 1}.txt`,
      match: index % 2 === 0 ? ("name" as const) : ("content" as const),
    }));

    const started = performance.now();
    const parsed = parseCSV(buildVisiblePageCSV(rows));
    const elapsed = performance.now() - started;
    console.info(`CSV1000 elapsed_ms=${elapsed.toFixed(3)}`);

    expect(parsed).toHaveLength(1_001);
    expect(parsed.every((record) => record.length === headers.length)).toBe(true);
    expect(parsed[1]?.[0]).toBe("1");
    expect(parsed[1_000]?.[0]).toBe("1000");
    expect(parsed[1_000]?.[11]).toBe("content");
  });
});
