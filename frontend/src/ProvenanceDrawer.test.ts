import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import ProvenanceDrawer from "./ProvenanceDrawer.svelte";
import type { Node, ProvenanceFact } from "./generated/docbank.js";

const olderIdentity = "a".repeat(64);
const activeIdentity = "b".repeat(64);

const node: Node = {
  id: 42,
  name: "quarterly-report.txt",
  kind: "file",
  current_version_id: "11111111-1111-4111-8111-111111111111",
  blob_hash: "c".repeat(64),
  size: 384,
  mime_type: "text/plain; charset=utf-8",
  revision: 3,
  created_at: "2026-01-02T03:04:05Z",
  modified_at: "2026-07-24T12:00:00Z",
};

const older: ProvenanceFact = {
  identity: olderIdentity,
  node_id: node.id,
  ingest_id: "22222222-2222-4222-8222-222222222222",
  ingest_started_at: "2026-07-20T10:00:00Z",
  source_kind: "filesystem",
  source_description: "quarterly reports",
  original_path: "drafts/quarterly-report.txt",
  original_mtime: "2026-07-20T09:30:00Z",
  active: false,
};

const active: ProvenanceFact = {
  identity: activeIdentity,
  node_id: node.id,
  ingest_id: "33333333-3333-4333-8333-333333333333",
  ingest_started_at: "2026-07-24T12:00:00Z",
  source_kind: "watch",
  source_description: "approved reports",
  original_path: "approved/quarterly-report.txt",
  original_mtime: "2026-07-24T11:45:00Z",
  supersedes: olderIdentity,
  active: true,
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("provenance drawer", () => {
  it("uses authoritative node state and exposes supersession history", async () => {
    const authoritativeNode = {
      ...node,
      parent_id: 99,
      path: "/Filed/quarterly-report.txt",
      revision: 4,
    };
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          node: authoritativeNode,
          items: [active, older],
          total: 2,
          limit: 1000,
          offset: 0,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    const close = vi.fn();

    render(ProvenanceDrawer, {
      session: "short-lived",
      node,
      path: "/Reports/quarterly-report.txt",
      onclose: close,
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("/Filed/quarterly-report.txt")).toBeTruthy();
    expect(screen.getByText("Added from watched folder")).toBeTruthy();
    expect(screen.getByText(activeIdentity)).toBeTruthy();
    expect(screen.getByText("Replaces")).toBeTruthy();
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/nodes/42/provenance?limit=1000&offset=0");

    expect(screen.getByText("Superseded")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Copy source path" })).toHaveLength(2);
    expect(screen.getByText("Added from folder")).toBeTruthy();
    expect(screen.getAllByText("Import details")).toHaveLength(2);

    await fireEvent.click(screen.getByRole("button", { name: "Close provenance" }));
    expect(close).toHaveBeenCalledOnce();
  });
});

it("offers the import ID for command-line imports", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        node,
        items: [
          {
            ...active,
            source_kind: "cli",
            source_description: '["/imports/report.txt"]',
            original_path: "/imports/report.txt",
          },
        ],
        total: 1,
        limit: 1000,
        offset: 0,
      }),
    ),
  );
  render(ProvenanceDrawer, {
    session: "session",
    node,
    path: "/report.txt",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  expect((await screen.findByRole("button", { name: "Copy import ID" })).textContent).toContain(
    active.ingest_id,
  );
});

it.each([
  { bytes: 'W/"\\xff\xff"', display: 'W/"\\\\xff\\xff"' },
  { bytes: null, display: null },
])("shows URL fetch evidence with optional headers: $display", async (headers) => {
  const urlFetch = {
    version: 1,
    requested_url: "https://example.com/download?id=7",
    final_url: "https://example.com/filing.pdf",
    fetched_at: "2026-10-01T12:34:56Z",
    ...(headers.bytes
      ? {
          etag: btoa(headers.bytes!),
          last_modified: btoa("server supplied text"),
        }
      : {}),
  };
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        node,
        items: [
          {
            ...active,
            source_kind: "url",
            original_path: "https://example.com/download?id=7",
            source_description: JSON.stringify(urlFetch),
            url_fetch: urlFetch,
          },
        ],
        total: 1,
        limit: 1000,
        offset: 0,
      }),
    ),
  );
  render(ProvenanceDrawer, {
    session: "session",
    node,
    path: "/filing.pdf",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  expect(await screen.findByText("Added from URL")).toBeTruthy();
  await fireEvent.click(screen.getByText("Import details"));
  expect(screen.queryByText("Source")).toBeNull();
  expect(screen.getByRole("button", { name: "Copy final URL" }).textContent).toContain(
    "https://example.com/filing.pdf",
  );
  expect(
    screen
      .getByText("Added from URL")
      .closest("li")
      ?.querySelector("time")
      ?.getAttribute("datetime"),
  ).toBe("2026-10-01T12:34:56Z");
  if (headers.bytes) {
    expect(screen.getByText(headers.display!)).toBeTruthy();
    expect(screen.getByText("server supplied text")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Copy ETag" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Copy Last-Modified" })).toBeNull();
  } else {
    expect(screen.queryByText("ETag")).toBeNull();
    expect(screen.queryByText("Last-Modified")).toBeNull();
  }
});
