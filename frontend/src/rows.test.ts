import { describe, expect, it } from "vite-plus/test";
import type { Node } from "./generated/docbank.js";
import { orderRows, reconcileSearchView } from "./rows.js";

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

    expect(orderRows(rows, "relevance", "asc").map((row) => row.node.id)).toEqual([3, 1, 2]);
  });

  it("sorts by the type shown, keeping names A to Z inside a type and folders first", () => {
    const typed = (id: number, name: string, mime: string, kind: Node["kind"] = "file") => ({
      node: { ...node(id, name, kind), mime_type: mime },
      path: `/${name}`,
    });
    const rows = [
      typed(1, "b.csv", "text/csv"),
      typed(2, "a.pdf", "application/pdf"),
      typed(3, "a.csv", "text/csv"),
      typed(4, "dir", "", "dir"),
    ];

    expect(orderRows(rows, "type", "asc").map((row) => row.node.id)).toEqual([4, 3, 1, 2]);
    expect(orderRows(rows, "type", "desc").map((row) => row.node.id)).toEqual([4, 2, 3, 1]);
  });

  it("sorts names by basename and paths by folder across search results", () => {
    const rows = [
      { node: node(1, "a.txt"), path: "/zeta/a.txt" },
      { node: node(2, "z.txt"), path: "/alpha/z.txt" },
    ];

    expect(orderRows(rows, "name", "asc").map((row) => row.node.id)).toEqual([1, 2]);
    expect(orderRows(rows, "path", "asc").map((row) => row.node.id)).toEqual([2, 1]);
    expect(orderRows(rows, "name", "desc").map((row) => row.node.id)).toEqual([2, 1]);
    expect(orderRows(rows, "path", "desc").map((row) => row.node.id)).toEqual([1, 2]);
  });

  it("preserves an explicit sort and selection when refreshing the same search", () => {
    const rows = [
      { node: node(2, "second.txt"), path: "/second.txt" },
      { node: node(1, "first.txt"), path: "/first.txt" },
    ];

    const view = reconcileSearchView(rows, "report", "report", "name", "desc", 1);

    expect(view.sortField).toBe("name");
    expect(view.sortDirection).toBe("desc");
    expect(view.selectedID).toBe(1);
  });

  it("resets a new search to relevance and its first ranked result", () => {
    const rows = [
      { node: node(2, "second.txt"), path: "/second.txt" },
      { node: node(1, "first.txt"), path: "/first.txt" },
    ];

    const view = reconcileSearchView(rows, "new", "old", "name", "asc", 1);

    expect(view.sortField).toBe("relevance");
    expect(view.sortDirection).toBe("asc");
    expect(view.selectedID).toBe(2);
  });
});
