import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import App from "./App.svelte";

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("keeps the exact page viewer and download when the selected live row is clicked again", async () => {
  history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  const root = {
    id: 1,
    name: "",
    kind: "dir",
    size: 0,
    revision: 1,
    path: "/",
    created_at: "2026-09-11T12:00:00Z",
    modified_at: "2026-09-11T12:00:00Z",
  };
  const node = {
    ...root,
    id: 2,
    parent_id: 1,
    kind: "file",
    name: "synthetic.pdf",
    path: "/synthetic.pdf",
    size: 100,
    mime_type: "application/pdf",
    current_version_id: "00000000-0000-4000-8000-000000000001",
    blob_hash: "a".repeat(64),
  };
  const json = (v: unknown) => new Response(JSON.stringify(v));
  let inventories = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url.includes("/nodes/1/children?"))
      return json({ directory: root, items: [node], total: 1, limit: 1000, offset: 0 });
    if (url === "/api/v1/nodes/2") return json(node);
    if (url.endsWith("/pages/inventory")) {
      inventories++;
      return json({
        runtime_available: false,
        inventory: {
          source: { version_id: node.current_version_id, sha256: node.blob_hash, size: 100 },
          page_count: 0,
          frames: [],
          recipes: [],
          images: [],
        },
      });
    }
    if (url.includes("/audit/status"))
      return json({
        enabled: false,
        vault_id: "synthetic",
        operation_sequence_high_water: 0,
        allocation_entry_count: 0,
        scopes: [],
        membership: {
          node_id: 2,
          path: node.path,
          trashed: false,
          protected: false,
          scope_ids: [],
          baseline_digests: [],
        },
      });
    if (url.includes("/tags?") || url.includes("/saved-queries?"))
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    throw new Error(`Unexpected synthetic request: ${url}`);
  });
  render(App);
  await screen.findByRole("region", { name: "Pages of synthetic.pdf" });
  await screen.findByText(/No page inventory/);
  await fireEvent.click(screen.getByRole("cell", { name: "synthetic.pdf" }));
  expect(screen.getByRole("region", { name: "Pages of synthetic.pdf" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Download verified original" })).toBeTruthy();
  expect(inventories).toBe(1);
});
