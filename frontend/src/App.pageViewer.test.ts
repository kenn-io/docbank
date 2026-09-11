import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import App from "./App.svelte";

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it.each(["none", "tags", "audit"])("reclick keeps the viewer and recovers a %s load failure", async failure => {
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
  let tagAttempts = 0, auditAttempts = 0;
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
    if (url.includes("/nodes/2/tags?")) {
      if (++tagAttempts === 1 && failure === "tags") throw new TypeError("Tag request lost");
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    }
    if (url.includes("/audit/status")) {
      if (url.includes("node_id=2") && ++auditAttempts === 1 && failure === "audit") throw new TypeError("Audit request lost");
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
    }
    if (url.includes("/tags?") || url.includes("/saved-queries?"))
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    throw new Error(`Unexpected synthetic request: ${url}`);
  });
  render(App);
  if (failure === "tags") await screen.findByText("Refresh the current view before using document actions.");
  else await screen.findByText(/No page inventory/);
  if (failure === "audit") await screen.findByText("Audit request lost");
  await fireEvent.click(screen.getByRole("cell", { name: "synthetic.pdf" }));
  await screen.findByText(/No page inventory/);
  await waitFor(() => {
    expect(screen.queryByText("Tag request lost")).toBeNull();
    expect(screen.queryByText("Audit request lost")).toBeNull();
    expect(screen.queryByText("Refresh the current view before using document actions.")).toBeNull();
  });
  expect(screen.getByRole("region", { name: "Pages of synthetic.pdf" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Download verified original" })).toBeTruthy();
  if (failure !== "tags") expect(inventories).toBe(1);
  if (failure === "tags") expect(tagAttempts).toBe(2);
  if (failure === "audit") expect(auditAttempts).toBe(2);
});
