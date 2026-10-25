import { afterEach, expect, it, vi } from "vite-plus/test";
import {
  moveToTrash,
  parentPath,
  restoredMessage,
  trashedMessage,
  undoTrash,
} from "./trashActions.js";
import type { Node } from "./generated/docbank.js";

afterEach(() => vi.restoreAllMocks());

const node = (overrides: Partial<Node> = {}): Node => ({
  id: 7,
  name: "clients",
  kind: "dir",
  revision: 1,
  size: 0,
  created_at: "2026-01-01T00:00:00Z",
  modified_at: "2026-01-01T00:00:00Z",
  ...overrides,
});

const json = (value: unknown) =>
  new Response(JSON.stringify(value), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

it("names what moved and how many items a folder held", () => {
  expect(trashedMessage(node(), 24)).toBe("Moved “clients” (24 items) to trash");
  expect(trashedMessage(node(), 1)).toBe("Moved “clients” (1 item) to trash");
  expect(trashedMessage(node({ name: "a.txt" }))).toBe("Moved “a.txt” to trash");
});

it("says where a restore landed, and which folder was gone", () => {
  const restored = { path: "/archive/clients" };
  expect(restoredMessage(node(), restored, undefined)).toBe("Restored to /archive");
  expect(restoredMessage(node(), restored, "/archive")).toBe("Restored to /archive");
  expect(restoredMessage(node(), { path: "/clients" }, "/old/place")).toBe(
    "Restored to / — /old/place no longer exists",
  );
  expect(restoredMessage(node({ photo_asset_id: "p", photo_file_count: 3 }), restored, "/x")).toBe(
    "Restored 3 files",
  );
  expect(parentPath("/a")).toBe("/");
  expect(parentPath("/a/b.txt")).toBe("/a");
});

it("counts a folder's items, trashes it with its revision, and undoes through restore", async () => {
  const calls: Array<[string, string | null]> = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    calls.push([url, new Headers(init?.headers).get("If-Match")]);
    if (url.includes("/children")) return json({ directory: node(), items: [], total: 24 });
    if (url.endsWith("/trash"))
      return json(node({ revision: 2, trashed_at: "2026-01-02T00:00:00Z", path: "/old/clients" }));
    return json(node({ revision: 3, path: "/clients" }));
  });

  const moved = await moveToTrash("s", node(), "/old/clients");
  expect(moved.message).toBe("Moved “clients” (24 items) to trash");

  const undone = await undoTrash("s", moved.receipt);
  expect(undone.notice).toBe("Restored to / — /old no longer exists");
  expect(calls.map(([url]) => url.split("/").slice(-1)[0]?.split("?")[0])).toEqual([
    "children",
    "trash",
    "restore",
  ]);
  expect(calls[1]![1]).toBe("1");
  expect(calls[2]![1]).toBe("2");
});
