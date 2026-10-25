import { afterEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { node, version } from "./version-test-fixtures.js";
import InspectorHistory from "./InspectorHistory.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

it("merges recent versions and imports, copies source paths, and opens complete records", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  const versions = vi.spyOn(api, "listContentVersions").mockResolvedValue({
    items: [
      version("replace", 6, "content_replace", "b"),
      version("revert", 4, "content_revert", "c"),
      version("create", 1, "content_create", "a"),
    ],
    total: 3,
    limit: 5,
    offset: 0,
  });
  const provenance = vi.spyOn(api, "listNodeProvenance").mockResolvedValue({
    node,
    limit: 5,
    offset: 0,
    total: 4,
    items: [
      {
        identity: "cli",
        ingest_id: "import-1",
        node_id: node.id,
        ingest_started_at: "2026-07-26T12:00:00Z",
        source_kind: "cli",
        source_description: "Documents",
        original_path: "reports/quarterly-report.txt",
        active: true,
      },
      {
        identity: "watch",
        ingest_id: "import-2",
        node_id: node.id,
        ingest_started_at: "2026-07-24T12:00:00Z",
        source_kind: "watch",
        source_description: "Inbox",
        original_path: "inbox/report.txt",
        active: true,
      },
      {
        identity: "web",
        ingest_id: "import-3",
        node_id: node.id,
        ingest_started_at: "2026-07-23T12:00:00Z",
        source_kind: "web",
        source_description: "Upload",
        original_path: "report.txt",
        active: true,
      },
      {
        identity: "old",
        ingest_id: "import-4",
        node_id: node.id,
        ingest_started_at: "2026-07-22T12:00:00Z",
        source_kind: "filesystem",
        source_description: "Archive",
        original_path: "archive/report.txt",
        active: true,
      },
    ],
  });
  const onversions = vi.fn(),
    onprovenance = vi.fn();
  render(InspectorHistory, {
    session: "test-session",
    node,
    onversions,
    onprovenance,
    onauthfailure: vi.fn(),
  });
  await screen.findByText("Added from command line");
  const entries = within(screen.getByRole("list")).getAllByRole("listitem");
  expect(entries).toHaveLength(5);
  expect(
    entries.map((entry) =>
      entry.querySelector(".entry-heading")?.textContent?.replace(/\s+/g, " ").trim(),
    ),
  ).toEqual([
    "Jul 27 · New version",
    "Jul 26 · Added from command line",
    "Jul 25 · Reverted version",
    "Jul 24 · Added from watched folder",
    "Jul 23 · Uploaded in browser",
  ]);
  expect(versions).toHaveBeenCalledWith(
    42,
    { limit: 5, offset: 0 },
    { session: "test-session", signal: expect.any(AbortSignal) },
  );
  expect(provenance).toHaveBeenCalledWith(
    42,
    { limit: 5, offset: 0 },
    { session: "test-session", signal: expect.any(AbortSignal) },
  );
  await fireEvent.click(within(entries[1]!).getByRole("button", { name: "Copy source path" }));
  expect(writeText).toHaveBeenCalledWith("reports/quarterly-report.txt");
  expect(screen.queryByRole("button", { name: "Added from command line" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "All versions" }));
  expect(onversions).toHaveBeenCalledOnce();
  // Older imports stay reachable although recent ones already show their source paths.
  await fireEvent.click(screen.getByRole("button", { name: "All imports" }));
  expect(onprovenance).toHaveBeenCalledOnce();
});

it("keeps readable history when one read fails and retries the missing history", async () => {
  vi.spyOn(api, "listContentVersions").mockResolvedValue({
    items: [version("first", 1, "content_create", "a")],
    limit: 5,
    offset: 0,
    total: 1,
  });
  vi.spyOn(api, "listNodeProvenance")
    .mockRejectedValueOnce(new Error("Read failed"))
    .mockResolvedValue({ node, items: [], limit: 5, offset: 0, total: 0 });
  render(InspectorHistory, {
    session: "test-session",
    node,
    onprovenance: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await screen.findByText("Created");
  await fireEvent.click(screen.getByRole("button", { name: "Retry history" }));
  await screen.findByText("Created");
  expect(screen.queryByRole("button", { name: "Retry history" })).toBeNull();
});

it("opens imports even when newer versions fill the recent history", async () => {
  vi.spyOn(api, "listContentVersions").mockResolvedValue({
    items: [6, 5, 4, 3, 2].map((revision) =>
      version(String(revision), revision, "content_replace", "a"),
    ),
    limit: 5,
    offset: 0,
    total: 6,
  });
  vi.spyOn(api, "listNodeProvenance").mockResolvedValue({
    node,
    items: [
      {
        identity: "original",
        ingest_id: "import-1",
        node_id: node.id,
        ingest_started_at: "2026-07-21T12:00:00Z",
        source_kind: "cli",
        source_description: "Documents",
        original_path: "reports/report.txt",
        active: true,
      },
    ],
    limit: 5,
    offset: 0,
    total: 1,
  });
  const onprovenance = vi.fn();
  render(InspectorHistory, { session: "test-session", node, onprovenance, onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "All imports" }));
  expect(onprovenance).toHaveBeenCalledOnce();
});

it("cancels a previous document's history and reports expired authentication", async () => {
  let finish!: (page: api.ContentVersionPage) => void;
  const versions = vi
    .spyOn(api, "listContentVersions")
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    )
    .mockRejectedValueOnce(new APIError("Expired", 401, "unauthorized"));
  vi.spyOn(api, "listNodeProvenance").mockResolvedValue({
    node,
    items: [],
    limit: 5,
    offset: 0,
    total: 0,
  });
  const onauthfailure = vi.fn();
  const props = { session: "test-session", node, onprovenance: vi.fn(), onauthfailure };
  const view = render(InspectorHistory, props);
  await view.rerender({ ...props, node: { ...node, id: 43 } });
  await act(async () => {
    finish({ items: [version("stale", 1, "content_create", "a")], limit: 5, offset: 0, total: 1 });
  });
  expect(versions.mock.calls[0]![2]!.signal!.aborted).toBe(true);
  expect(onauthfailure).toHaveBeenCalledWith(expect.objectContaining({ status: 401 }));
  expect(screen.queryByText("Created")).toBeNull();
});
