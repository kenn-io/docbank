import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/svelte";
import ProvenanceDrawer from "./ProvenanceDrawer.svelte";
import type { Node, ProvenanceFact } from "./api.js";

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
    expect(screen.getByText("Active origin")).toBeTruthy();
    expect(screen.getByText(activeIdentity)).toBeTruthy();
    expect(screen.getByText("Supersedes")).toBeTruthy();
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/api/v1/nodes/42/provenance?limit=1000&offset=0",
    );

    const olderHeading = screen.getByText("quarterly reports");
    const olderButton = olderHeading.closest("button");
    expect(olderButton).toBeTruthy();
    await fireEvent.click(olderButton!);

    expect(screen.getByText("Superseded origin")).toBeTruthy();
    expect(screen.getByText(olderIdentity)).toBeTruthy();
    expect(screen.queryByText("Supersedes")).toBeNull();

    await fireEvent.click(
      screen.getByRole("button", { name: "Close provenance" }),
    );
    expect(close).toHaveBeenCalledOnce();
  });
});
