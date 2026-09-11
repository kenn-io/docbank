import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";

afterEach(() => {
  cleanup();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  vi.restoreAllMocks();
});

it("opens the exact collection member when its bounded parent page omits it", async () => {
  history.replaceState(null, "", "/#web_session=short-lived&web_upload_secret=proof");
  vi.stubGlobal("ResizeObserver", class {
    observe() {}
    unobserve() {}
    disconnect() {}
  });
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });

  const root = {
    id: 1, name: "", kind: "dir", size: 0, revision: 1,
    created_at: "2026-09-09T12:00:00Z", modified_at: "2026-09-09T12:00:00Z", path: "/",
  };
  const archive = { ...root, id: 7, parent_id: 1, name: "Archive", path: "/Archive" };
  const member = {
    id: 42, parent_id: 7, name: "report.txt", kind: "file", path: "/Archive/report.txt",
    current_version_id: "22222222-2222-4222-8222-222222222222",
    blob_hash: "a".repeat(64), size: 2048, revision: 5,
    created_at: "2026-09-09T14:30:00Z", modified_at: "2026-09-09T14:30:00Z",
  };
  const oldPageOccupant = {
    ...member, id: 9, name: "first.txt", path: "/Archive/first.txt",
    current_version_id: "33333333-3333-4333-8333-333333333333",
    blob_hash: "b".repeat(64),
  };
  const collectionID = "11111111-1111-4111-8111-111111111111";
  const collection = {
    id: collectionID, source_kind: "cli", source_description: "/synthetic/drop",
    started_at: "2026-09-09T14:30:00Z", file_count: 1, total_bytes: 2048,
    label: "Discovery batch", label_revision: 3, label_updated_at: "2026-09-09T14:31:00Z",
  };
  const json = (value: unknown, etag?: string) => new Response(JSON.stringify(value), {
    headers: { "Content-Type": "application/json", ...(etag ? { ETag: etag } : {}) },
  });
  let replaceMemberPath = false;
  let deferMemberRead = false;
  let resolveMemberRead!: (response: Response) => void;
  const deferredMemberRead = new Promise<Response>((resolve) => {
    resolveMemberRead = resolve;
  });
  let parentReads = 0;

  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/path?path=%2F") return json(root);
    if (url === "/api/v1/nodes/1/children?limit=1000&offset=0") {
      return json({ directory: root, items: [], total: 0, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/tags?limit=1000&offset=0") {
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/collections?limit=100&offset=0") {
      return json({ items: [collection], total: 1, limit: 100, offset: 0 });
    }
    if (url === `/api/v1/collections/${collectionID}/members?limit=100&offset=0`) {
      return json({ collection, items: [member], total: 1, limit: 100, offset: 0 });
    }
    if (url === `/api/v1/collections/${collectionID}/label`) {
      return json({ ingest_id: collectionID, label: collection.label, revision: 3, updated_at: collection.label_updated_at }, '"3"');
    }
    if (url === "/api/v1/path?path=%2FArchive%2Freport.txt") {
      if (deferMemberRead) return deferredMemberRead;
      return json(replaceMemberPath ? { ...member, id: 99 } : member);
    }
    if (url === "/api/v1/path?path=%2FArchive") {
      parentReads += 1;
      return json(archive);
    }
    if (url === "/api/v1/nodes/7/children?limit=1000&offset=0") {
      return json({ directory: archive, items: [oldPageOccupant], total: 2, limit: 1000, offset: 0 });
    }
    if (url === "/api/v1/audit/status?node_id=42") return json({ enabled: false, scopes: [] });
    if (url === "/api/v1/nodes/42/tags?limit=1000&offset=0") {
      return json({ items: [], total: 0, limit: 1000, offset: 0 });
    }
    throw new Error(`unexpected request: ${url}`);
  });

  render(App);
  await screen.findByText("This folder is empty");
  await fireEvent.click(screen.getByRole("button", { name: "Import collections" }));
  const drawer = await screen.findByRole("dialog", { name: "Import collections" });
  await fireEvent.click(await within(drawer).findByRole("button", { name: "Browse collection Discovery batch" }));
  await fireEvent.click(await within(drawer).findByRole("button", { name: "Open document /Archive/report.txt" }));

  expect(await screen.findByRole("cell", { name: "report.txt" })).toBeTruthy();
  expect(screen.queryByRole("dialog", { name: "Import collections" })).toBeNull();
  expect(screen.getByRole("heading", { name: "report.txt" })).toBeTruthy();
  expect(screen.queryByRole("heading", { name: "first.txt" })).toBeNull();

  await fireEvent.click(screen.getByRole("button", { name: "Import collections" }));
  const reopened = await screen.findByRole("dialog", { name: "Import collections" });
  await fireEvent.click(await within(reopened).findByRole("button", { name: "Browse collection Discovery batch" }));
  replaceMemberPath = true;
  await fireEvent.click(await within(reopened).findByRole("button", { name: "Open document /Archive/report.txt" }));
  expect((await within(reopened).findByRole("alert")).textContent).toContain("old path now belongs to another document");
  expect(parentReads).toBe(1);

  replaceMemberPath = false;
  deferMemberRead = true;
  await fireEvent.click(within(reopened).getByRole("button", { name: "Open document /Archive/report.txt" }));
  await fireEvent.click(within(reopened).getByRole("button", { name: "Close import collections" }));
  resolveMemberRead(json(member));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "Import collections" })).toBeNull());
  await Promise.resolve();
  expect(parentReads).toBe(1);
});
