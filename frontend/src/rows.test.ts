import { describe, expect, it } from "vitest";
import type { Node } from "./api.js";
import { orderRows } from "./rows.js";

function node(id: number, name: string, kind: Node["kind"] = "file"): Node {
  return {
    id,
    name,
    kind,
    path: `/${name}`,
    revision: 1,
    created_at: "2026-01-01T00:00:00Z",
    modified_at: "2026-01-01T00:00:00Z",
    current_version_id: "",
    blob_hash: "",
    size: id,
    mime_type: "",
  };
}

describe("orderRows", () => {
  it("preserves API relevance order for search results", () => {
    const rows = [
      { node: node(3, "third.txt"), path: "/third.txt" },
      { node: node(1, "first", "dir"), path: "/first" },
      { node: node(2, "second.txt"), path: "/second.txt" },
    ];

    expect(orderRows(rows, "relevance", "asc", true).map((row) => row.node.id)).toEqual([3, 1, 2]);
  });

  it("sorts search documents by the full path displayed in the table", () => {
    const rows = [
      { node: node(1, "a.txt"), path: "/zeta/a.txt" },
      { node: node(2, "z.txt"), path: "/alpha/z.txt" },
    ];

    expect(orderRows(rows, "name", "asc", true).map((row) => row.node.id)).toEqual([2, 1]);
  });
});
